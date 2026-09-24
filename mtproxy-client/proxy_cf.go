package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

var cfConnSemaphore chan struct{}

// Маппинг IP Telegram DC → DC ID
var dcIPToID = map[string]int{
	"149.154.175.50":  1,
	"149.154.167.51":  2,
	"149.154.175.100": 3,
	"149.154.167.91":  4,
	"149.154.171.5":   5,
	"91.105.192.100":  203,
	// Дополнительные IP (могут меняться)
	"149.154.175.53":  1,
	"149.154.167.53":  2,
	"149.154.175.103": 3,
	"149.154.167.93":  4,
	"149.154.171.7":   5,
}

func runCfProxy() error {
	cfConnSemaphore = make(chan struct{}, *maxConns)

	addrs := listenAddrs.addrs()
	lns := make([]net.Listener, 0, len(addrs))
	for _, a := range addrs {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			for _, o := range lns {
				o.Close()
			}
			return fmt.Errorf("listen %s: %w", a, err)
		}
		lns = append(lns, ln)
	}

	log.Printf("[cfproxy] listening on %s", strings.Join(addrs, " "))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfMgr := newCfProxyManager()
	go cfMgr.refreshLoop()

	wsPool := newWsPool(cfMgr)
	go wsPool.rotationLoop()

	var wg sync.WaitGroup
	for _, ln := range lns {
		wg.Add(1)
		go func(ln net.Listener) {
			defer wg.Done()
			cfAcceptLoop(ctx, ln, cfMgr, wsPool)
		}(ln)
	}

	<-ctx.Done()
	log.Println("[cfproxy] shutting down...")
	for _, ln := range lns {
		ln.Close()
	}
	cfMgr.cancel()
	wsPool.cancel()
	wg.Wait()
	return nil
}

func cfAcceptLoop(ctx context.Context, ln net.Listener, cfMgr *cfProxyManager, wsPool *wsPool) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				log.Printf("[cfproxy] accept error: %v", err)
				time.Sleep(200 * time.Millisecond)
				continue
			}
		}

		tcpConn, ok := conn.(*net.TCPConn)
		if !ok {
			conn.Close()
			continue
		}

		select {
		case cfConnSemaphore <- struct{}{}:
			go cfHandleConn(tcpConn, cfMgr, wsPool)
		default:
			if *verbose {
				log.Printf("[cfproxy] max connections reached, rejecting %s", conn.RemoteAddr())
			}
			conn.Close()
		}
	}
}

func cfHandleConn(clientConn *net.TCPConn, cfMgr *cfProxyManager, wsPool *wsPool) {
	defer func() { <-cfConnSemaphore }()
	defer clientConn.Close()

	clientConn.SetNoDelay(true)
	clientConn.SetDeadline(time.Now().Add(*connTimeout))

	// Извлекаем оригинальный IP назначения (Telegram DC) через SO_ORIGINAL_DST
	origIP, origPort, err := getOriginalDst(clientConn)
	if err != nil {
		if *verbose {
			log.Printf("[cfproxy] getOriginalDst failed: %v", err)
		}
		return
	}

	// Определяем DC ID по IP
	dcID := ipToDCID(origIP.String())
	if dcID == 0 {
		if *verbose {
			log.Printf("[cfproxy] unknown Telegram DC IP: %s:%d from %s",
				origIP, origPort, clientConn.RemoteAddr())
		}
		return
	}

	// Определяем, media ли это трафик (DC 2 и 4 обычно media)
	isMedia := dcID == 2 || dcID == 4

	if *verbose {
		log.Printf("[cfproxy] %s → %s:%d (DC%d%s)",
			clientConn.RemoteAddr(), origIP, origPort, dcID, mediaTag(isMedia))
	}

	// Читаем init пакет (64 байта MTProto obfuscation)
	initBuf := make([]byte, 64)
	n, err := clientConn.Read(initBuf)
	if err != nil {
		if *verbose {
			log.Printf("[cfproxy] read init failed: %v", err)
		}
		return
	}

	// Пробуем получить соединение из пула
	var conn *wsPoolConn
	conn = wsPool.get(dcID, isMedia, origIP.String())
	if conn != nil {
		if *verbose {
			log.Printf("[cfproxy] pool hit DC%d%s via %s", dcID, mediaTag(isMedia), conn.domain)
		}
	}

	// Если в пуле нет - создаём новое
	if conn == nil {
		var err error
		conn, err = wsPool.connect(dcID, isMedia, origIP.String())
		if err != nil {
			log.Printf("[cfproxy] connect to DC%d%s failed: %v", dcID, mediaTag(isMedia), err)
			return
		}
		if *verbose {
			log.Printf("[cfproxy] connected to DC%d%s via %s", dcID, mediaTag(isMedia), conn.domain)
		}
	}

	defer func() {
		wsPool.put(conn)
	}()

	// Отправляем init пакет
	if err := conn.ws.WriteMessage(websocket.BinaryMessage, initBuf[:n]); err != nil {
		log.Printf("[cfproxy] write init failed: %v", err)
		return
	}

	// Bidirectional splice
	done := make(chan struct{}, 2)

	// Client → Telegram (через CF Worker)
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32*1024)
		for {
			n, err := clientConn.Read(buf)
			if n > 0 {
				if err := conn.ws.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Telegram → Client (через CF Worker)
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			_, msg, err := conn.ws.ReadMessage()
			if err != nil {
				return
			}
			if _, err := clientConn.Write(msg); err != nil {
				return
			}
		}
	}()

	// Ждём завершения одного из направлений
	<-done
}

// ipToDCID определяет DC ID по IP адресу Telegram DC
func ipToDCID(ip string) int {
	if dcID, ok := dcIPToID[ip]; ok {
		return dcID
	}

	// Эвристика: проверяем подсети
	// Telegram DC 1: 149.154.175.0/24
	// Telegram DC 2: 149.154.167.0/24
	// Telegram DC 3: 149.154.175.0/24
	// Telegram DC 4: 149.154.167.0/24
	// Telegram DC 5: 149.154.171.0/24
	if strings.HasPrefix(ip, "149.154.175.") {
		return 1 // или 3, но обычно 1
	}
	if strings.HasPrefix(ip, "149.154.167.") {
		return 2 // или 4
	}
	if strings.HasPrefix(ip, "149.154.171.") {
		return 5
	}
	if strings.HasPrefix(ip, "91.108.") {
		return 2 // EU DC
	}
	if strings.HasPrefix(ip, "95.161.") {
		return 4 // EU DC
	}

	return 0
}
