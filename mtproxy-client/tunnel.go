package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

const (
	muxCONNECT      = 0x01
	muxDATA         = 0x02
	muxCLOSE        = 0x03
	muxCONNECT_OK   = 0x04
	muxCONNECT_FAIL = 0x05
)

const (
	wsPingInterval = 10 * time.Second
	wsReadTimeout  = 30 * time.Second
)

const reRegMinIntervalSec = 120

const (
	addrIPv4 = 1
	addrIPv6 = 4
)

type muxFrame struct {
	StreamID uint16
	MsgType  byte
	Payload  []byte
}

func encodeMuxFrame(streamID uint16, msgType byte, payload []byte) []byte {
	buf := make([]byte, 3+len(payload))
	binary.BigEndian.PutUint16(buf[0:2], streamID)
	buf[2] = msgType
	if len(payload) > 0 {
		copy(buf[3:], payload)
	}
	return buf
}

func decodeMuxFrame(data []byte) (muxFrame, error) {
	if len(data) < 3 {
		return muxFrame{}, fmt.Errorf("mux frame too short: %d bytes", len(data))
	}
	return muxFrame{
		StreamID: binary.BigEndian.Uint16(data[0:2]),
		MsgType:  data[2],
		Payload:  data[3:],
	}, nil
}

func encodeConnectPayload(ip net.IP, port int) []byte {
	v4 := ip.To4()
	if v4 != nil {
		buf := make([]byte, 1+4+2)
		buf[0] = addrIPv4
		copy(buf[1:5], v4)
		binary.BigEndian.PutUint16(buf[5:7], uint16(port))
		return buf
	}
	buf := make([]byte, 1+16+2)
	buf[0] = addrIPv6
	copy(buf[1:17], ip.To16())
	binary.BigEndian.PutUint16(buf[17:19], uint16(port))
	return buf
}

func computeAuthHMAC(secret string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(secret))
	return mac.Sum(nil)
}

type pendingConn struct {
	clientConn   *net.TCPConn
	origIP       net.IP
	origPort     int
	waitingSince time.Time
}

func configureWSKeepalive(ws *websocket.Conn) {
	ws.SetReadLimit(2 * 1024 * 1024)
	_ = ws.SetReadDeadline(time.Now().Add(wsReadTimeout))
	ws.SetPongHandler(func(string) error {
		_ = ws.SetReadDeadline(time.Now().Add(wsReadTimeout))
		return nil
	})
	ws.SetPingHandler(func(data string) error {
		_ = ws.SetReadDeadline(time.Now().Add(wsReadTimeout))
		return ws.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})
}

type tunnelClient struct {
	tunnelURL    string
	tunnelSecret string

	identity     atomic.Pointer[relayIdentity]
	registerURL  string
	useID        atomic.Bool
	idFailStreak atomic.Int32
	reRegAt      atomic.Int64
	reRegBusy    atomic.Bool

	ws         *websocket.Conn
	writer     *wsWriter
	streams    sync.Map
	nextID     atomic.Uint32
	dropped    atomic.Uint64
	dropLogged atomic.Bool
	mu         sync.Mutex
	connectSem chan struct{}
	ctx        context.Context
	cancel     context.CancelFunc

	// Горячее переподключение и буферизация
	reconnecting atomic.Bool
	pendingMu    sync.Mutex
	pendingConns []*pendingConn

	// Протокол v2
	v2          atomic.Bool
	forceV1     atomic.Bool
	clockOffset atomic.Int64
	retryAfter  atomic.Int64
	window      atomic.Int64
}

type tunnelStream struct {
	id          uint16
	conn        *net.TCPConn
	client      *tunnelClient
	closeOnce   sync.Once
	remoteClose atomic.Bool
	semHeld     atomic.Bool
	writing     atomic.Bool

	outq *byteQueue

	credit      atomic.Int64
	creditWake  chan struct{}
	recvUnacked atomic.Int64

	// Заморозка при переподключении
	frozen         atomic.Bool
	reconnectReady chan struct{}

	// Кэшированные дедлайны
	writeDeadline time.Time
	readDeadline  time.Time
}

