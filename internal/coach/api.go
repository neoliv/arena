package coach

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/neoliv/arena/internal/db"
	"nhooyr.io/websocket"
)

// Handler manages coach REST endpoints: register, heartbeat.
// The old task-based assignment system (HandleTasks, HandleTaskStatus)
// has been replaced by the matchmaker's pull-based polling
// (GET /api/matchmaker/poll, POST /api/matchmaker/complete).
type Handler struct {
	DB            *db.DB
	Token         string
	Relay         *Relay
	ValidateToken func(string) bool
	ServerGen     string // random ID regenerated on server restart
	ErrorStore    *CoachErrorStore
	rateMu        sync.Mutex
	rateWindows   map[string][]time.Time

	// OnHeartbeat is called on each coach heartbeat to update in-memory state.
	// Replaces the old DB coach_ais.instances_running update.
	// Returns true if the session ID changed (coach restarted).
	OnHeartbeat func(coachID string, sessionID string, aiUpdates map[string]int) (sessionChanged bool)

	// WebSocket callbacks (push-based coach protocol).
	OnWSConnect    func(conn *websocket.Conn)
	OnWSDisconnect func(conn *websocket.Conn)
	OnCoachMessage func(conn *websocket.Conn, msg CoachMessage)
}

type registerReq struct {
	CoachID   string `json:"coach_id"`
	Token     string `json:"token"`
	Label     string `json:"label"`
	Version   string `json:"version"`
	Resources struct {
		Cores    int `json:"cores"`
		MemoryMB int `json:"memory_mb"`
	} `json:"resources"`
	AIs []struct {
		Name             string `json:"name"`
		Version          string `json:"version"`
		Created          string `json:"created"`
		ChangelogShort   string `json:"changelog_short"`
		ChangelogFull    string `json:"changelog_full"`
		BuildCmd         string `json:"build_cmd"`
		RunCmd           string `json:"run_cmd"`
		EngineID         string `json:"engine_id"`
		EngineManifest   string `json:"engine_manifest"`
		ResourceCores    int    `json:"resource_cores"`
		ResourceMemoryMB int    `json:"resource_memory_mb"`
		MaxConcurrency   int    `json:"max_concurrency"`
	} `json:"ais"`
}

type heartbeatReq struct {
	CoachID      string              `json:"coach_id"`
	Token        string              `json:"token"`
	SessionID    string              `json:"session_id"`
	AIsAvailable []heartbeatAI       `json:"ais_available"`
	Resources    *heartbeatResources `json:"resources"`
}

type heartbeatAI struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	CurrentMatches int    `json:"current_matches"`
	MaxConcurrency int    `json:"max_concurrency"`
}

type heartbeatResources struct {
	CoresUsed    int `json:"cores_used"`
	MemoryMBUsed int `json:"memory_mb_used"`
}

func NewHandler(database *db.DB, token string, relay *Relay, validateToken func(string) bool, serverGen string) *Handler {
	return &Handler{
		DB: database, Token: token, Relay: relay, ValidateToken: validateToken,
		ServerGen: serverGen,
	}
}

func (h *Handler) checkAuth(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	t := strings.TrimPrefix(auth, "Bearer ")
	if t == "" {
		return false
	}
	if h.ValidateToken != nil && h.ValidateToken(t) {
		return true
	}
	if h.Token != "" && t == h.Token {
		return true
	}
	return false
}

func (h *Handler) checkAuthOrOpen(r *http.Request) bool {
	if h.Token == "" && h.ValidateToken == nil {
		return true
	}
	return h.checkAuth(r)
}

