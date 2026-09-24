package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type wsPoolConn struct {
	ws      *websocket.Conn
	created time.Time
	dcID    int
	isMedia bool
	domain  string
}

type wsPool struct {
	mu         sync.Mutex
	conns      []*wsPoolConn
	maxAge     time.Duration
	checkEvery time.Duration
	poolSize   int
	cfMgr      *cfProxyManager
	dcIPs      map[int]string
	ctx        context.Context
	cancel     context.CancelFunc
}

var telegramDCIPs = map[int]string{
	1:   "149.154.175.50",
	2:   "149.154.167.51",
	3:   "149.154.175.100",
	4:   "149.154.167.91",
	5:   "149.154.171.5",
	203: "91.105.192.100",
}

func newWsPool(cfMgr *cfProxyManager) *wsPool {
	ctx, cancel := context.WithCancel(context.Background())

	pool := &wsPool{
		conns:      make([]*wsPoolConn, 0, cfProxyPoolSize*5),
		maxAge:     cfProxyMaxAge,
		checkEvery: 5 * time.Second,
		poolSize:   cfProxyPoolSize,
		cfMgr:      cfMgr,
		dcIPs:      telegramDCIPs,
		ctx:        ctx,
		cancel:     cancel,
	}

	return pool
}

func (p *wsPool) get(dcID int, isMedia bool) *wsPoolConn {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()

	for i := len(p.conns) - 1; i >= 0; i-- {
		conn := p.conns[i]
		if conn.dcID != dcID || conn.isMedia != isMedia {
			continue
		}

		if now.Sub(conn.created) > p.maxAge {
			p.conns = append(p.conns[:i], p.conns[i+1:]...)
			conn.ws.Close()
			continue
		}

		p.conns = append(p.conns[:i], p.conns[i+1:]...)
		return conn
	}

	return nil
}

func (p *wsPool) put(conn *wsPoolConn) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if time.Since(conn.created) > p.maxAge {
		conn.ws.Close()
		return
	}

	p.conns = append(p.conns, conn)

	if len(p.conns) > p.poolSize*5 {
		oldest := p.conns[0]
		p.conns = p.conns[1:]
		oldest.ws.Close()
	}
}

func (p *wsPool) connect(dcID int, isMedia bool) (*wsPoolConn, error) {
	dcIP, ok := p.dcIPs[dcID]
	if !ok {
		return nil, fmt.Errorf("unknown DC ID: %d", dcID)
	}

	domain := p.cfMgr.getRandomDomain()
	if domain == "" {
		return nil, fmt.Errorf("no CF proxy domains available")
	}

	var sni string
	if isMedia {
		sni = fmt.Sprintf("kws%d-1.%s", dcID, domain)
	} else {
		sni = fmt.Sprintf("kws%d.%s", dcID, domain)
	}

	dialer := websocket.Dialer{
		TLSClientConfig: &tls.Config{
			ServerName:         sni,
			InsecureSkipVerify: false,
		},
		HandshakeTimeout: 10 * time.Second,
		NetDial: func(network, addr string) (net.Conn, error) {
			conn, err := net.DialTimeout("tcp", net.JoinHostPort(dcIP, "443"), 10*time.Second)
			if err != nil {
				return nil, err
			}
			if tcpConn, ok := conn.(*net.TCPConn); ok {
				tcpConn.SetNoDelay(true)
			}
			return conn, nil
		},
	}

	wsURL := fmt.Sprintf("wss://%s/apiws", sni)

	ws, _, err := dialer.Dial(wsURL, http.Header{})
	if err != nil {
		return nil, fmt.Errorf("WebSocket dial failed: %w", err)
	}

	conn := &wsPoolConn{
		ws:      ws,
		created: time.Now(),
		dcID:    dcID,
		isMedia: isMedia,
		domain:  domain,
	}

	return conn, nil
}

func (p *wsPool) rotationLoop() {
	ticker := time.NewTicker(p.checkEvery)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.mu.Lock()
			now := time.Now()
			alive := make([]*wsPoolConn, 0, len(p.conns))

			for _, conn := range p.conns {
				if now.Sub(conn.created) <= p.maxAge {
					alive = append(alive, conn)
				} else {
					conn.ws.Close()
				}
			}

			p.conns = alive
			p.mu.Unlock()
		}
	}
}

func (p *wsPool) warmup() {
	for dcID := 1; dcID <= 5; dcID++ {
		for _, isMedia := range []bool{false, true} {
			conn, err := p.connect(dcID, isMedia)
			if err != nil {
				if *verbose {
					log.Printf("[wspool] warmup DC%d%s failed: %v", dcID, mediaTag(isMedia), err)
				}
				continue
			}
			p.put(conn)
			log.Printf("[wspool] warmup DC%d%s ready", dcID, mediaTag(isMedia))
		}
	}
}

func mediaTag(isMedia bool) string {
	if isMedia {
		return "m"
	}
	return ""
}
