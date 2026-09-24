package main

import (
	"flag"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var defaultTunnelSecret = ""

var buildVersion = "dev"

var (
	listenAddrs  listenList
	tunnelURL    = flag.String("tunnel-url", "wss://213.176.74.63.nip.io/ws", "Tunnel relay WebSocket URL")
	tunnelSecret = flag.String("tunnel-secret", defaultTunnelSecret, "Shared secret for tunnel auth (build-injected; override with --tunnel-secret)")
	verbose      = flag.Bool("v", false, "Verbose logging")
	connTimeout  = flag.Duration("timeout", 15*time.Minute, "Idle connection timeout")
	maxConns     = flag.Int("max-conns", 1024, "Maximum concurrent connections")
	relayIDFile  = flag.String("relay-id-file", "/opt/zapret2/.z2k-relay-id", "per-install identity file (Stage B)")

	// Горячее переподключение и буферизация
	hotReconnect        = flag.Bool("hot-reconnect", true, "try hot reconnect before closing streams")
	hotReconnectTimeout = flag.Duration("hot-reconnect-timeout", 5*time.Second, "timeout for hot reconnect attempts")
	pendingTimeout      = flag.Duration("pending-timeout", 10*time.Second, "max time to buffer incoming connections")
	maxPending          = flag.Int("max-pending", 128, "max buffered connections during reconnect")
)

var connSemaphore chan struct{}

type wsWriter struct {
	ws       *websocket.Conn
	mu       sync.Mutex
	deadline time.Time
}

func (w *wsWriter) WriteMessage(messageType int, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	if w.deadline.IsZero() || now.After(w.deadline.Add(-2*time.Second)) {
		w.deadline = now.Add(10 * time.Second)
		w.ws.SetWriteDeadline(w.deadline)
	}
	return w.ws.WriteMessage(messageType, data)
}

func (w *wsWriter) WriteControl(messageType int, data []byte, deadline time.Time) error {
	return w.ws.WriteControl(messageType, data, deadline)
}

func init() {
	flag.Var(&listenAddrs, "listen", "Local listen address; repeatable or comma-separated (default :1443)")
}

func main() {
	flag.Parse()

	if err := runTunnel(); err != nil {
		log.Fatal(err)
	}
}
