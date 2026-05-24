package observer

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// broadcastInterval is the cadence of the periodic stats + log_append push
// (SPEC §4.3).
const (
	broadcastInterval = 500 * time.Millisecond
	wsReadLimit       = 4 << 10          // cap inbound cmd messages
	wsReadTimeout     = 60 * time.Second // each pong must arrive within this
	wsWriteTimeout    = 10 * time.Second
	wsPingInterval    = 25 * time.Second
)

// Broadcaster owns the WebSocket fan-out: it accepts /stream upgrades, sends
// a one-time snapshot per new connection, and ticks every 500ms to push the
// latest stats + any newly-appended log lines to all connected clients.
type Broadcaster struct {
	hub            *Hub
	log            *slog.Logger
	upgrader       websocket.Upgrader
	allowedOrigins map[string]bool
	mu             sync.Mutex
	clients        map[*client]struct{}
	stop           chan struct{}
	done           chan struct{}
}

type client struct {
	conn       *websocket.Conn
	out        chan []byte
	logsCursor int64
	done       chan struct{}
	closeOnce  sync.Once
}

// close signals all the client's goroutines (writeLoop) and any in-flight
// trySend calls to stop. Safe to call concurrently or repeatedly.
func (c *client) close() {
	c.closeOnce.Do(func() { close(c.done) })
}

// NewBroadcaster builds the broadcaster but does not start it — call Start
// after Hub.Start so the aggregator is already draining /ingest. The
// allowedOrigins list comes from OBSERVER_ALLOWED_ORIGINS; the showcase page
// is cross-origin (page :8090 vs observer :9090), so gorilla's default
// CheckOrigin would reject it.
func NewBroadcaster(hub *Hub, log *slog.Logger, allowedOrigins []string) *Broadcaster {
	allow := make(map[string]bool, len(allowedOrigins))
	for _, o := range allowedOrigins {
		o = strings.TrimSpace(o)
		if o != "" {
			allow[o] = true
		}
	}
	b := &Broadcaster{
		hub:            hub,
		log:            log,
		allowedOrigins: allow,
		clients:        make(map[*client]struct{}),
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
	}
	b.upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 4096,
		CheckOrigin: func(r *http.Request) bool {
			// Browsers always send Origin on WebSocket upgrades. Allowing
			// an empty Origin would let any local process (wscat, curl)
			// connect and harvest key hints / webhook URLs from the
			// broadcast — so we require an explicit allowlisted Origin.
			return b.allowedOrigins[r.Header.Get("Origin")]
		},
	}
	return b
}

// Register attaches the WS endpoint to the given mux. Call once before Start.
func (b *Broadcaster) Register(mux *http.ServeMux) {
	mux.HandleFunc("/stream", b.handleStream)
}

// Start launches the tick loop.
func (b *Broadcaster) Start() {
	go b.run()
}

// Shutdown stops the tick loop and closes every connection.
func (b *Broadcaster) Shutdown(ctx context.Context) error {
	close(b.stop)
	select {
	case <-b.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	b.mu.Lock()
	for c := range b.clients {
		c.close()
		_ = c.conn.Close()
	}
	b.clients = make(map[*client]struct{})
	b.mu.Unlock()
	return nil
}

func (b *Broadcaster) run() {
	defer close(b.done)
	ticker := time.NewTicker(broadcastInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			b.broadcastTick()
		case <-b.stop:
			return
		}
	}
}

func (b *Broadcaster) handleStream(w http.ResponseWriter, r *http.Request) {
	conn, err := b.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrader has already written an error response.
		b.log.Debug("ws upgrade", "error", err)
		return
	}
	conn.SetReadLimit(wsReadLimit)
	_ = conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
	})

	c := &client{conn: conn, out: make(chan []byte, 32), done: make(chan struct{})}

	keys, logs, system, ts := b.hub.State().Snapshot()
	_, cursor := b.hub.State().LogsSince(0)
	c.logsCursor = cursor
	snap := struct {
		Type     string     `json:"type"`
		TS       time.Time  `json:"ts"`
		KeyStats []KeyStat  `json:"key_stats"`
		Logs     []LogEntry `json:"logs"`
		System   SystemStat `json:"system"`
	}{Type: "snapshot", TS: ts, KeyStats: keys, Logs: logs, System: system}
	if payload, err := json.Marshal(snap); err == nil {
		c.out <- payload
	}

	b.mu.Lock()
	b.clients[c] = struct{}{}
	b.mu.Unlock()

	go b.writeLoop(c)
	b.readLoop(c) // blocks until disconnect
}