func newTunnelStream(id uint16, conn *net.TCPConn, tc *tunnelClient) *tunnelStream {
	return &tunnelStream{id: id, conn: conn, client: tc,
		outq:           newByteQueue(phoneQueueBytes),
		creditWake:     make(chan struct{}, 1),
		reconnectReady: make(chan struct{}, 1)}
}

const phoneQueueBytes = 4 * 1024 * 1024

func (s *tunnelStream) releaseSem() {
	if s.semHeld.CompareAndSwap(true, false) {
		select {
		case <-s.client.connectSem:
		default:
		}
	}
}

func (s *tunnelStream) close() {
	s.closeOnce.Do(func() {
		s.outq.close()
		s.conn.Close()
		s.client.streams.Delete(s.id)
		s.releaseSem()
		select {
		case s.creditWake <- struct{}{}:
		default:
		}
		if !s.remoteClose.Load() {
			s.client.mu.Lock()
			w := s.client.writer
			s.client.mu.Unlock()
			if w != nil {
				frame := encodeMuxFrame(s.id, muxCLOSE, nil)
				w.WriteMessage(websocket.BinaryMessage, frame)
			}
		}
		<-connSemaphore
	})
}

func (s *tunnelStream) phoneWriter() {
	tc := s.client
	var consumed int64
	defer s.close()
	for s.outq.wait(tc.ctx.Done()) {
		tc.mu.Lock()
		w := tc.writer
		tc.mu.Unlock()

		if w == nil || s.frozen.Load() {
			select {
			case <-s.reconnectReady:
				continue
			case <-tc.ctx.Done():
				return
			case <-time.After(30 * time.Second):
				return
			}
		}

		p, ok := s.outq.pop()
		if !ok {
			continue
		}
		now := time.Now()
		if s.writeDeadline.IsZero() || now.After(s.writeDeadline.Add(-1*time.Minute)) {
			s.writeDeadline = now.Add(*connTimeout)
			s.conn.SetWriteDeadline(s.writeDeadline)
		}
		if _, err := s.conn.Write(p); err != nil {
			if *verbose {
				log.Printf("[tunnel] stream %d write error: %v", s.id, err)
			}
			s.close()
			return
		}
		if tc.v2.Load() {
			consumed += int64(len(p))
			if win := tc.window.Load(); win > 0 && consumed >= win/2 {
				tc.mu.Lock()
				w := tc.writer
				tc.mu.Unlock()
				if w != nil {
					w.WriteMessage(websocket.BinaryMessage, encodeMuxFrame(s.id, muxWINDOW, encodeWindow(uint32(consumed))))
				}
				s.recvUnacked.Add(-consumed)
				consumed = 0
			}
		}
	}
}

func (s *tunnelStream) grant(credit int64) {
	s.credit.Add(credit)
	select {
	case s.creditWake <- struct{}{}:
	default:
	}
}

func (tc *tunnelClient) identityLoop() {
	for attempt := 0; ; attempt++ {
		if tc.identity.Load() == nil {
			if id, err := loadOrMintIdentity(*relayIDFile); err != nil {
				log.Printf("[tunnel] личность недоступна (%v) — повторю попытку", err)
			} else {
				tc.identity.Store(id)
			}
		}
		if tc.identity.Load() != nil && tc.registerOnce() {
			tc.useID.Store(true)
			if id := tc.identity.Load(); id != nil {
				log.Printf("[tunnel] registered identity %s — using per-install auth", id.InstallID)
			}
			return
		}
		wait := 30 * time.Second
		if attempt >= 20 {
			wait = 5 * time.Minute
		}
		select {
		case <-time.After(wait):
		case <-tc.ctx.Done():
			return
		}
	}
}

