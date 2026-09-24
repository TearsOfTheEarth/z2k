package main

import (
	"context"
	"encoding/binary"
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
	go wsPool.warmup()

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

	initBuf := make([]byte, 64)
	n, err := clientConn.Read(initBuf)
	if err != nil {
		if *verbose {
			log.Printf("[cfproxy] read init failed: %v", err)
		}
		return
	}

	dcID := extractCfDCID(initBuf[:n])
	if dcID == 0 {
		if *verbose {
			log.Printf("[cfproxy] cannot extract DC ID from %s", clientConn.RemoteAddr())
		}
		return
	}

	isMedia := dcID == 4

	if *verbose {
		log.Printf("[cfproxy] %s → DC%d%s", clientConn.RemoteAddr(), dcID, mediaTag(isMedia))
	}

	var conn *wsPoolConn
	conn = wsPool.get(dcID, isMedia)
	if conn != nil {
		if *verbose {
			log.Printf("[cfproxy] pool hit DC%d%s via %s", dcID, mediaTag(isMedia), conn.domain)
		}
	}

	if conn == nil {
		var err error
		conn, err = wsPool.connect(dcID, isMedia)
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

	if err := conn.ws.WriteMessage(websocket.BinaryMessage, initBuf[:n]); err != nil {
		log.Printf("[cfproxy] write init failed: %v", err)
		return
	}

	done := make(chan struct{}, 2)

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

	<-done
}

func extractCfDCID(buf []byte) int {
	if len(buf) < 64 {
		return 0
	}

	if len(buf) >= 64 {
		dcID := int(binary.LittleEndian.Uint32(buf[60:64]))
		if dcID >= 1 && dcID <= 5 {
			return dcID
		}
		if dcID == 203 {
			return 203
		}
	}

	return 0
}