func (h *Handler) checkRate(coachID string) bool {
	h.rateMu.Lock()
	defer h.rateMu.Unlock()
	if h.rateWindows == nil {
		h.rateWindows = make(map[string][]time.Time)
	}
	now := time.Now()
	cutoff := now.Add(-30 * time.Second)
	recent := h.rateWindows[coachID]
	valid := recent[:0]
	for _, t := range recent {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	if len(valid) >= 60 {
		return false
	}
	valid = append(valid, now)
	h.rateWindows[coachID] = valid
	return true
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// HandleRegister persists ENGINE info to the database (persistent identity).
// Coach and assignment state is in-memory only (MM's WantedList), reset on restart.
// The matchmaker's in-memory registry is populated via POST /api/matchmaker/register.
func (h *Handler) HandleRegister(w http.ResponseWriter, r *http.Request) {
	if !h.checkAuthOrOpen(r) {
		jsonErr(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req registerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.CoachID == "" {
		jsonErr(w, "coach_id required", http.StatusBadRequest)
		return
	}

	registered := 0
	for _, ai := range req.AIs {
		if registered >= 64 {
			break
		}
		if ai.Name == "" || ai.Version == "" {
			continue
		}
		_, err := h.DB.Exec(`INSERT INTO engines (name, version, created, changelog_short, changelog_full, engine_id, engine_manifest) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(name, version) DO UPDATE SET created=excluded.created, changelog_short=excluded.changelog_short, changelog_full=excluded.changelog_full, engine_id=excluded.engine_id, engine_manifest=excluded.engine_manifest`,
			ai.Name, ai.Version, ai.Created, ai.ChangelogShort, ai.ChangelogFull, ai.EngineID, ai.EngineManifest)
		if err != nil {
			slog.Error("upsert engine", "name", ai.Name, "version", ai.Version, "err", err)
		}
		registered++
	}
	jsonOK(w, map[string]any{"status": "registered", "ais_registered": registered})
}

// HandleHeartbeat updates coach liveness and running AI counts in memory.
// The old DB-backed coaches/coach_ais tables are no longer used — everything
// is in the MM's in-memory WantedList (repopulated on re-registration).
func (h *Handler) HandleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if !h.checkAuthOrOpen(r) {
		jsonErr(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<18)
	var req heartbeatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.CoachID == "" {
		jsonErr(w, "coach_id required", http.StatusBadRequest)
		return
	}

	aiUpdates := make(map[string]int)
	for _, ai := range req.AIsAvailable {
		key := ai.Name + ":" + ai.Version
		aiUpdates[key] = ai.CurrentMatches
	}

	// Update in-memory state via the MM callback.
	if h.OnHeartbeat != nil {
		h.OnHeartbeat(req.CoachID, req.SessionID, aiUpdates)
	}

	jsonOK(w, map[string]any{"ok": true, "server_gen": h.ServerGen})
}

// ── Per-player resource stats (in-memory, sent by coaches every ~20s) ─

// MinMaxAvgStd holds statistical summary of a metric.
type MinMaxAvgStd struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
	Avg float64 `json:"avg"`
	Std float64 `json:"std"`
}

// ResourceWindow holds CPU and RAM stats for one aggregation window.
type ResourceWindow struct {
	CPUPct MinMaxAvgStd `json:"cpu_pct"`
	RSSMb  MinMaxAvgStd `json:"rss_mb"`
}

// PlayerResourceSnapshot is a point-in-time resource usage report for one player.
type PlayerResourceSnapshot struct {
	Name       string         `json:"name"`
	Version    string         `json:"version"`
	CoachID    string         `json:"coach_id"`
	Instances  int            `json:"instances"`
	MemoryMB   int            `json:"memory_mb"` // declared RAM allocation from player config
	Interval   ResourceWindow `json:"interval"`
	Cumulative ResourceWindow `json:"cumulative"`
	UpdatedAt  time.Time      `json:"-"`
}

// PlayerResourceStore holds the latest resource snapshot per player key.
type PlayerResourceStore struct {
	mu    sync.RWMutex
	stats map[string]*PlayerResourceSnapshot // key: "name:version"
}

// NewPlayerResourceStore creates a new store.
func NewPlayerResourceStore() *PlayerResourceStore {
	return &PlayerResourceStore{stats: make(map[string]*PlayerResourceSnapshot)}
}

// Update replaces the snapshot for each player in the payload.
func (s *PlayerResourceStore) Update(players []PlayerResourceSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, p := range players {
		key := p.Name + ":" + p.Version
		if existing, ok := s.stats[key]; ok {
			// Preserve stats from other coaches for the same engine
			if existing.CoachID == p.CoachID {
				*existing = p
				existing.UpdatedAt = now
			} else {
				// Different coach, different entry
				p.UpdatedAt = now
				s.stats[key+"@"+p.CoachID] = &p
			}
		} else {
			p.UpdatedAt = now
			s.stats[key] = &p
		}
	}
}

// GetAll returns fresh snapshots (updated within staleness).
func (s *PlayerResourceStore) GetAll(staleTimeout time.Duration) []*PlayerResourceSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	var out []*PlayerResourceSnapshot
	for _, p := range s.stats {
		if now.Sub(p.UpdatedAt) <= staleTimeout {
			out = append(out, p)
		}
	}
	return out
}

// HandleResources receives per-player resource stats from a coach.
func (s *PlayerResourceStore) HandleResources(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CoachID string                   `json:"coach_id"`
		Players []PlayerResourceSnapshot `json:"players"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, 400)
		return
	}
	for i := range req.Players {
		req.Players[i].CoachID = req.CoachID
	}
	s.Update(req.Players)
	jsonOK(w, map[string]any{"ok": true})
}

// HandleEngineError receives an engine error classification from the coach.
// The coach is authoritative — it owns the engine process and can distinguish
// crashes from timeouts from infrastructure kills.
func (h *Handler) HandleEngineError(w http.ResponseWriter, r *http.Request) {
	if !h.checkAuthOrOpen(r) {
		http.Error(w, `{"error":"unauthorized"}`, 401)
		return
	}
	var req struct {
		SessionID string `json:"session_id"`
		ErrorType string `json:"error_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SessionID == "" || req.ErrorType == "" {
		http.Error(w, `{"error":"invalid JSON — need session_id and error_type"}`, 400)
		return
	}
	if h.ErrorStore != nil {
		h.ErrorStore.Report(req.SessionID, req.ErrorType)
		slog.Info("coach reported engine error", "session", req.SessionID, "error_type", req.ErrorType)
	}
	jsonOK(w, map[string]any{"ok": true})
}

// ServeWS handles the persistent coach WebSocket connection.
// Replaces the old polling-based model: coaches connect once and receive
// commands (launch/kill) pushed by the MM, rather than polling for assignments.
func (h *Handler) ServeWS(w http.ResponseWriter, r *http.Request) {
	if !h.checkAuthOrOpen(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"arena.arsac.org", "localhost"},
	})
	if err != nil {
		slog.Error("coach ws accept", "err", err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	slog.Info("coach ws connected")
	if h.OnWSConnect != nil {
		h.OnWSConnect(conn)
	}

	// Read loop — block until connection drops.
	ctx := r.Context()
	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			slog.Info("coach ws disconnected", "err", err)
			if h.OnWSDisconnect != nil {
				h.OnWSDisconnect(conn)
			}
			return
		}
		var cm CoachMessage
		if err := json.Unmarshal(msg, &cm); err != nil {
			slog.Warn("coach ws bad JSON", "msg", string(msg)[:min(256, len(string(msg)))])
			continue
		}
		if h.OnCoachMessage != nil {
			h.OnCoachMessage(conn, cm)
		}
	}
}