func (tc *tunnelClient) registerOnce() bool {
	id := tc.identity.Load()
	if id == nil || tc.registerURL == "" {
		return false
	}
	err := id.register(tc.registerURL, *tunnelSecret)
	if err == nil {
		return true
	}
	if errors.Is(err, errIdentityTaken) {
		log.Printf("[tunnel] идентификатор %s занят другим ключом — перевыпускаю личность", id.InstallID)
		fresh, mErr := reMintIdentity(*relayIDFile)
		if mErr != nil {
			log.Printf("[tunnel] перевыпуск личности не удался: %v", mErr)
			return false
		}
		tc.identity.Store(fresh)
		if rErr := fresh.register(tc.registerURL, *tunnelSecret); rErr != nil {
			log.Printf("[tunnel] регистрация новой личности не удалась: %v", rErr)
			return false
		}
		log.Printf("[tunnel] новая личность зарегистрирована (%s)", fresh.InstallID)
		return true
	}
	log.Printf("[tunnel] регистрация не удалась: %v", err)
	return false
}

func (tc *tunnelClient) triggerReRegister() {
	if tc.identity.Load() == nil || tc.registerURL == "" {
		return
	}
	now := time.Now().Unix()
	if last := tc.reRegAt.Load(); last > 0 && now-last < reRegMinIntervalSec {
		return
	}
	if !tc.reRegBusy.CompareAndSwap(false, true) {
		return
	}
	tc.reRegAt.Store(now)
	go func() {
		defer tc.reRegBusy.Store(false)
		if tc.registerOnce() {
			if id := tc.identity.Load(); id != nil {
				log.Printf("[tunnel] установка перерегистрирована (%s)", id.InstallID)
			}
		}
	}()
}

func (tc *tunnelClient) connectTunnelWS() (*websocket.Conn, error) {
	id := tc.identity.Load()
	if id == nil || !tc.useID.Load() {
		return nil, errNotRegistered
	}

	dialer := websocket.Dialer{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: false},
		HandshakeTimeout:  10 * time.Second,
		EnableCompression: false,
		NetDial: func(network, addr string) (net.Conn, error) {
			conn, err := net.DialTimeout("tcp4", relayDialAddr(addr), 10*time.Second)
			if err != nil {
				return nil, err
			}
			if tcpConn, ok := conn.(*net.TCPConn); ok {
				tcpConn.SetNoDelay(true)
			}
			return conn, nil
		},
	}
	ws, _, err := dialer.Dial(tc.tunnelURL, http.Header{})
	if err != nil {
		return nil, fmt.Errorf("WS dial %s: %w", tc.tunnelURL, err)
	}
	configureWSKeepalive(ws)

	if !tc.forceV1.Load() {
		if err := tc.handshakeV2(ws, id); err != nil {
			ws.Close()
			tc.forceV1.Store(true)
			return nil, fmt.Errorf("рукопожатие v2: %w (следующая попытка — v1)", err)
		}
		tc.v2.Store(true)
		log.Printf("[tunnel] connected to %s (proto v2, window %d)", tc.tunnelURL, tc.window.Load())
		return ws, nil
	}

	authFrame := encodeMuxFrame(0x0000, muxAUTHID, id.authPayload())
	ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := ws.WriteMessage(websocket.BinaryMessage, authFrame); err != nil {
		ws.Close()
		return nil, fmt.Errorf("WS auth write: %w", err)
	}
	tc.v2.Store(false)
	log.Printf("[tunnel] connected to %s (proto v1)", tc.tunnelURL)
	return ws, nil
}

var errHandshakeRejected = errors.New("relay rejected v2 handshake")

