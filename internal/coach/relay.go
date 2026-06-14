package coach

import (
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

// Relay manages WebSocket sessions for GTP relay.
type Relay struct {
	mu       sync.Mutex
	sessions map[string]*relaySlot
}

type relaySlot struct {
	conn  *websocket.Conn
	ready chan struct{} // closed when connection is available
}

// NewRelay creates a new relay manager.
func NewRelay() *Relay {
	return &Relay{sessions: make(map[string]*relaySlot)}
}

// HandleRelay handles WebSocket upgrade from a coach.
func (r *Relay) HandleRelay(w http.ResponseWriter, req *http.Request) {
	sessionID := req.PathValue("session_id")
	if sessionID == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}

	conn, err := websocket.Accept(w, req, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		slog.Error("ws accept", "err", err)
		return
	}

	r.mu.Lock()
	slot, exists := r.sessions[sessionID]
	if exists && slot.conn != nil {
		r.mu.Unlock()
		conn.Close(websocket.StatusNormalClosure, "session already connected")
		slog.Warn("relay duplicate connection", "session", sessionID)
		return
	}
	if exists {
		slot.conn = conn
		close(slot.ready)
	} else {
		slot = &relaySlot{conn: conn, ready: make(chan struct{})}
		close(slot.ready)
		r.sessions[sessionID] = slot
	}
	r.mu.Unlock()

	slog.Info("relay engine connected", "session", sessionID)
	// Return immediately — Accept hijacks the connection from the HTTP
	// handler. The match executor uses it via WaitForConn, and Cleanup
	// closes it when the match ends.
}

// ErrRelayTimeout is returned when waiting for a coach times out.
var ErrRelayTimeout = errors.New("relay timeout")

// WaitForConn blocks until a coach connects for the given session.
func (r *Relay) WaitForConn(sessionID string, timeoutSec int) (*websocket.Conn, error) {
	r.mu.Lock()
	slot, exists := r.sessions[sessionID]
	if !exists {
		slot = &relaySlot{ready: make(chan struct{})}
		r.sessions[sessionID] = slot
	}
	r.mu.Unlock()

	select {
	case <-slot.ready:
		r.mu.Lock()
		c := slot.conn
		r.mu.Unlock()
		return c, nil
	case <-time.After(time.Duration(timeoutSec) * time.Second):
		return nil, ErrRelayTimeout
	}
}

// Cleanup removes a session and closes the connection.
func (r *Relay) Cleanup(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	slot, ok := r.sessions[sessionID]
	if ok && slot.conn != nil {
		slot.conn.Close(websocket.StatusNormalClosure, "match done")
	}
	delete(r.sessions, sessionID)
}
