package main

import (
	"flag"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// defaultTunnelSecret is injected at BUILD time, NOT committed to source, via:
//
//	go build -ldflags "-X main.defaultTunnelSecret=<hex>"
//
// (see mtproxy-client/Makefile, var Z2K_TUNNEL_SECRET). It is intentionally empty
// in the public repo so the shared tunnel credential is not published. A binary
// built without it requires --tunnel-secret at runtime (the router passes it from
// /opt/zapret2/config Z2K_RELAY_SECRET when set; see files/init.d/S98tg-tunnel).
var defaultTunnelSecret = ""

// buildVersion — версия релиза z2k, вшивается Makefile (-X main.buildVersion);
// уходит релею в HELLO, чтобы тот мог попросить обновиться.
var buildVersion = "dev"

var (
	listenAddrs  listenList
	tunnelURL    = flag.String("tunnel-url", "wss://213.176.74.63.nip.io/ws", "Tunnel relay WebSocket URL")
	tunnelSecret = flag.String("tunnel-secret", defaultTunnelSecret, "Shared secret for tunnel auth (build-injected; override with --tunnel-secret)")
	verbose      = flag.Bool("v", false, "Verbose logging")
	connTimeout  = flag.Duration("timeout", 15*time.Minute, "Idle connection timeout")
	maxConns     = flag.Int("max-conns", 1024, "Maximum concurrent connections")
	relayIDFile  = flag.String("relay-id-file", "/opt/zapret2/.z2k-relay-id", "per-install identity file (Stage B)")

	// Горячее переподключение и буферизация (оптимизация для слабого железа).
	// Позволяет сохранить активные стримы при кратковременных обрывах связи с релея
	// и буферизовать входящие соединения, пока релей недоступен.
	hotReconnect        = flag.Bool("hot-reconnect", true, "try hot reconnect before closing streams")
	hotReconnectTimeout = flag.Duration("hot-reconnect-timeout", 5*time.Second, "timeout for hot reconnect attempts")
	pendingTimeout      = flag.Duration("pending-timeout", 60*time.Second, "max time to buffer incoming connections while relay is down")
	maxPending          = flag.Int("max-pending", 256, "max buffered connections during reconnect")

	// Настраиваемый семафор CONNECT: лимит одновременных запросов к релею и таймаут
	// ожидания слота. Дефолт 6 слишком мал для пиковых нагрузок (100+ соединений
	// одновременно после рестарта), приводил к массовым CONNECT throttled (timeout).
	maxConnectPending = flag.Int("max-connect-pending", 64, "max concurrent CONNECT requests to relay")
	connectTimeout    = flag.Duration("connect-timeout", 3*time.Second, "timeout for CONNECT request slot acquisition")
)

// connSemaphore limits concurrent connections
var connSemaphore chan struct{}

// wsWriter serializes all writes to a WebSocket connection.
// gorilla/websocket supports only one concurrent writer.
type wsWriter struct {
	ws       *websocket.Conn
	mu       sync.Mutex
	deadline time.Time // Кэш дедлайна для избежания лишних SetWriteDeadline
}

func (w *wsWriter) WriteMessage(messageType int, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	// Оптимизация: обновляем дедлайн только если он скоро истечёт.
	// Избавляет от системного вызова SetWriteDeadline на каждый кадр.
	now := time.Now()
	if w.deadline.IsZero() || now.After(w.deadline.Add(-2*time.Second)) {
		w.deadline = now.Add(10 * time.Second)
		w.ws.SetWriteDeadline(w.deadline)
	}
	return w.ws.WriteMessage(messageType, data)
}

// WriteControl — без общего замка: gorilla допускает control-кадры параллельно
// с data-записью, а под замком пинг ждал медленную DATA-запись и WS умирал
// по «read timeout» ни за что.
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