func (tc *tunnelClient) handshakeV2(ws *websocket.Conn, id *relayIdentity) error {
	ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := ws.WriteMessage(websocket.BinaryMessage, encodeMuxFrame(0, muxHELLO, encodeHello(buildVersion))); err != nil {
		return fmt.Errorf("HELLO: %w", err)
	}
	ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, msg, err := ws.ReadMessage()
	if err != nil {
		return fmt.Errorf("нет HELLO_ACK: %w", err)
	}
	f, err := decodeMuxFrame(msg)
	if err != nil || f.StreamID != 0 || f.MsgType != muxHELLO_ACK {
		return fmt.Errorf("вместо HELLO_ACK кадр 0x%02x: %w", f.MsgType, errHandshakeRejected)
	}
	ack, err := decodeHelloAck(f.Payload)
	if err != nil {
		return err
	}
	tc.clockOffset.Store(ack.ServerUnix - time.Now().Unix())
	tc.window.Store(int64(ack.DefaultWindow))
	if ack.MinBuild != "" && buildVersion != "dev" && buildVersion < ack.MinBuild {
		log.Printf("[tunnel] релей просит клиента не старше %s, у нас %s — обновитесь", ack.MinBuild, buildVersion)
	}
	ts := time.Now().Unix() + tc.clockOffset.Load()
	ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := ws.WriteMessage(websocket.BinaryMessage, encodeMuxFrame(0, muxAUTHID, id.authPayloadV2(ts, ack.Nonce))); err != nil {
		return fmt.Errorf("AUTHID: %w", err)
	}
	ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, msg, err = ws.ReadMessage()
	if err != nil {
		return fmt.Errorf("нет ответа на AUTHID: %w", err)
	}
	f, err = decodeMuxFrame(msg)
	if err != nil || f.StreamID != 0 || f.MsgType != muxINFO {
		return fmt.Errorf("вместо INFO кадр 0x%02x: %w", f.MsgType, errHandshakeRejected)
	}
	kind, arg, text, err := decodeInfo(f.Payload)
	if err != nil {
		return err
	}
	switch kind {
	case infoAuthOK:
		ws.SetReadDeadline(time.Now().Add(wsReadTimeout))
		return nil
	case infoClockSkew:
		log.Printf("[tunnel] релей: часы разошлись на %d с — поправка применена", int32(arg))
		return fmt.Errorf("часы: %w", errHandshakeRejected)
	case infoGoodbye:
		log.Printf("[tunnel] релей отказал: %s %s", reasonName(byte(arg)), text)
		if byte(arg) == rProtocol {
			return errHandshakeRejected
		}
		return errAuthRefused
	}
	return fmt.Errorf("INFO kind=%d в рукопожатии: %w", kind, errHandshakeRejected)
}

var errAuthRefused = errors.New("relay refused auth")

func (tc *tunnelClient) closeAllStreams() {
	tc.streams.Range(func(key, value any) bool {
		stream := value.(*tunnelStream)
		stream.close()
		return true
	})
}

func (tc *tunnelClient) freezeStreams() {
	tc.streams.Range(func(key, value any) bool {
		stream := value.(*tunnelStream)
		stream.frozen.Store(true)
		return true
	})
}

func (tc *tunnelClient) unfreezeStreams() {
	tc.streams.Range(func(key, value any) bool {
		stream := value.(*tunnelStream)
		if stream.frozen.CompareAndSwap(true, false) {
			select {
			case stream.reconnectReady <- struct{}{}:
			default:
			}
		}
		return true
	})
}

func (tc *tunnelClient) bufferPendingConn(clientConn *net.TCPConn, origIP net.IP, origPort int) {
	tc.pendingMu.Lock()
	defer tc.pendingMu.Unlock()

	if len(tc.pendingConns) >= *maxPending {
		if len(tc.pendingConns) > 0 {
			oldest := tc.pendingConns[0]
			oldest.clientConn.Close()
			tc.pendingConns = tc.pendingConns[1:]
			tc.dropped.Add(1)
		}
	}

	tc.pendingConns = append(tc.pendingConns, &pendingConn{
		clientConn:   clientConn,
		origIP:       origIP,
		origPort:     origPort,
		waitingSince: time.Now(),
	})

	if *verbose {
		log.Printf("[tunnel] buffered connection %s:%d (pending: %d)", origIP, origPort, len(tc.pendingConns))
	}
}

func (tc *tunnelClient) processPendingConns() {
	tc.pendingMu.Lock()
	pending := tc.pendingConns
	tc.pendingConns = nil
	tc.pendingMu.Unlock()

	if len(pending) == 0 {
		return
	}

	log.Printf("[tunnel] processing %d pending connections", len(pending))

	for _, pc := range pending {
		if time.Since(pc.waitingSince) > *pendingTimeout {
			pc.clientConn.Close()
			tc.dropped.Add(1)
			continue
		}
		tc.openStream(pc.clientConn, pc.origIP, pc.origPort)
	}
}

