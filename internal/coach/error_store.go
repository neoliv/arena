package coach

import (
	"sync"
)

// CoachErrorStore holds engine error classifications reported by coaches.
// The coach is the authoritative source — it owns the engine process.
// The matchmaker consults this store: no coach verdict → infrastructure, engine blameless.
type CoachErrorStore struct {
	mu     sync.Mutex
	errors map[string]string // sessionID → error_type
}

// NewCoachErrorStore creates a new error store.
func NewCoachErrorStore() *CoachErrorStore {
	return &CoachErrorStore{errors: make(map[string]string)}
}

// Report records an error for a session.
func (s *CoachErrorStore) Report(sessionID, errorType string) {
	s.mu.Lock()
	s.errors[sessionID] = errorType
	s.mu.Unlock()
}

// Get returns the coach-reported error type for a session, or "" if none.
func (s *CoachErrorStore) Get(sessionID string) string {
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
