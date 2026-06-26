package coach

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/neoliv/arena/internal/db"
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
	rateMu        sync.Mutex
	rateWindows   map[string][]time.Time
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

// HandleRegister persists coach and engine info to the database.
// The matchmaker's in-memory registry is populated separately via
// POST /api/matchmaker/register (called by the coach after this).
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

	coachID, err := h.DB.UpsertCoach(req.CoachID, req.Version, req.Label, req.Resources.Cores, req.Resources.MemoryMB)
	if err != nil {
		slog.Error("register coach", "err", err)
		jsonErr(w, "db error", http.StatusInternalServerError)
		return
	}

	// Coach restarted — cancel all its pending assignments.
	h.DB.Exec(`UPDATE match_assignments SET status='failed', decline_reason='coach restarted'
		WHERE status IN ('pending','assigned','accepted','ready','in_progress','retry')
		AND (coach1_ai_id IN (SELECT id FROM coach_ais WHERE coach_id=?) OR coach2_ai_id IN (SELECT id FROM coach_ais WHERE coach_id=?))`, coachID, coachID)

	registered := 0
	for _, ai := range req.AIs {
		if registered >= 64 {
			break
		}
		if ai.Name == "" || ai.Version == "" {
			continue
		}
		cores := ai.ResourceCores
		if cores == 0 {
			cores = 1
		}
		mem := ai.ResourceMemoryMB
		if mem == 0 {
			mem = 64
		}
		maxInst := ai.MaxConcurrency
		if maxInst == 0 {
			maxInst = 1
		}
		_, err := h.DB.UpsertCoachAI(coachID, ai.Name, ai.Version, ai.Created, ai.ChangelogShort, ai.ChangelogFull, cores, mem, maxInst, ai.RunCmd, ai.BuildCmd, ai.EngineID, ai.EngineManifest)
		if err != nil {
			slog.Error("register coach ai", "name", ai.Name, "version", ai.Version, "err", err)
			continue
		}
		_, err = h.DB.Exec(`INSERT INTO engines (name, version, created, changelog_short, changelog_full, engine_id, engine_manifest) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(name, version) DO UPDATE SET created=excluded.created, changelog_short=excluded.changelog_short, changelog_full=excluded.changelog_full, engine_id=excluded.engine_id, engine_manifest=excluded.engine_manifest`,
			ai.Name, ai.Version, ai.Created, ai.ChangelogShort, ai.ChangelogFull, ai.EngineID, ai.EngineManifest)
		if err != nil {
			slog.Error("upsert engine", "name", ai.Name, "version", ai.Version, "err", err)
		}
		registered++
	}
	jsonOK(w, map[string]any{"status": "registered", "ais_registered": registered})
}

// HandleHeartbeat updates coach liveness and running AI counts in the database.
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

	var coachID int
	var lastSession string
	if err := h.DB.QueryRow("SELECT id, COALESCE(session_id,'') FROM coaches WHERE coach_id=?", req.CoachID).Scan(&coachID, &lastSession); err != nil {
		jsonErr(w, "coach not registered", http.StatusNotFound)
		return
	}

	// Coach restarted (new session) — clean all its stale assignments.
	if req.SessionID != "" && req.SessionID != lastSession {
		h.DB.Exec(`UPDATE match_assignments SET status='failed', decline_reason='coach restarted'
			WHERE status IN ('pending','assigned','accepted','ready','in_progress','retry')
			AND (coach1_ai_id IN (SELECT id FROM coach_ais WHERE coach_id=?)
			  OR coach2_ai_id IN (SELECT id FROM coach_ais WHERE coach_id=?))`,
			coachID, coachID)
		h.DB.Exec("UPDATE coaches SET session_id=? WHERE id=?", req.SessionID, coachID)
	}

	aiUpdates := make(map[string]int)
	for _, ai := range req.AIsAvailable {
		key := ai.Name + ":" + ai.Version
		aiUpdates[key] = ai.CurrentMatches
	}
	if err := h.DB.UpdateCoachHeartbeat(coachID, aiUpdates); err != nil {
		slog.Error("heartbeat", "err", err)
		jsonErr(w, "db error", http.StatusInternalServerError)
		return
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
