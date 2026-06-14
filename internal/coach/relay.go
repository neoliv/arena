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
// Each session has one connection from a coach (engine).
// The match runner picks up the connection to send/receive GTP.
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
	if exists {
		// Second connection? Close — only one per session in this model.
		r.mu.Unlock()
		conn.Close(websocket.StatusNormalClosure, "session already connected")
		slog.Warn("relay duplicate connection", "session", sessionID)
		return
	}
	slot = &relaySlot{conn: conn, ready: make(chan struct{})}
	close(slot.ready)
	r.sessions[sessionID] = slot
	r.mu.Unlock()

	slog.Info("relay engine connected", "session", sessionID)

	// Keep the connection alive until the match is done.
	// Read in a loop to detect disconnection.
	for {
		_, _, err := conn.Read(req.Context())
		if err != nil { break }
	}
	r.mu.Lock()
	delete(r.sessions, sessionID)
	r.mu.Unlock()
	slog.Info("relay engine disconnected", "session", sessionID)
}

// ErrRelayTimeout is returned when waiting for a coach times out.
var ErrRelayTimeout = errors.New("relay timeout")

// WaitForConn blocks until a coach connects for the given session, then returns the connection.
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

// GetConn returns the connection for a session if available.
func (r *Relay) GetConn(sessionID string) *websocket.Conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	slot, ok := r.sessions[sessionID]
	if !ok || slot.conn == nil { return nil }
	return slot.conn
}

// Cleanup removes a session.
func (r *Relay) Cleanup(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, sessionID)
}
