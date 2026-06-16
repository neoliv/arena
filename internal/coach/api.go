package coach

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/neoliv/arena/internal/db"
)

type Handler struct {
	DB            *db.DB
	Token         string
	Relay         *Relay
	ValidateToken func(string) bool
	matchmaker    MatchMakerFunc
	heartbeatHook func()
	ServerGen     string // random ID regenerated on server restart
	rateMu        sync.Mutex
	rateWindows   map[string][]time.Time
	launchMu      sync.Mutex // serializes both-ready check to prevent double OnBothReady
}

type MatchMakerFunc func(assignmentID int)

func (h *Handler) SetMatchMaker(fn MatchMakerFunc) { h.matchmaker = fn }
func (h *Handler) SetHeartbeatHook(fn func())      { h.heartbeatHook = fn }

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

type taskResp struct {
	Tasks []taskItem `json:"tasks"`
}

type taskItem struct {
	AssignmentID  int    `json:"assignment_id"`
	EngineName    string `json:"engine_name"`
	EngineVersion string `json:"engine_version"`
	TimeControl   string `json:"time_control"`
	NumGames      int    `json:"num_games"`
	SessionID     string `json:"session_id"`
	RelayPath     string `json:"relay_path"`
}

type taskStatusReq struct {
	CoachID      string `json:"coach_id"`
	AssignmentID int    `json:"assignment_id"`
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
}

func NewHandler(database *db.DB, token string, relay *Relay, validateToken func(string) bool, serverGen string) *Handler {
	return &Handler{
		DB: database, Token: token, Relay: relay, ValidateToken: validateToken,
		ServerGen: serverGen,
	}
}

func (h *Handler) checkAuth(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") { return false }
	t := strings.TrimPrefix(auth, "Bearer ")
	if t == "" { return false }
	if h.ValidateToken != nil && h.ValidateToken(t) { return true }
	if h.Token != "" && t == h.Token { return true }
	return false
}

func (h *Handler) checkAuthOrOpen(r *http.Request) bool {
	if h.Token == "" && h.ValidateToken == nil { return true }
	return h.checkAuth(r)
}

func (h *Handler) checkRate(coachID string) bool {
	h.rateMu.Lock()
	defer h.rateMu.Unlock()
	if h.rateWindows == nil { h.rateWindows = make(map[string][]time.Time) }
	now := time.Now()
	cutoff := now.Add(-30 * time.Second)
	recent := h.rateWindows[coachID]
	valid := recent[:0]
	for _, t := range recent {
		if t.After(cutoff) { valid = append(valid, t) }
	}
	if len(valid) >= 60 {
		return false
	}
	valid = append(valid, now)
	h.rateWindows[coachID] = valid
	return true
}

func genSessionID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
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

func (h *Handler) HandleRegister(w http.ResponseWriter, r *http.Request) {
	if !h.checkAuthOrOpen(r) { jsonErr(w, "unauthorized", http.StatusUnauthorized); return }
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req registerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid JSON", http.StatusBadRequest); return
	}
	if req.CoachID == "" { jsonErr(w, "coach_id required", http.StatusBadRequest); return }

	coachID, err := h.DB.UpsertCoach(req.CoachID, req.Version, req.Label, req.Resources.Cores, req.Resources.MemoryMB)
	if err != nil {
		slog.Error("register coach", "err", err)
		jsonErr(w, "db error", http.StatusInternalServerError); return
	}

	// Coach restarted — cancel all its pending assignments.
	h.DB.Exec(`UPDATE match_assignments SET status='failed', decline_reason='coach restarted'
		WHERE status IN ('pending','assigned','accepted','ready','in_progress','retry')
		AND (coach1_ai_id IN (SELECT id FROM coach_ais WHERE coach_id=?) OR coach2_ai_id IN (SELECT id FROM coach_ais WHERE coach_id=?))`, coachID, coachID)

	registered := 0
	for _, ai := range req.AIs {
		if registered >= 64 { break }
		if ai.Name == "" || ai.Version == "" { continue }
		cores := ai.ResourceCores
		if cores == 0 { cores = 1 }
		mem := ai.ResourceMemoryMB
		if mem == 0 { mem = 64 }
		maxInst := ai.MaxConcurrency
		if maxInst == 0 { maxInst = 1 }
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

func (h *Handler) HandleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if !h.checkAuthOrOpen(r) { jsonErr(w, "unauthorized", http.StatusUnauthorized); return }
	r.Body = http.MaxBytesReader(w, r.Body, 1<<18)
	var req heartbeatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid JSON", http.StatusBadRequest); return
	}
	if req.CoachID == "" { jsonErr(w, "coach_id required", http.StatusBadRequest); return }

	var coachID int
	var lastSession string
	if err := h.DB.QueryRow("SELECT id, COALESCE(session_id,'') FROM coaches WHERE coach_id=?", req.CoachID).Scan(&coachID, &lastSession); err != nil {
		jsonErr(w, "coach not registered", http.StatusNotFound); return
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
		jsonErr(w, "db error", http.StatusInternalServerError); return
	}
	jsonOK(w, map[string]any{"ok": true, "server_gen": h.ServerGen})
	if h.heartbeatHook != nil { h.heartbeatHook() }
}

