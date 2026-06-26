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
	"github.com/neoliv/arena/internal/db"
	"github.com/neoliv/arena/internal/elo"
	"github.com/neoliv/arena/internal/web"
)

// ── MatchMaker ──────────────────────────────────────────────────────────

// MatchMaker orchestrates engine pairing, relay-based game execution,
// and result storage. It owns the WantedList (in-memory engine registry
// and wanted-pair generation) and a dedicated SQLite writer goroutine.
type MatchMaker struct {
	DB        *db.DB
	Relay     *coach.Relay
	Wanted    *WantedList
	storeCh   chan GameResult // game results → storage goroutine
	eloMu     sync.Mutex
	dbWriteMu sync.Mutex // serializes all DB writes (SQLite single-writer)
	wakeup    chan struct{}
	quit      chan struct{}
}

// GameResult carries completed game data for storage.
type GameResult struct {
	Games                  []gameResult
	E1Name, E1Ver, E2Name, E2Ver string
}

func New(database *db.DB, relay *coach.Relay) *MatchMaker {
	storeCh := make(chan GameResult, 64)
	m := &MatchMaker{
		DB:      database,
		Relay:   relay,
		Wanted:  NewWantedList(database, storeCh),
		storeCh: storeCh,
		wakeup:  make(chan struct{}, 1),
		quit:    make(chan struct{}),
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
		gres, err := m.DB.Exec(`INSERT INTO games (match_id, game_number, black_id, white_id, result, final_score, opening_line, black_time_s, white_time_s, black_nodes, white_nodes, black_depth, white_depth, disconnect)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			matchID, i+1, blackID, whiteID, g.Result, g.FinalScore, g.OpeningLine,
			g.BlackTimeS, g.WhiteTimeS, g.BlackNodes, g.WhiteNodes, g.BlackDepth, g.WhiteDepth, disc)
		if err != nil || gres == nil {
			slog.Error("storeMatch: insert game failed", "err", err, "match", matchID, "game", i+1)
			continue
		}
		gameID64, _ := gres.LastInsertId()
		gameID := int(gameID64)

		// Store per-move data for game detail charts.
		for mn, mv := range g.Moves {
			nps := int64(0)
			if mv.TimeMs > 0 {
				nps = int64(float64(mv.Nodes) / (mv.TimeMs / 1000.0))
			}
			m.DB.Exec(`INSERT INTO game_moves (game_id, move_num, side, move, nodes, depth, time_ms, score, nps)
				VALUES (?,?,?,?,?,?,?,?,?)`,
				gameID, mn+1, mv.Side, mv.Move, mv.Nodes, mv.Depth, mv.TimeMs, mv.Score, nps)
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
	m.DB.Exec(`INSERT OR IGNORE INTO engines (name,version,created) VALUES (?,?,datetime('now'))`, name, ver)
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
			// Decline BOTH sides: the no-show white AND the connected black.
			// Otherwise the same engine gets re-offered the same pair on every poll → cycle.
			for coachID, c := range m.Wanted.coaches {
				if _, ok := c.Engines[p.WhiteEngine]; ok {
					m.Wanted.declines[declineKey(coachID, p.WhiteEngine)] = now
				}
				if _, ok := c.Engines[p.BlackEngine]; ok {
					m.Wanted.declines[declineKey(coachID, p.BlackEngine)] = now
				}
			}
		}
		if p.WhiteConnected && !p.BlackConnected && now.Sub(p.WhiteConnectedAt) > timeout {
			slog.Info("reaping lone white connection", "pair", p.ID)
			m.Relay.Cleanup(p.SessionID + "-w")
			p.WhiteConnected = false
			// Decline BOTH sides: the no-show black AND the connected white.
			for coachID, c := range m.Wanted.coaches {
				if _, ok := c.Engines[p.BlackEngine]; ok {
					m.Wanted.declines[declineKey(coachID, p.BlackEngine)] = now
				}
				if _, ok := c.Engines[p.WhiteEngine]; ok {
					m.Wanted.declines[declineKey(coachID, p.WhiteEngine)] = now
				}
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

	// Both streams are already connected (OnConnect fired for each).
	blackStream, err := m.Relay.WaitForStream(blackSid, 1)
	if err != nil {
		slog.Error("match: black stream gone", "pair", p.ID, "err", err)
		m.failPair(p, "black stream gone")
		return
	}
	whiteStream, err := m.Relay.WaitForStream(whiteSid, 1)
	if err != nil {
		slog.Error("match: white stream gone", "pair", p.ID, "err", err)
		m.failPair(p, "white stream gone")
		return
	}

	var gameTimeSec float64 = 30
	fmt.Sscanf(p.TimeControl, "%fs", &gameTimeSec)

	slog.Info("both streams ready, executing match", "pair", p.ID)

	// Resolve engine IDs and create an in-progress row so the web
	// dashboard shows the match under "In Progress".
	bParts := splitEngineKey(p.BlackEngine)
	wParts := splitEngineKey(p.WhiteEngine)
	m.dbWriteMu.Lock()
	bID := m.resolveEngine(bParts[0], bParts[1])
	wID := m.resolveEngine(wParts[0], wParts[1])

	// Look up coach_ais IDs (needed for the legacy match_assignments FK).
	var bCAID, wCAID int64
	m.DB.QueryRow(`SELECT ca.id FROM coach_ais ca WHERE ca.engine_name=? AND ca.engine_version=? LIMIT 1`,
		bParts[0], bParts[1]).Scan(&bCAID)
	m.DB.QueryRow(`SELECT ca.id FROM coach_ais ca WHERE ca.engine_name=? AND ca.engine_version=? LIMIT 1`,
		wParts[0], wParts[1]).Scan(&wCAID)

	tc, _ := json.Marshal(map[string]interface{}{"type": "total", "seconds": gameTimeSec})
	res, err := m.DB.Exec(`INSERT INTO match_assignments (engine1_id, engine2_id, coach1_ai_id, coach2_ai_id, time_control, num_games, status, in_progress_at)
		VALUES (?,?,?,?,?,2,'in_progress',datetime('now'))`, bID, wID, bCAID, wCAID, tc)
	if err != nil {
		slog.Warn("match_assignments insert failed", "err", err)
	}
	var assignID int64
	if res != nil { assignID, _ = res.LastInsertId() }
	m.dbWriteMu.Unlock()

	ctx := context.Background()
	games := playGames(ctx, blackStream, whiteStream, 2, gameTimeSec, int(assignID))

	slog.Info("matchmaker match played", "pair", p.ID, "games", len(games))
	m.storeCh <- GameResult{
		Games:  games,
		E1Name: bParts[0], E1Ver: bParts[1],
		E2Name: wParts[0], E2Ver: wParts[1],
	}

	// Mark assignment completed so it disappears from "In Progress".
	if assignID > 0 {
		m.dbWriteMu.Lock()
		m.DB.Exec(`UPDATE match_assignments SET status='completed', completed_at=datetime('now') WHERE id=?`, assignID)
		m.dbWriteMu.Unlock()
	}

	// Cleanup relay sessions
	m.Relay.Cleanup(blackSid)
	m.Relay.Cleanup(whiteSid)
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

var _ = web.EngineStatus{} // compile-time check

func (m *MatchMaker) HandleStatus(w http.ResponseWriter, r *http.Request) {
	m.Wanted.mu.RLock()
	defer m.Wanted.mu.RUnlock()
	fmt.Fprintf(w, `{"coaches": %d, "pairs": %d}`, len(m.Wanted.coaches), len(m.Wanted.pairs))
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
	m.Wanted.Heartbeat(coachID, 0, 0)
	n := 3
	if ns := r.URL.Query().Get("n"); ns != "" {
		fmt.Sscanf(ns, "%d", &n)
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
