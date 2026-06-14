package coach

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/neoliv/arena/internal/db"
)

type Handler struct {
	DB            *db.DB
	Token         string
	Relay         *Relay
	ValidateToken func(string) bool
	matchmaker    MatchMakerFunc
	ServerGen     string // random ID regenerated on server restart
}

type MatchMakerFunc func(assignmentID int)

func (h *Handler) SetMatchMaker(fn MatchMakerFunc) { h.matchmaker = fn }

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
	CoachID     string              `json:"coach_id"`
	Token       string              `json:"token"`
	AIsAvailable []heartbeatAI      `json:"ais_available"`
	Resources   *heartbeatResources `json:"resources"`
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

	registered := 0
	for _, ai := range req.AIs {
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
		// Also upsert into engines table with changelog info
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
	var req heartbeatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid JSON", http.StatusBadRequest); return
	}
	if req.CoachID == "" { jsonErr(w, "coach_id required", http.StatusBadRequest); return }

	var coachID int
	if err := h.DB.QueryRow("SELECT id FROM coaches WHERE coach_id=?", req.CoachID).Scan(&coachID); err != nil {
		jsonErr(w, "coach not registered", http.StatusNotFound); return
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

	if err := h.DB.UpdateAssignmentStatus(assignmentID, status, reason); err != nil {
		slog.Error("task status", "err", err)
		jsonErr(w, "db error", http.StatusInternalServerError); return
	}

	if status == "ready" && h.matchmaker != nil {
		var a db.AssignmentRow
		row := h.DB.QueryRow("SELECT id, COALESCE(session1_id,''), COALESCE(session2_id,''), coach1_ai_id, coach2_ai_id FROM match_assignments WHERE id=?", assignmentID)
		if err := row.Scan(&a.ID, &a.Session1ID, &a.Session2ID, &a.Coach1AIID, &a.Coach2AIID); err == nil {
			if a.Session1ID != "" && a.Session2ID != "" {
				h.matchmaker(assignmentID)
			}
		}
	}

	jsonOK(w, map[string]string{"status": status})
}
