package coach

import (
	"sync"

	"github.com/neoliv/arena/internal/game"
)

// CoachErrorStore holds engine error classifications reported by coaches.
// The coach is the authoritative source — it owns the engine process.
// The matchmaker consults this store: no coach verdict → infrastructure, engine blameless.
type CoachErrorStore struct {
	mu     sync.Mutex
	errors map[string]int8 // sessionID → game error code
}

// NewCoachErrorStore creates a new error store.
func NewCoachErrorStore() *CoachErrorStore {
	return &CoachErrorStore{errors: make(map[string]int8)}
}

// Report records an error for a session. The coach sends a string
// like "crash"; we map it to the int8 error code.
func (s *CoachErrorStore) Report(sessionID, errorType string) {
	s.mu.Lock()
	s.errors[sessionID] = game.CoachErrorCode(errorType)
	s.mu.Unlock()
}

// Get returns the coach-reported error code for a session, or 0 if none.
func (s *CoachErrorStore) Get(sessionID string) int8 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.errors[sessionID]
}

// Cleanup removes the error record for a session.
func (s *CoachErrorStore) Cleanup(sessionID string) {
	s.mu.Lock()
	delete(s.errors, sessionID)
	s.mu.Unlock()
}