func (tc *tunnelClient) tryHotReconnect() bool {
	tc.reconnecting.Store(true)
	defer tc.reconnecting.Store(false)

	if !*hotReconnect {
		return false
	}

	deadline := time.Now().Add(*hotReconnectTimeout)
	attempt := 0

	for time.Now().Before(deadline) {
		attempt++
		ws, err := tc.connectTunnelWS()
		if err == nil {
			tc.mu.Lock()
			tc.ws = ws
			tc.writer = &wsWriter{ws: ws}
			tc.mu.Unlock()

			log.Printf("[tunnel] hot reconnect succeeded (attempt %d)", attempt)
			return true
		}

		if *verbose {
			log.Printf("[tunnel] hot reconnect attempt %d failed: %v", attempt, err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	log.Printf("[tunnel] hot reconnect failed after %d attempts", attempt)
	return false
}

func (tc *tunnelClient) readLoop(ws *websocket.Conn) {
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			if *verbose {
				log.Printf("[tunnel] WS read error: %v", err)
			}
			return
		}
		frame, err := decodeMuxFrame(msg)
		if err != nil {
			if *verbose {
				log.Printf("[tunnel] bad mux frame: %v", err)
			}
			continue
		}
		if frame.StreamID == 0 {
			tc.onControl(frame)
			continue
		}
		val, ok := tc.streams.Load(frame.StreamID)
		if !ok {
			if *verbose && frame.MsgType != muxCLOSE {
				log.Printf("[tunnel] frame for unknown stream %d (type=0x%02x)", frame.StreamID, frame.MsgType)
			}
			continue
		}
		stream := val.(*tunnelStream)

		switch frame.MsgType {
		case muxDATA:
			if tc.v2.Load() && stream.recvUnacked.Add(int64(len(frame.Payload))) > tc.window.Load() {
				log.Printf("[tunnel] stream %d: релей превысил окно — закрываю", frame.StreamID)
				stream.close()
				continue
			}
			if !stream.outq.push(frame.Payload) {
				if *verbose {
					log.Printf("[tunnel] stream %d: очередь к телефону переполнена", frame.StreamID)
				}
				stream.close()
			}

		case muxCLOSE:
			if r, txt := decodeClose(frame.Payload); tc.v2.Load() && r != rNormal {
				log.Printf("[tunnel] stream %d closed by relay: %s %s", frame.StreamID, reasonName(r), txt)
			} else if *verbose {
				log.Printf("[tunnel] stream %d closed by relay", frame.StreamID)
			}
			stream.remoteClose.Store(true)
			if stream.writing.Load() {
				stream.outq.finish()
			} else {
				stream.close()
			}

		case muxCONNECT_OK:
			stream.releaseSem()
			if tc.v2.Load() {
				if w := decodeConnectOK(frame.Payload); w > 0 {
					stream.credit.Store(int64(w))
				} else {
					stream.credit.Store(tc.window.Load())
				}
			}
			if *verbose {
				log.Printf("[tunnel] stream %d CONNECT_OK", frame.StreamID)
			}
			stream.writing.Store(true)
			go stream.phoneWriter()
			go tc.streamReadLoop(stream)

		case muxCONNECT_FAIL:
			stream.releaseSem()
			if r, txt := decodeClose(frame.Payload); tc.v2.Load() && len(frame.Payload) > 0 {
				log.Printf("[tunnel] stream %d CONNECT_FAIL: %s %s", frame.StreamID, reasonName(r), txt)
			} else {
				log.Printf("[tunnel] stream %d CONNECT_FAIL", frame.StreamID)
			}
			stream.remoteClose.Store(true)
			stream.close()

		case muxWINDOW:
			if c, err := decodeWindow(frame.Payload); err == nil {
				stream.grant(int64(c))
			}

		default:
			if *verbose {
				log.Printf("[tunnel] stream %d unknown msg type 0x%02x", frame.StreamID, frame.MsgType)
			}
		}
	}
}

func (tc *tunnelClient) onControl(frame muxFrame) {
	if frame.MsgType != muxINFO {
		return
	}
	kind, arg, text, err := decodeInfo(frame.Payload)
	if err != nil {
		return
	}
	switch kind {
	case infoRetryAfter:
		if arg > maxRetryAfterSec {
			arg = maxRetryAfterSec
		}
		tc.retryAfter.Store(int64(arg))
		log.Printf("[tunnel] релей просит переподключиться через %d с (%s)", arg, text)
	case infoUpdateRequired:
		log.Printf("[tunnel] релей: требуется обновление клиента (%s)", text)
	case infoClockSkew:
		log.Printf("[tunnel] релей: часы разошлись на %d с", int32(arg))
	case infoGoodbye:
		log.Printf("[tunnel] релей закрывает сессию: %s %s", reasonName(byte(arg)), text)
	}
}

func (tc *tunnelClient) streamReadLoop(stream *tunnelStream) {
	defer stream.close()

	buf := make([]byte, 16*1024+3)

	for {
		tc.mu.Lock()
		w := tc.writer
		tc.mu.Unlock()

		if w == nil || stream.frozen.Load() {
			select {
			case <-stream.reconnectReady:
				continue
			case <-tc.ctx.Done():
				return
			case <-time.After(30 * time.Second):
				if *verbose {
					log.Printf("[tunnel] stream %d: timeout waiting for reconnect", stream.id)
				}
				return
			}
		}

		want := len(buf) - 3
		if tc.v2.Load() {
			for stream.credit.Load() <= 0 {
				select {
				case <-stream.creditWake:
				case <-tc.ctx.Done():
					return
				}
				if stream.outq.isClosed() {
					return
				}
			}
			if cr := stream.credit.Load(); cr < int64(want) {
				want = int(cr)
			}
		}
		n, err := stream.conn.Read(buf[3 : 3+want])
		if n > 0 {
			now := time.Now()
			if stream.readDeadline.IsZero() || now.After(stream.readDeadline.Add(-1*time.Minute)) {
				stream.readDeadline = now.Add(*connTimeout)
				stream.conn.SetDeadline(stream.readDeadline)
			}
			if tc.v2.Load() {
				stream.credit.Add(-int64(n))
			}

			binary.BigEndian.PutUint16(buf[0:2], stream.id)
			buf[2] = muxDATA
			frame := buf[:3+n]

			tc.mu.Lock()
			w := tc.writer
			tc.mu.Unlock()
			if w == nil {
				return
			}
			if werr := w.WriteMessage(websocket.BinaryMessage, frame); werr != nil {
				if *verbose {
					log.Printf("[tunnel] stream %d WS write error: %v", stream.id, werr)
				}
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (tc *tunnelClient) run() {
	consecutiveFails := 0

	for {
		select {
		case <-tc.ctx.Done():
			return
		default:
		}

		ws, err := tc.connectTunnelWS()
		if errors.Is(err, errNotRegistered) {
			select {
			case <-time.After(5 * time.Second):
				continue
			case <-tc.ctx.Done():
				return
			}
		}
		if err != nil {
			consecutiveFails++
			backoff := jitter(backoffFor(consecutiveFails))
			log.Printf("[tunnel] connect failed (%d in a row, backoff %s): %v", consecutiveFails, backoff.Round(time.Millisecond), err)
			select {
			case <-time.After(backoff):
				continue
			case <-tc.ctx.Done():
				return
			}
		}

		tc.mu.Lock()
		tc.ws = ws
		tc.writer = &wsWriter{ws: ws}
		tc.mu.Unlock()

		tc.unfreezeStreams()
		tc.processPendingConns()

		if n := tc.dropped.Swap(0); n > 0 {
			log.Printf("[tunnel] WS поднят; пока его не было, отброшено соединений: %d", n)
		}
		tc.dropLogged.Store(false)

		connectedAt := time.Now()

		wsDone := make(chan struct{})
		pingDone := make(chan struct{})
		go func() {
			defer close(pingDone)
			ticker := time.NewTicker(wsPingInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					tc.mu.Lock()
					w := tc.writer
					tc.mu.Unlock()
					if w != nil {
						if err := w.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
							if *verbose {
								log.Printf("[tunnel] ping failed: %v", err)
							}
							_ = ws.Close()
							return
						}
					}
				case <-wsDone:
					return
				case <-tc.ctx.Done():
					return
				}
			}
		}()

		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			tc.readLoop(ws)
		}()
		<-readDone

		close(wsDone)
		log.Printf("[tunnel] WS disconnected")

		tc.freezeStreams()

		tc.mu.Lock()
		tc.ws = nil
		tc.writer = nil
		tc.mu.Unlock()
		ws.Close()

		if tc.tryHotReconnect() {
			continue
		}

		log.Printf("[tunnel] hot reconnect failed, closing all streams")
		tc.closeAllStreams()

		for {
			select {
			case <-tc.connectSem:
			default:
				goto drained
			}
		}
	drained:

		select {
		case <-pingDone:
		case <-time.After(2 * time.Second):
		}

		if tc.useID.Load() {
			if time.Since(connectedAt) < 8*time.Second {
				if tc.idFailStreak.Add(1) >= 3 {
					tc.idFailStreak.Store(0)
					tc.triggerReRegister()
				}
			} else {
				tc.idFailStreak.Store(0)
			}
		}

		if ra := tc.retryAfter.Swap(0); ra > 0 {
			wait := jitter(time.Duration(ra) * time.Second)
			log.Printf("[tunnel] reconnecting in %s (relay asked)", wait.Round(time.Millisecond))
			select {
			case <-time.After(wait):
			case <-tc.ctx.Done():
				return
			}
		} else if time.Since(connectedAt) < 5*time.Second {
			consecutiveFails++
			backoff := jitter(backoffFor(consecutiveFails))
			log.Printf("[tunnel] WS died too fast (%d in a row), backing off %s", consecutiveFails, backoff.Round(time.Millisecond))
			select {
			case <-time.After(backoff):
			case <-tc.ctx.Done():
				return
			}
		} else {
			consecutiveFails = 0
			select {
			case <-tc.ctx.Done():
				return
			case <-time.After(jitter(1 * time.Second)):
				log.Printf("[tunnel] reconnecting...")
			}
		}
	}
}

func (tc *tunnelClient) handleTunnelConn(clientConn *net.TCPConn) {
	clientConn.SetNoDelay(true)
	clientConn.SetDeadline(time.Now().Add(*connTimeout))

	origIP, origPort, err := getOriginalDst(clientConn)
	if err != nil {
		if *verbose {
			log.Printf("[tunnel] getOriginalDst failed: %v", err)
		}
		clientConn.Close()
		<-connSemaphore
		return
	}

	if isSelfDialAny(origIP, origPort, listenPorts) {
		if *verbose {
			log.Printf("[tunnel] самонабор на %s:%d — соединение пришло на слушатель напрямую, не через redirect", origIP, origPort)
		}
		clientConn.Close()
		<-connSemaphore
		return
	}

	tc.openStream(clientConn, origIP, origPort)
}

func (tc *tunnelClient) openStream(clientConn *net.TCPConn, origIP net.IP, origPort int) {
	var streamID uint16
	idFound := false
	for i := 0; i < 100; i++ {
		rawID := tc.nextID.Add(1)
		streamID = uint16(rawID%65535) + 1
		if _, exists := tc.streams.Load(streamID); !exists {
			idFound = true
			break
		}
	}
	if !idFound {
		log.Printf("[tunnel] stream ID exhaustion, dropping connection from %s", clientConn.RemoteAddr())
		clientConn.Close()
		<-connSemaphore
		return
	}

	tc.mu.Lock()
	w := tc.writer
	tc.mu.Unlock()
	if w == nil {
		tc.bufferPendingConn(clientConn, origIP, origPort)
		return
	}

	stream := newTunnelStream(streamID, clientConn, tc)
	tc.streams.Store(streamID, stream)

	if *verbose {
		log.Printf("[tunnel] stream %d: %s -> %s:%d", streamID, clientConn.RemoteAddr(), origIP, origPort)
	}

	select {
	case tc.connectSem <- struct{}{}:
		stream.semHeld.Store(true)
	case <-time.After(10 * time.Second):
		log.Printf("[tunnel] stream %d CONNECT throttled (timeout)", streamID)
		stream.remoteClose.Store(true)
		stream.close()
		return
	}

	connectPayload := encodeConnectPayload(origIP, origPort)
	frame := encodeMuxFrame(streamID, muxCONNECT, connectPayload)
	if err := w.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		log.Printf("[tunnel] stream %d CONNECT write error: %v", streamID, err)
		stream.remoteClose.Store(true)
		stream.close()
		return
	}
}

func runTunnel() error {
	if *tunnelURL == "" {
		return fmt.Errorf("--tunnel-url is required in tunnel mode")
	}
	if *tunnelSecret == "" {
		return fmt.Errorf("--tunnel-secret is required in tunnel mode")
	}

	connSemaphore = make(chan struct{}, *maxConns)

	addrs := listenAddrs.addrs()
	listenPorts = make(map[int]bool, len(addrs))
	for _, a := range addrs {
		if p := listenPortOf(a); p != 0 {
			listenPorts[p] = true
		}
	}
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

	log.Printf("[tunnel] listening on %s, relay=%s", strings.Join(addrs, " "), *tunnelURL)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tc := &tunnelClient{
		tunnelURL:    *tunnelURL,
		tunnelSecret: *tunnelSecret,
		connectSem:   make(chan struct{}, 6),
	}
	tc.ctx, tc.cancel = context.WithCancel(ctx)

	tc.registerURL = deriveRegisterURL(*tunnelURL)
	go tc.identityLoop()

	go tc.run()

	for i := 0; i < 100; i++ {
		tc.mu.Lock()
		w := tc.writer
		tc.mu.Unlock()
		if w != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-tc.ctx.Done():
				return
			case <-t.C:
				tc.mu.Lock()
				w := tc.writer
				tc.mu.Unlock()
				if w != nil {
					continue
				}
				if n := tc.dropped.Swap(0); n > 0 {
					log.Printf("[tunnel] WS всё ещё не поднят; за минуту отброшено соединений: %d", n)
				}
			}
		}
	}()

	go func() {
		<-ctx.Done()
		log.Println("[tunnel] shutting down...")
		tc.cancel()
		for _, ln := range lns {
			ln.Close()
		}
	}()

	var wg sync.WaitGroup
	errs := make(chan error, len(lns))
	for _, ln := range lns {
		wg.Add(1)
		go func(ln net.Listener) {
			defer wg.Done()
			errs <- tc.acceptLoop(ctx, ln)
		}(ln)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func (tc *tunnelClient) acceptLoop(ctx context.Context, ln net.Listener) error {
	var acceptDelay time.Duration
	const acceptDelayMax = 1 * time.Second

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				log.Println("[tunnel] stopped")
				return nil
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				log.Printf("[tunnel] listener closed: %v — stopping accept loop", err)
				return err
			}
			if acceptDelay == 0 {
				acceptDelay = 5 * time.Millisecond
			} else {
				acceptDelay *= 2
			}
			if acceptDelay > acceptDelayMax {
				acceptDelay = acceptDelayMax
			}
			log.Printf("[tunnel] accept error: %v — retrying in %v", err, acceptDelay)
			select {
			case <-time.After(acceptDelay):
			case <-ctx.Done():
				log.Println("[tunnel] stopped")
				return nil
			}
			continue
		}
		acceptDelay = 0
		tcpConn, ok := conn.(*net.TCPConn)
		if !ok {
			conn.Close()
			continue
		}

		select {
		case connSemaphore <- struct{}{}:
			go tc.handleTunnelConn(tcpConn)
		default:
			if *verbose {
				log.Printf("[tunnel] max connections reached, rejecting %s", conn.RemoteAddr())
			}
			conn.Close()
		}
	}
}

const maxRetryAfterSec = 5

func backoffFor(consecutiveFails int) time.Duration {
	switch {
	case consecutiveFails >= 10:
		return 120 * time.Second
	case consecutiveFails >= 5:
		return 30 * time.Second
	case consecutiveFails >= 3:
		return 10 * time.Second
	}
	return 3 * time.Second
}

func jitter(d time.Duration) time.Duration {
	f := 0.7 + rand.Float64()*0.6
	return time.Duration(float64(d) * f)
}