func (h *Handler) HandleTasks(w http.ResponseWriter, r *http.Request) {
	if !h.checkAuthOrOpen(r) { jsonErr(w, "unauthorized", http.StatusUnauthorized); return }
	coachIDStr := r.URL.Query().Get("coach_id")
	if coachIDStr == "" { jsonErr(w, "coach_id required", http.StatusBadRequest); return }

	var coachID int
	if err := h.DB.QueryRow("SELECT id FROM coaches WHERE coach_id=?", coachIDStr).Scan(&coachID); err != nil {
		jsonErr(w, "coach not registered", http.StatusNotFound); return
	}

	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		ais, err := h.DB.GetAvailableAIs(coachID)
		if err != nil {
			jsonErr(w, "db error", http.StatusInternalServerError); return
		}
		var tasks []taskItem
		for _, ai := range ais {
			assignments, err := h.DB.GetPendingAssignments(ai.ID)
			if err != nil { continue }
			for _, a := range assignments {
				sessionID := a.Session1ID
				if a.Coach2AIID == ai.ID {
					sessionID = a.Session2ID
				}
				if sessionID == "" {
					sessionID = genSessionID()
					if a.Coach1AIID == ai.ID {
						h.DB.Exec("UPDATE match_assignments SET session1_id=?, status='assigned', assigned_at=? WHERE id=?", sessionID, time.Now().UTC().Format(time.RFC3339), a.ID)
					} else {
						h.DB.Exec("UPDATE match_assignments SET session2_id=?, status='assigned', assigned_at=? WHERE id=?", sessionID, time.Now().UTC().Format(time.RFC3339), a.ID)
					}
				}
				tasks = append(tasks, taskItem{
					AssignmentID:  a.ID,
					EngineName:    ai.EngineName,
					EngineVersion: ai.EngineVersion,
					TimeControl:   a.TimeControl,
					NumGames:      a.NumGames,
					SessionID:     sessionID,
					RelayPath:     "/api/relay/" + sessionID,
				})
			}
		}
		if len(tasks) > 0 {
			jsonOK(w, taskResp{Tasks: tasks})
			return
		}
		time.Sleep(2 * time.Second)
	}
	jsonOK(w, taskResp{Tasks: nil})
}

func (h *Handler) HandleTaskStatus(w http.ResponseWriter, r *http.Request) {
	if !h.checkAuthOrOpen(r) { jsonErr(w, "unauthorized", http.StatusUnauthorized); return }
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	idStr := r.PathValue("id")
	assignmentID, err := strconv.Atoi(idStr)
	if err != nil { jsonErr(w, "invalid id", http.StatusBadRequest); return }

	var req taskStatusReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid JSON", http.StatusBadRequest); return
	}
	req.AssignmentID = assignmentID

	status := req.Status
	reason := req.Reason
	if status == "" { jsonErr(w, "status required", http.StatusBadRequest); return }

	// Verify coach owns this assignment
	var ownerCoachID int
	if err := h.DB.QueryRow("SELECT c.id FROM coaches c JOIN coach_ais ca ON ca.coach_id=c.id WHERE (ca.id=(SELECT coach1_ai_id FROM match_assignments WHERE id=?) OR ca.id=(SELECT coach2_ai_id FROM match_assignments WHERE id=?)) AND c.coach_id=?", assignmentID, assignmentID, req.CoachID).Scan(&ownerCoachID); err != nil {
		jsonErr(w, "not your assignment", http.StatusForbidden); return
	}
	if status == "declined" {
		var retryCount int
		h.DB.QueryRow("SELECT retry_count FROM match_assignments WHERE id=?", assignmentID).Scan(&retryCount)
		delays := []int{5, 15, 30, 60, 120}
		delay := 480
		if retryCount < len(delays) {
			delay = delays[retryCount]
		}
		retryAfter := time.Now().Add(time.Duration(delay) * time.Second).UTC().Format(time.RFC3339)
		h.DB.Exec("UPDATE match_assignments SET status='retry', decline_reason=?, retry_after=? WHERE id=?",
			reason, retryAfter, assignmentID)
	} else if status == "ready" && h.matchmaker != nil {
		// Serialize the full check+update under a mutex. The
		// UpdateAssignmentStatus("ready") call would overwrite
		// 'in_progress' back to 'ready' if placed outside the lock,
		// causing duplicate match execution when both coaches
		// report ready simultaneously.
		h.launchMu.Lock()
		var curStatus string
		h.DB.QueryRow("SELECT status FROM match_assignments WHERE id=?", assignmentID).Scan(&curStatus)
		if curStatus != "in_progress" && curStatus != "completed" && curStatus != "failed" {
			h.DB.UpdateAssignmentStatus(assignmentID, "ready", "")
		}
		var a db.AssignmentRow
		row := h.DB.QueryRow("SELECT id, COALESCE(session1_id,''), COALESCE(session2_id,''), coach1_ai_id, coach2_ai_id, status FROM match_assignments WHERE id=?", assignmentID)
		if err := row.Scan(&a.ID, &a.Session1ID, &a.Session2ID, &a.Coach1AIID, &a.Coach2AIID, &a.Status); err == nil {
			if a.Session1ID != "" && a.Session2ID != "" && a.Status == "ready" {
				slog.Info("both sessions ready, starting match", "assignment", assignmentID)
				h.DB.Exec("UPDATE match_assignments SET status='in_progress' WHERE id=? AND status='ready'", assignmentID)
				h.launchMu.Unlock()
				h.matchmaker(assignmentID)
				jsonOK(w, map[string]string{"status": status})
				return
			}
		}
		h.launchMu.Unlock()
	} else {
		if err := h.DB.UpdateAssignmentStatus(assignmentID, status, reason); err != nil {
			slog.Error("task status", "err", err)
			jsonErr(w, "db error", http.StatusInternalServerError); return
		}
	}

	jsonOK(w, map[string]string{"status": status})
}
