// Package game — shared error codes for engine faults.
// Stored as int8 in the DB for compactness. The label map is the
// single source of truth for display strings.
package game

// Engine error codes (int8, stored in DB).
const (
	ErrNone             int8 = 0 // no error (infrastructure)
	ErrIllegalMove      int8 = 1
	ErrTimeout          int8 = 2
	ErrCrash            int8 = 3
	ErrResign           int8 = 4
	ErrInvalidResponse  int8 = 5
	ErrIllegalPass     int8 = 6
	ErrOpeningRejected  int8 = 7 // both engines rejected a legal opening move (MM drift?)
)

// ErrorLabel maps each error code to its human-readable label.
var ErrorLabel = map[int8]string{
	ErrIllegalMove:     "illegal move",
	ErrTimeout:         "timeout",
	ErrCrash:           "crash",
	ErrResign:          "resign",
	ErrInvalidResponse: "invalid response",
	ErrIllegalPass:    "illegal pass",
	ErrOpeningRejected: "opening rejected",
}

// CoachErrorCode maps a coach-reported error string to the int8 code.
func CoachErrorCode(s string) int8 {
	switch s {
	case "illegal_move":
		return ErrIllegalMove
	case "timeout":
		return ErrTimeout
	case "crash":
		return ErrCrash
	case "resign":
		return ErrResign
	case "invalid_response":
		return ErrInvalidResponse
	default:
		return ErrNone
	}
}
