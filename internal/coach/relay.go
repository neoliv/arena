package coach

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

// Stream is a bidirectional GTP channel pair for a single engine session.
// The match executor writes GTP commands to Out and reads responses from In.
type Stream struct {
	In  <-chan string
	Out chan<- string
}

// Relay manages WebSocket sessions for GTP relay.
type Relay struct {
	mu       sync.Mutex
	sessions map[string]*relaySlot
}

type relaySlot struct {
	stream Stream
	ready  chan struct{} // closed when stream is available
	done   chan struct{} // closed by Cleanup
	cancel context.CancelFunc
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
		OriginPatterns: []string{"arena.arsac.org", "localhost"},
	})
	if err != nil {
		slog.Error("ws accept", "err", err)
		return
	}

	in := make(chan string, 8)
	out := make(chan string, 8)
	ctx, cancel := context.WithCancel(context.Background())

	// Reader: WebSocket → in channel
	go func() {
		defer close(in)
		for {
			_, msg, err := conn.Read(ctx)
			if err != nil {
				return
			}
			select {
			case in <- string(msg):
			case <-ctx.Done():
				return
			}
		}
	}()

	// Writer: out channel → WebSocket
	go func() {
		for {
			select {
			case cmd, ok := <-out:
				if !ok {
					return
				}
				if err := conn.Write(ctx, websocket.MessageText, []byte(cmd)); err != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	stream := Stream{In: in, Out: out}

	r.mu.Lock()
	slot, exists := r.sessions[sessionID]
	if exists {
		if slot.cancel != nil { slot.cancel() }
		slot.stream = stream
		if slot.ready != nil {
			select {
			case <-slot.ready:
				// already closed, don't close again
			default:
				close(slot.ready)
			}
		}
	} else {
		slot = &relaySlot{stream: stream, ready: make(chan struct{}), done: make(chan struct{}), cancel: cancel}
		close(slot.ready)
		r.sessions[sessionID] = slot
	}
	r.mu.Unlock()

	slog.Info("relay engine connected", "session", sessionID)

	select {
	case <-slot.done:
	case <-time.After(10 * time.Minute):
		slog.Warn("relay timed out waiting for cleanup", "session", sessionID)
	}
	cancel()
	conn.Close(websocket.StatusNormalClosure, "match done")
}

// ErrRelayTimeout is returned when waiting for a coach times out.
var ErrRelayTimeout = errors.New("relay timeout")

// WaitForStream blocks until a coach connects and returns a bidirectional stream.
func (r *Relay) WaitForStream(sessionID string, timeoutSec int) (Stream, error) {
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
		s := slot.stream
		r.mu.Unlock()
		return s, nil
	case <-time.After(time.Duration(timeoutSec) * time.Second):
		r.mu.Lock()
		if slot.stream.In == nil && slot.stream.Out == nil {
			delete(r.sessions, sessionID)
		}
		r.mu.Unlock()
		return Stream{}, ErrRelayTimeout
	}
}

// Cleanup signals the handler to stop and removes the session.
func (r *Relay) Cleanup(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	slot, ok := r.sessions[sessionID]
	if ok && slot.done != nil {
		close(slot.done)
	}
	delete(r.sessions, sessionID)
}
