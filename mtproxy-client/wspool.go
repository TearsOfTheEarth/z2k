package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type wsPoolConn struct {
	ws      *websocket.Conn
	created time.Time
	dcID    int
	isMedia bool
	dstIP   string
	domain  string
}

type wsPool struct {
	mu         sync.Mutex
	conns      []*wsPoolConn
	maxAge     time.Duration
	checkEvery time.Duration
	poolSize   int
	cfMgr      *cfProxyManager
	ctx        context.Context
	cancel     context.CancelFunc
}

func newWsPool(cfMgr *cfProxyManager) *wsPool {
	ctx, cancel := context.WithCancel(context.Background())

	pool := &wsPool{
		conns:      make([]*wsPoolConn, 0, cfProxyPoolSize*5),
		maxAge:     cfProxyMaxAge,
		checkEvery: 5 * time.Second,
		poolSize:   cfProxyPoolSize,
		cfMgr:      cfMgr,
		ctx:        ctx,
		cancel:     cancel,
	}

	return pool
}

func (p *wsPool) get(dcID int, isMedia bool, dstIP string) *wsPoolConn {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()

	for i := len(p.conns) - 1; i >= 0; i-- {
		conn := p.conns[i]
		if conn.dcID != dcID || conn.isMedia != isMedia || conn.dstIP != dstIP {
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

func (p *wsPool) connect(dcID int, isMedia bool, dstIP string) (*wsPoolConn, error) {
	domain := p.cfMgr.getRandomDomain()
	if domain == "" {
		return nil, fmt.Errorf("no CF proxy domains available")
	}

	// Формируем URL для CF Worker: wss://domain/apiws?dst=IP&dc=DC_ID
	params := url.Values{}
	params.Set("dst", dstIP)
	params.Set("dc", fmt.Sprintf("%d", dcID))

	wsURL := fmt.Sprintf("wss://%s/apiws?%s", domain, params.Encode())

	if *verbose {
		log.Printf("[wspool] connecting to CF Worker: %s", wsURL)
	}

	dialer := websocket.Dialer{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: false,
		},
		HandshakeTimeout: 10 * time.Second,
		NetDial: func(network, addr string) (net.Conn, error) {
			// Подключаемся к IP CF proxy домена (не к Telegram DC!)
			conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
			if err != nil {
				return nil, err
			}
			if tcpConn, ok := conn.(*net.TCPConn); ok {
				tcpConn.SetNoDelay(true)
			}
			return conn, nil
		},
	}

	ws, _, err := dialer.Dial(wsURL, http.Header{})
	if err != nil {
		return nil, fmt.Errorf("WebSocket dial to %s failed: %w", domain, err)
	}

	conn := &wsPoolConn{
		ws:      ws,
		created: time.Now(),
		dcID:    dcID,
		isMedia: isMedia,
		dstIP:   dstIP,
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

func mediaTag(isMedia bool) string {
	if isMedia {
		return "m"
	}
	return ""
}
