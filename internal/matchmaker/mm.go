// Package matchmaker — game pairing, relay, and execution.
package matchmaker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/neoliv/arena/internal/coach"
	"github.com/neoliv/arena/internal/game"
	"github.com/neoliv/arena/internal/db"
	"github.com/neoliv/arena/internal/elo"
	"github.com/neoliv/arena/internal/web"
)

// ── MatchMaker ──────────────────────────────────────────────────────────

// MatchAssignment tracks a match in progress (in-memory only — reset on restart).
type MatchAssignment struct {
	ID           int64
	BlackEngine  string // "name:version"
	WhiteEngine  string
	TimeControl  string
	NumGames     int
	InProgressAt time.Time
}

// MatchMaker orchestrates engine pairing, relay-based game execution,
// and result storage. It owns the WantedList (in-memory engine registry
// and wanted-pair generation) and a dedicated SQLite writer goroutine.
type MatchMaker struct {
	DB         *db.DB
	Relay      *coach.Relay
	Wanted     *WantedList
	ErrorStore *coach.CoachErrorStore // coach-reported engine errors
	storeCh    chan GameResult        // game results → storage goroutine
	eloMu      sync.Mutex
	dbWriteMu  sync.Mutex // serializes all DB writes (SQLite single-writer)
	wakeup     chan struct{}
	quit       chan struct{}

	// In-memory transient state (reset on arena restart — coaches re-register).
	assignMu    sync.Mutex
	assignments map[int64]*MatchAssignment
	nextAssignID int64
}

// GameResult carries completed game data for storage.
type GameResult struct {
	Games                  []gameResult
	E1Name, E1Ver, E2Name, E2Ver string
}

func New(database *db.DB, relay *coach.Relay) *MatchMaker {
	storeCh := make(chan GameResult, 64)
	m := &MatchMaker{
		DB:          database,
		Relay:       relay,
		Wanted:      NewWantedList(database, storeCh),
		storeCh:     storeCh,
		wakeup:      make(chan struct{}, 1),
		quit:        make(chan struct{}),
		assignments: make(map[int64]*MatchAssignment),
	}
	// When a coach dials the relay, check if both sides of a pair are ready.
	relay.OnConnect = func(sessionID string) {
		start, p := m.Wanted.ClaimSide(sessionID)
		if start {
			slog.Info("both sides connected, starting match", "pair", p.ID,
				"black", p.BlackEngine, "white", p.WhiteEngine)
			go m.executeConnectedPair(p)
		}
	}

	go m.storageLoop()
	go m.tickLoop()
	return m
}

// ── Storage goroutine (single SQLite writer) ────────────────────────────

func (m *MatchMaker) storageLoop() {
	for gr := range m.storeCh {
		m.storeMatch(gr)
	}
}

