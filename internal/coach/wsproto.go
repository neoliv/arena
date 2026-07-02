// Package coach — WebSocket protocol messages for coach↔MM communication.
package coach

// CoachMessage is a JSON message sent from the coach to the MM.
type CoachMessage struct {
	Type string `json:"type"` // "register", "heartbeat", "engine_exited", "engine_timeout", "engine_crash"

	// register fields
	CoachID  string        `json:"coach_id,omitempty"`
	Cores    int           `json:"cores,omitempty"`
	MemMB    int           `json:"mem_mb,omitempty"`
	Engines  []EngineInfo  `json:"engines,omitempty"`

	// heartbeat fields
	CoresUsed int            `json:"cores_used,omitempty"`
	MemUsed   int            `json:"mem_used,omitempty"`
	Players   []PlayerStatus `json:"players,omitempty"`

	// engine event fields
	Session string `json:"session,omitempty"`
	OK      bool   `json:"ok,omitempty"`
	Error   string `json:"error,omitempty"`
}

// EngineInfo describes an engine the coach can run (sent at registration).
type EngineInfo struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	Cores        int    `json:"cores"`
	MemMB        int    `json:"mem_mb"`
	MaxInstances int    `json:"max_instances"`
	RunCmd       string `json:"run_cmd"`
	EngineID     string `json:"engine_id"`
}

// PlayerStatus reports current running instances of an engine.
type PlayerStatus struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Instances int    `json:"instances"`
}

// MMMessage is a JSON command sent from the MM to a coach.
type MMMessage struct {
	Type string `json:"type"` // "launch", "kill"

	// launch fields
	Session     string      `json:"session,omitempty"`
	Engine      string      `json:"engine,omitempty"` // "name:version"
	Side        string      `json:"side,omitempty"`   // "black" | "white"
	TimeControl TimeControl `json:"time_control,omitempty"`
	Opening     string      `json:"opening,omitempty"`

	// kill fields
	Reason string `json:"reason,omitempty"`
}

// TimeControl specifies the time budget for a game.
type TimeControl struct {
	Seconds float64 `json:"seconds"`
}