// readLoop processes inbound cmd messages and pumps the read deadline forward.
// Returns on any error, after which writeLoop will also exit when out closes.
func (b *Broadcaster) readLoop(c *client) {
	defer b.disconnect(c)
	for {
		_, msg, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var cmd struct {
			Type   string `json:"type"`
			Action string `json:"action"`
		}
		if err := json.Unmarshal(msg, &cmd); err != nil || cmd.Type != "cmd" {
			continue
		}
		switch cmd.Action {
		case "clear_logs":
			b.hub.State().clearLogs()
			b.broadcastReset("logs")
		case "reset_stats":
			b.hub.State().resetStats()
			b.broadcastReset("stats")
		}
	}
}

func (b *Broadcaster) writeLoop(c *client) {
	pingTicker := time.NewTicker(wsPingInterval)
	defer pingTicker.Stop()
	for {
		select {
		case <-c.done:
			return
		case msg := <-c.out:
			_ = c.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-pingTicker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (b *Broadcaster) disconnect(c *client) {
	b.mu.Lock()
	delete(b.clients, c)
	b.mu.Unlock()
	c.close()
	_ = c.conn.Close()
}

// broadcastTick is invoked every broadcastInterval. It builds the stats frame
// once (small, shared across all clients) and the per-client log_append frame
// from each client's own cursor (so reconnects don't replay anyone else's
// already-shipped logs).
func (b *Broadcaster) broadcastTick() {
	keys, system, ts := b.hub.State().StatsSnapshot()
	stats := struct {
		Type     string     `json:"type"`
		TS       time.Time  `json:"ts"`
		KeyStats []KeyStat  `json:"key_stats"`
		System   SystemStat `json:"system"`
	}{Type: "stats", TS: ts, KeyStats: keys, System: system}
	statsBytes, err := json.Marshal(stats)
	if err != nil {
		b.log.Debug("marshal stats frame", "error", err)
		return
	}

	b.mu.Lock()
	clients := make([]*client, 0, len(b.clients))
	for c := range b.clients {
		clients = append(clients, c)
	}
	b.mu.Unlock()

	for _, c := range clients {
		b.trySend(c, statsBytes)
		newLogs, cursor := b.hub.State().LogsSince(c.logsCursor)
		c.logsCursor = cursor
		if len(newLogs) == 0 {
			continue
		}
		appendFrame := struct {
			Type string     `json:"type"`
			TS   time.Time  `json:"ts"`
			Logs []LogEntry `json:"logs"`
		}{Type: "log_append", TS: ts, Logs: newLogs}
		if payload, err := json.Marshal(appendFrame); err == nil {
			b.trySend(c, payload)
		}
	}
}

// trySend pushes msg onto the client's send buffer; if the buffer is full the
// client is too slow and gets disconnected (we'd rather drop one stale viewer
// than back up the entire broadcast). If the client is already disconnected,
// the message is silently dropped.
func (b *Broadcaster) trySend(c *client, msg []byte) {
	select {
	case <-c.done:
	case c.out <- msg:
	default:
		b.log.Debug("ws client backed up, disconnecting")
		b.disconnect(c)
	}
}

// broadcastReset fans the reset frame out to every connected client so all
// viewers see the cleared state in lock-step, not just the one that issued
// the cmd. Snapshots the client set under mu, sends outside the lock the same
// way broadcastTick does, so trySend / disconnect races stay symmetric.
func (b *Broadcaster) broadcastReset(scope string) {
	frame := struct {
		Type  string `json:"type"`
		Scope string `json:"scope"`
	}{Type: "reset", Scope: scope}
	payload, err := json.Marshal(frame)
	if err != nil {
		return
	}
	b.mu.Lock()
	clients := make([]*client, 0, len(b.clients))
	for c := range b.clients {
		clients = append(clients, c)
	}
	b.mu.Unlock()
	for _, c := range clients {
		b.trySend(c, payload)
	}
}