func (m *MatchMaker) storeMatch(gr GameResult) {
	m.dbWriteMu.Lock()
	defer m.dbWriteMu.Unlock()
	e1ID := m.resolveEngine(gr.E1Name, gr.E1Ver)
	e2ID := m.resolveEngine(gr.E2Name, gr.E2Ver)
	if e1ID == 0 || e2ID == 0 {
		return
	}

	tc, _ := json.Marshal(map[string]interface{}{"type": "total", "seconds": 30})
	res, err := m.DB.Exec(`INSERT INTO matches (engine1_id, engine2_id, time_control, runner_id, total_games)
		VALUES (?,?,?,?,?)`, e1ID, e2ID, tc, "matchmaker", len(gr.Games))
	if err != nil || res == nil {
		slog.Error("storeMatch: insert matches failed", "err", err)
		return
	}
	matchID64, _ := res.LastInsertId()
	matchID := int(matchID64)

	wins1, wins2, draws := 0, 0, 0
	for i, g := range gr.Games {
		var blackID, whiteID int
		if i%2 == 0 {
			blackID, whiteID = e1ID, e2ID
		} else {
			blackID, whiteID = e2ID, e1ID
		}

		disc := 0
		if g.Disconnect {
			disc = 1
		}
		gres, err := m.DB.Exec(`INSERT INTO games (match_id, game_number, black_id, white_id, result, final_score, opening_line, black_time_s, white_time_s, black_nodes, white_nodes, black_depth, white_depth, disconnect, error_code)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			matchID, i+1, blackID, whiteID, g.Result, g.FinalScore, g.OpeningLine,
			g.BlackTimeS, g.WhiteTimeS, g.BlackNodes, g.WhiteNodes, g.BlackDepth, g.WhiteDepth, disc, g.ErrorCode)
		if err != nil || gres == nil {
			slog.Error("storeMatch: insert game failed", "err", err, "match", matchID, "game", i+1)
			continue
		}
		gameID64, _ := gres.LastInsertId()
		gameID := int(gameID64)

		// Store per-move data for game detail charts.
		for mn, mv := range g.Moves {
			m.DB.Exec(`INSERT INTO game_moves (game_id, move_num, side, move, nodes, depth, time_ms, score)
				VALUES (?,?,?,?,?,?,?,?)`,
				gameID, mn+1, mv.Side, mv.Move, mv.Nodes, mv.Depth, mv.TimeMs, mv.Score)
		}

		if g.Result == "1-0" {
			if blackID == e1ID {
				wins1++
			} else {
				wins2++
			}
		}
		if g.Result == "0-1" {
			if whiteID == e1ID {
				wins1++
			} else {
				wins2++
			}
		}
		if g.Result == "1/2" {
			draws++
		}

		// Elo update (skip disconnected games)
		if !g.Disconnect {
			m.updateElo(blackID, whiteID, matchID, g)
		}
	}
	m.DB.Exec("UPDATE matches SET wins_1=?, wins_2=?, draws=? WHERE id=?", wins1, wins2, draws, matchID)
}

func (m *MatchMaker) resolveEngine(name, ver string) int {
	m.DB.Exec(`INSERT OR IGNORE INTO engines (name,version,created) VALUES (?,?,unixepoch())`, name, ver)
	var id int
	m.DB.QueryRow(`SELECT id FROM engines WHERE name=? AND version=?`, name, ver).Scan(&id)
	return id
}

func (m *MatchMaker) updateElo(blackID, whiteID, matchID int, g gameResult) {
	m.eloMu.Lock()
	defer m.eloMu.Unlock()
	var rB, rW float64
	var gB, gW int
	m.DB.QueryRow(`SELECT COALESCE((SELECT rating_after FROM elo_history WHERE engine_id=? ORDER BY id DESC LIMIT 1), 1500.0)`, blackID).Scan(&rB)
	m.DB.QueryRow(`SELECT COALESCE((SELECT rating_after FROM elo_history WHERE engine_id=? ORDER BY id DESC LIMIT 1), 1500.0)`, whiteID).Scan(&rW)
	m.DB.QueryRow(`SELECT COALESCE((SELECT COUNT(*) FROM elo_history WHERE engine_id=?), 0)`, blackID).Scan(&gB)
	m.DB.QueryRow(`SELECT COALESCE((SELECT COUNT(*) FROM elo_history WHERE engine_id=?), 0)`, whiteID).Scan(&gW)

	var sB float64
	switch g.Result {
	case "1-0":
		sB = 1
	case "0-1":
		sB = 0
	default:
		sB = 0.5
	}
	nB, _ := elo.Update(rB, rW, sB, gB)
	nW, _ := elo.Update(rW, rB, 1-sB, gW)

	w1, l1, d1 := 0, 0, 0
	switch sB {
	case 1:
		w1 = 1
	case 0:
		l1 = 1
	default:
		d1 = 1
	}
	m.DB.Exec(`INSERT INTO elo_history (engine_id, opponent_id, match_id, rating_before, rating_after, games, wins, losses, draws)
		VALUES (?,?,?,?,?,1,?,?,?)`, blackID, whiteID, matchID, rB, nB, w1, l1, d1)
	w2, l2, d2 := l1, w1, d1
	m.DB.Exec(`INSERT INTO elo_history (engine_id, opponent_id, match_id, rating_before, rating_after, games, wins, losses, draws)
		VALUES (?,?,?,?,?,1,?,?,?)`, whiteID, blackID, matchID, rW, nW, w2, l2, d2)
}

// ── Tick loop ───────────────────────────────────────────────────────────

func (m *MatchMaker) tickLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.quit:
			return
		case <-m.wakeup:
		case <-ticker.C:
		}
		m.Wanted.Tick()

		// Reap lone connections (one side connected, opponent no-show).
		// Clean up the relay session for the connected side so the coach
		// frees its engine for other assignments.
		m.reapLoneConnections(15 * time.Second)
		m.Wanted.ReapStale(15 * time.Second)
	}
}

// reapLoneConnections finds pairs where only one side connected and
// cleans the relay session + records a decline for the no-show engine.
func (m *MatchMaker) reapLoneConnections(timeout time.Duration) {
	m.Wanted.mu.Lock()
	defer m.Wanted.mu.Unlock()
	now := time.Now()
	for _, p := range m.Wanted.pairs {
		if p.Status != "pending" {
			continue
		}
		if p.BlackConnected && !p.WhiteConnected && now.Sub(p.BlackConnectedAt) > timeout {
			slog.Info("reaping lone black connection", "pair", p.ID)
			m.Relay.Cleanup(p.SessionID + "-b")
			p.BlackConnected = false
			// Decline BOTH sides using the stored coach IDs so only the
			// coaches that were actually assigned are penalized.
			if p.BlackCoachID != "" {
				m.Wanted.declines[declineKey(p.BlackCoachID, p.BlackEngine)] = now
			}
			if p.WhiteCoachID != "" {
				m.Wanted.declines[declineKey(p.WhiteCoachID, p.WhiteEngine)] = now
			}
		}
		if p.WhiteConnected && !p.BlackConnected && now.Sub(p.WhiteConnectedAt) > timeout {
			slog.Info("reaping lone white connection", "pair", p.ID)
			m.Relay.Cleanup(p.SessionID + "-w")
			p.WhiteConnected = false
			// Decline BOTH sides using the stored coach IDs.
			if p.BlackCoachID != "" {
				m.Wanted.declines[declineKey(p.BlackCoachID, p.BlackEngine)] = now
			}
			if p.WhiteCoachID != "" {
				m.Wanted.declines[declineKey(p.WhiteCoachID, p.WhiteEngine)] = now
			}
		}
	}
}

// executeConnectedPair is called via the relay OnConnect callback when
// both sides of a pair have dialed in. Both relay streams are already
// available — no waiting, no idle time.
func (m *MatchMaker) executeConnectedPair(p *wantedPair) {
	blackSid := p.SessionID + "-b"
	whiteSid := p.SessionID + "-w"

	// Wait for both streams. Even though OnConnect already fired,
	// the relay may need a moment to set up the stream channels.
	blackStream, err := m.Relay.WaitForStream(blackSid, 10)
	if err != nil {
		slog.Error("match: black stream gone", "pair", p.ID, "err", err)
		m.failPair(p, "black stream gone")
		return
	}
	whiteStream, err := m.Relay.WaitForStream(whiteSid, 10)
	if err != nil {
		slog.Error("match: white stream gone", "pair", p.ID, "err", err)
		m.failPair(p, "white stream gone")
		return
	}

	var gameTimeSec float64 = 30
	if n, _ := fmt.Sscanf(p.TimeControl, "%fs", &gameTimeSec); n != 1 {
		slog.Warn("unparseable time control, using default", "time_control", p.TimeControl, "default_s", gameTimeSec)
	}

	slog.Info("both streams ready, executing match", "pair", p.ID)

	// Resolve engine IDs for Elo/storage (called on game completion).
	bParts := splitEngineKey(p.BlackEngine)
	wParts := splitEngineKey(p.WhiteEngine)

	// In-memory match assignment (appears under "In Progress" on the dashboard).
	m.assignMu.Lock()
	m.nextAssignID++
	assignID := m.nextAssignID
	m.assignments[assignID] = &MatchAssignment{
		ID:           assignID,
		BlackEngine:  p.BlackEngine,
		WhiteEngine:  p.WhiteEngine,
		TimeControl:  p.TimeControl,
		NumGames:     2,
		InProgressAt: time.Now(),
	}
	m.assignMu.Unlock()

	ctx := context.Background()
	games := playGames(ctx, blackStream, whiteStream, 2, gameTimeSec, int(assignID))

	// Post-process: for wsSend failures (genmove or play), consult the
	// coach error store. The coach is authoritative — if it didn't report
	// an error, the failure is infrastructure (engine blameless).
	for i := range games {
		if games[i].ErrorCode != game.ErrCrash {
			continue
		}
		// Check both sessions — play failures can happen on either stream.
		var coachErr int8
		if m.ErrorStore != nil {
			coachErr = m.ErrorStore.Get(blackSid)
			if coachErr == 0 {
				coachErr = m.ErrorStore.Get(whiteSid)
			}
		}
		if coachErr != 0 {
			games[i].ErrorCode = coachErr
			slog.Info("coach error verdict applied", "error_code", coachErr)
		} else {
			// No coach verdict — infrastructure, engine blameless.
			slog.Warn("wsSend failure without coach verdict — treating as infrastructure")
			games[i].ErrorCode = game.ErrNone
			games[i].Disconnect = true
		}
	}

	slog.Info("matchmaker match played", "pair", p.ID, "games", len(games))
	m.storeCh <- GameResult{
		Games:  games,
		E1Name: bParts[0], E1Ver: bParts[1],
		E2Name: wParts[0], E2Ver: wParts[1],
	}

	// Mark assignment completed so it disappears from "In Progress".
	if assignID > 0 {
		m.assignMu.Lock()
		delete(m.assignments, assignID)
		m.assignMu.Unlock()
	}

	// Cleanup relay sessions and error store entries
	m.Relay.Cleanup(blackSid)
	m.Relay.Cleanup(whiteSid)
	if m.ErrorStore != nil {
		m.ErrorStore.Cleanup(blackSid)
		m.ErrorStore.Cleanup(whiteSid)
	}
}

func (m *MatchMaker) failPair(p *wantedPair, reason string) {
	slog.Error("match failed", "pair", p.ID, "reason", reason)
	m.Wanted.ResetPair(p.ID)
	m.Relay.Cleanup(p.SessionID + "-b")
	m.Relay.Cleanup(p.SessionID + "-w")
}

func splitEngineKey(key string) []string {
	// key is "name:version" — split on LAST colon (version may contain nothing, name shouldn't)
	idx := strings.LastIndex(key, ":")
	if idx < 0 {
		return []string{key, "unknown"}
	}
	return []string{key[:idx], key[idx+1:]}
}

// ── Execute match (called from relay or external trigger) ───────────────

func (m *MatchMaker) executeMatch(blackStream, whiteStream coach.Stream, gameTimeSec float64) {
	ctx := context.Background()
	games := playGames(ctx, blackStream, whiteStream, 2, gameTimeSec, 0)
	slog.Info("matchmaker match played", "games", len(games))
	m.storeCh <- GameResult{Games: games}
}

// ── HTTP handlers ───────────────────────────────────────────────────────

// HandleStatus returns current matchmaker state for the dashboard.
// EngineStatus returns a snapshot of all registered engines with their
// availability state. Used by the web dashboard (Players page).
func (m *MatchMaker) EngineStatus() []web.EngineStatus {
	m.Wanted.mu.RLock()
	defer m.Wanted.mu.RUnlock()
	var out []web.EngineStatus
	for _, c := range m.Wanted.coaches {
		for _, e := range c.Engines {
			out = append(out, web.EngineStatus{
				Name:              e.Name,
				Version:           e.Version,
				CoachID:           c.ID,
				Available:         e.Available,
				UnavailableReason: e.UnavailableReason,
			})
		}
	}
	return out
}

// CoachStatus returns a snapshot of all registered coaches with their declared
// resources and per-engine instance counts. Used by the web Coaches page.
func (m *MatchMaker) CoachStatus() []web.CoachStatus {
	m.Wanted.mu.RLock()
	defer m.Wanted.mu.RUnlock()
	var out []web.CoachStatus
	now := time.Now()
	for _, c := range m.Wanted.coaches {
		if now.Sub(c.LastSeen) > 90*time.Second {
			continue // coach appears offline
		}
		cs := web.CoachStatus{
			ID:         c.ID,
			SessionID:  c.SessionID,
			CoresTotal: c.CoresTotal,
			LastSeen:   c.LastSeen,
		}
		for _, e := range c.Engines {
			cs.CoresUsed += e.Cores * e.InstancesRunning
			cs.MemUsed += e.MemoryMB * e.InstancesRunning
		}
		out = append(out, cs)
	}
	return out
}

// ActiveAssignments returns matches currently in progress. Used by the
// web Games page "In Progress" section.
func (m *MatchMaker) ActiveAssignments() []web.AssignmentStatus {
	m.assignMu.Lock()
	defer m.assignMu.Unlock()
	var out []web.AssignmentStatus
	for _, a := range m.assignments {
		out = append(out, web.AssignmentStatus{
			ID:           a.ID,
			BlackEngine:  a.BlackEngine,
			WhiteEngine:  a.WhiteEngine,
			TimeControl:  a.TimeControl,
			NumGames:     a.NumGames,
			InProgressAt: a.InProgressAt,
		})
	}
	return out
}

// OnCoachHeartbeat updates in-memory coach state from a heartbeat.
// Replaces the old DB-backed coach_ais.instances_running update.
// Returns true if the session ID changed (coach restarted → pairs need cleanup).
func (m *MatchMaker) OnCoachHeartbeat(coachID string, sessionID string, aiUpdates map[string]int) bool {
	m.Wanted.mu.Lock()
	defer m.Wanted.mu.Unlock()
	c, ok := m.Wanted.coaches[coachID]
	if !ok {
		return false
	}
	c.LastSeen = time.Now()
	sessionChanged := false
	if sessionID != "" && sessionID != c.SessionID {
		sessionChanged = true
		c.SessionID = sessionID
	}
	// Zero all first: engines not in the heartbeat have 0 instances.
	for _, e := range c.Engines {
		e.InstancesRunning = 0
	}
	for key, count := range aiUpdates {
		if e, ok := c.Engines[key]; ok {
			e.InstancesRunning = count
		}
	}
	return sessionChanged
}

var _ = web.EngineStatus{} // compile-time check

func (m *MatchMaker) HandleStatus(w http.ResponseWriter, r *http.Request) {
	m.Wanted.mu.RLock()
	defer m.Wanted.mu.RUnlock()
	fmt.Fprintf(w, `{"coaches": %d, "pairs": %d}`, len(m.Wanted.coaches), len(m.Wanted.pairs))
}

// HandleDebug dumps the full state for debugging pairing issues.
func (m *MatchMaker) HandleDebug(w http.ResponseWriter, r *http.Request) {
	m.Wanted.mu.RLock()
	defer m.Wanted.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"coaches": %d, "pairs": %d`, len(m.Wanted.coaches), len(m.Wanted.pairs))
	// Dump coach engine keys
	for coachID, c := range m.Wanted.coaches {
		fmt.Fprintf(w, `, "coach_%s": {`, coachID)
		fmt.Fprintf(w, `"engines": [`)
		first := true
		for key, e := range c.Engines {
			if !first { fmt.Fprintf(w, `,`) }
			first = false
			fmt.Fprintf(w, `{"key": %q, "available": %v, "max_inst": %d, "inst_running": %d}`, key, e.Available, e.MaxInstances, e.InstancesRunning)
		}
		fmt.Fprintf(w, `]}`)
	}
	// Dump first 3 pairs
	fmt.Fprintf(w, `, "first_pairs": [`)
	for i, p := range m.Wanted.pairs {
		if i >= 3 { break }
		if i > 0 { fmt.Fprintf(w, `,`) }
		fmt.Fprintf(w, `{"id": %q, "status": %q, "b_key": %q, "w_key": %q, "b_connected": %v, "w_connected": %v, "session_id": %q, "b_coach": %q, "w_coach": %q}`,
			p.ID, p.Status, p.BlackEngine, p.WhiteEngine, p.BlackConnected, p.WhiteConnected, p.SessionID, p.BlackCoachID, p.WhiteCoachID)
	}
	fmt.Fprintf(w, `]}`)
}

// HandleRegister accepts engine registrations from coaches and populates
// the in-memory WantedList.
func (m *MatchMaker) HandleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CoachID    string        `json:"coach_id"`
		CoresTotal int           `json:"cores_total"`
		Engines    []EngineEntry `json:"engines"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, 400)
		return
	}
	if req.CoachID == "" {
		http.Error(w, `{"error":"coach_id required"}`, 400)
		return
	}
	m.Wanted.RegisterCoach(req.CoachID, req.CoresTotal, req.Engines)
	slog.Info("coach registered in matchmaker", "coach", req.CoachID, "engines", len(req.Engines))
	// Immediately rebuild pairs so the coach does not wait 15s for the next Tick.
	go m.Wanted.Tick()
	w.Write([]byte(`{"status":"ok"}`))
}

// HandlePoll returns pending assignments for a coach's engines.
// The coach calls this in a loop (every ~5s). Liveness is tracked
// via the poll itself — no separate heartbeat needed.
func (m *MatchMaker) HandlePoll(w http.ResponseWriter, r *http.Request) {
	coachID := r.URL.Query().Get("coach")
	if coachID == "" {
		http.Error(w, `{"error":"coach required"}`, 400)
		return
	}
	if len(coachID) > 128 {
		http.Error(w, `{"error":"coach ID too long"}`, 400)
		return
	}
	m.Wanted.Heartbeat(coachID, 0, 0)
	n := 3
	if ns := r.URL.Query().Get("n"); ns != "" {
		if _, err := fmt.Sscanf(ns, "%d", &n); err != nil {
			n = 3
		}
		if n > 16 { n = 16 }
	}
	assignments := m.Wanted.PollAssignments(coachID, n)
	if len(assignments) == 0 {
		m.Wanted.mu.RLock()
		coachCount := len(m.Wanted.coaches)
		pairCount := len(m.Wanted.pairs)
		_, coachExists := m.Wanted.coaches[coachID]
		m.Wanted.mu.RUnlock()
		slog.Warn("HandlePoll returned empty", "coach", coachID, "coach_exists", coachExists,
			"coaches_total", coachCount, "pairs_total", pairCount, "requested_n", n)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"assignments": assignments})
}

// HandleComplete receives game results after a match finishes.
func (m *MatchMaker) HandleComplete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string       `json:"session_id"`
		Games     []gameResult `json:"games"`
		E1Name    string       `json:"e1_name"`
		E1Ver     string       `json:"e1_ver"`
		E2Name    string       `json:"e2_name"`
		E2Ver     string       `json:"e2_ver"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, 400)
		return
	}
	if len(req.Games) > 0 {
		m.storeCh <- GameResult{
			Games:  req.Games,
			E1Name: req.E1Name, E1Ver: req.E1Ver,
			E2Name: req.E2Name, E2Ver: req.E2Ver,
		}
	}
	w.Write([]byte(`{"status":"ok"}`))
}
