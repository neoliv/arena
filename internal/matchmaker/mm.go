// Package matchmaker schedules and executes distributed matches.
package matchmaker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/neoliv/arena/internal/coach"
	"github.com/neoliv/arena/internal/db"
)

// TimeControl configures a time control tier.
type TimeControl struct {
	Seconds int     `yaml:"seconds"`
	Weight  float64 `yaml:"weight"`
	Label   string  `yaml:"label"`
}

// Config holds matchmaker configuration.
type Config struct {
	ArenaURL         string        `yaml:"arena_url"`
	Token            string        `yaml:"token"`
	TimeControls     []TimeControl `yaml:"time_controls"`
	ProvisionalGames int           `yaml:"provisional_games"`
	GamesPerMatch    int           `yaml:"games_per_match"`
	TickInterval     time.Duration `yaml:"tick_interval"`
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		TimeControls: []TimeControl{
			{Seconds: 30, Weight: 1.0, Label: "blitz"},
		},
		ProvisionalGames: 20,
		GamesPerMatch:    2,
		TickInterval:     60 * time.Second,
	}
}

// MatchMaker schedules matches and executes them.
type MatchMaker struct {
	DB     *db.DB
	Relay  interface {
		WaitForStream(sessionID string, timeoutSec int) (coach.Stream, error)
		Cleanup(sessionID string)
	}
	Config Config
	ticker *time.Ticker
}

// New creates a new MatchMaker.
func New(database *db.DB, relay interface {
	WaitForStream(sessionID string, timeoutSec int) (coach.Stream, error)
	Cleanup(sessionID string)
}, cfg Config) *MatchMaker {
	interval := cfg.TickInterval
	if interval == 0 { interval = 60 * time.Second }
	if s := os.Getenv("MATCHMAKER_TICK"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d >= time.Second {
			interval = d
		}
	}
	if cfg.Token == "" { cfg.Token = os.Getenv("ARENA_TOKEN") }
	return &MatchMaker{
		DB:     database,
		Relay:  relay,
		Config: cfg,
		ticker: time.NewTicker(interval),
	}
}

// Run starts the scheduler loop. Call as a goroutine.
func (m *MatchMaker) Run() {
	slog.Info("matchmaker started")
	// Clear stale assignments from previous server run.
	m.DB.Exec("UPDATE match_assignments SET status='failed', decline_reason='server restarted' WHERE status IN ('pending','assigned','accepted','ready','in_progress','declined')")
	for range m.ticker.C {
		m.tick()
	}
}

// tick runs one scheduling cycle.
func (m *MatchMaker) tick() {
	// Phase 1: Retry expired assignments
	if err := m.DB.RetryExpiredAssignments(); err != nil {
		slog.Error("matchmaker retry", "err", err)
	}
	// Phase 2: Fail stale assignments (coaches offline or server restarted)
	m.DB.Exec("UPDATE match_assignments SET status='failed', decline_reason='timeout' WHERE (status='in_progress' AND in_progress_at < datetime('now','-5 minutes')) OR (status='ready' AND assigned_at < datetime('now','-2 minutes'))")
	if err := m.DB.FailStaleAssignments(); err != nil {
		slog.Error("matchmaker fail stale", "err", err)
	}

	// Get online coaches
	coaches, err := m.DB.GetOnlineCoaches(90)
	if err != nil {
		slog.Error("matchmaker get online coaches", "err", err)
		return
	}
	if len(coaches) == 0 {
		return
	}
	slog.Info("matchmaker tick", "coaches", len(coaches))

	// Collect all available AIs from all coaches
	type availAI struct {
		ai     db.CoachAIRow
		coach  db.CoachRow
	}
	var allAIs []availAI
	for _, c := range coaches {
		ais, err := m.DB.GetAvailableAIs(c.ID)
		if err != nil { continue }
		for _, ai := range ais {
			allAIs = append(allAIs, availAI{ai: ai, coach: c})
		}
	}
	if len(allAIs) < 2 { return }

	// Build feasible pairs with priorities
	type pair struct {
		a        availAI
		b        availAI
		priority float64
	}
	var pairs []pair
	for i := 0; i < len(allAIs); i++ {
		for j := i + 1; j < len(allAIs); j++ {
			a, b := allAIs[i], allAIs[j]
			// Skip same engine (can be re-enabled later)
			if a.ai.EngineName == b.ai.EngineName && a.ai.EngineVersion == b.ai.EngineVersion {
				continue
			}
			// Resource check
			if a.coach.ID == b.coach.ID {
				// Same host: need combined resources
				usedCores := 0
				for _, ca := range allAIs {
					if ca.coach.ID == a.coach.ID {
						usedCores += ca.ai.CoresPerInstance * ca.ai.InstancesRunning
					}
				}
				freeCores := a.coach.CoresTotal - usedCores
				if freeCores < a.ai.CoresPerInstance+b.ai.CoresPerInstance {
					continue
				}
			} else {
				// Different hosts: check each independently (already filtered by GetAvailableAIs)
			}

			// Elo uncertainty
			aEngID, _ := m.DB.GetEngineIDByName(a.ai.EngineName)
			bEngID, _ := m.DB.GetEngineIDByName(b.ai.EngineName)
			if aEngID == 0 || bEngID == 0 { continue }

			aGames := m.countGames(aEngID)
			bGames := m.countGames(bEngID)
			ciA := 400.0 / math.Sqrt(math.Max(float64(aGames), 1))
			ciB := 400.0 / math.Sqrt(math.Max(float64(bGames), 1))
			priority := math.Sqrt(ciA*ciA+ciB*ciB) * (1.0 + m.hoursSinceLastMatch(aEngID, bEngID)/48.0)

			pairs = append(pairs, pair{a: a, b: b, priority: priority})
		}
	}

	if len(pairs) == 0 {
		slog.Info("matchmaker no feasible pairs", "coaches", len(coaches), "ais", len(allAIs))
		return
	}

	// Pick the best pair (highest priority × recency)
	rand.Shuffle(len(pairs), func(i, j int) { pairs[i], pairs[j] = pairs[j], pairs[i] })
	var best pair
	bestPriority := -1.0
	for _, p := range pairs {
		if p.priority > bestPriority { bestPriority = p.priority; best = p }
	}
	if best.a.ai.ID == 0 { return }

	// Select time control: weighted random, with uncertainty boost for longer controls.
	// Higher combined CI → more weight on longer time controls.
	ciA := 400.0 / math.Sqrt(math.Max(float64(m.countGames(best.a.ai.ID)), 1))
	ciB := 400.0 / math.Sqrt(math.Max(float64(m.countGames(best.b.ai.ID)), 1))
	combinedCI := (ciA + ciB) / 2.0
	uncertaintyBoost := combinedCI / 400.0 // 1.0 for new engines, ~0.03 for established

	var totalWeight float64
	var weights []float64
	for _, tc := range m.Config.TimeControls {
		w := tc.Weight + tc.Weight*uncertaintyBoost*2 // long games boosted more by uncertainty
		weights = append(weights, w)
		totalWeight += w
	}
	r := rand.Float64() * totalWeight
	var chosen TimeControl
	cumulative := 0.0
	for i, w := range weights {
		cumulative += w
		if r <= cumulative { chosen = m.Config.TimeControls[i]; break }
	}
	if chosen.Seconds == 0 { chosen = m.Config.TimeControls[0] }

	// Register players with time-aware version (e.g., "d8-es44" → "d8-es44-30s")
	timeSuffix := fmt.Sprintf("-%ds", chosen.Seconds)
	aVerTimed := best.a.ai.EngineVersion + timeSuffix
	bVerTimed := best.b.ai.EngineVersion + timeSuffix

	aEngID, _ := m.DB.GetEngineID(best.a.ai.EngineName, aVerTimed)
	if aEngID == 0 {
		m.DB.Exec("INSERT OR IGNORE INTO engines (name, version) VALUES (?,?)", best.a.ai.EngineName, aVerTimed)
		aEngID, _ = m.DB.GetEngineID(best.a.ai.EngineName, aVerTimed)
	}
	bEngID, _ := m.DB.GetEngineID(best.b.ai.EngineName, bVerTimed)
	if bEngID == 0 {
		m.DB.Exec("INSERT OR IGNORE INTO engines (name, version) VALUES (?,?)", best.b.ai.EngineName, bVerTimed)
		bEngID, _ = m.DB.GetEngineID(best.b.ai.EngineName, bVerTimed)
	}

	s1 := randomSessionID()
	s2 := randomSessionID()
	numGames := m.Config.GamesPerMatch
	if numGames == 0 { numGames = 2 }
	tcJSON := fmt.Sprintf(`{"type":"total","seconds":%d}`, chosen.Seconds)
	id, err := m.DB.CreateAssignment(aEngID, bEngID, best.a.ai.ID, best.b.ai.ID, tcJSON, numGames, s1, s2)
	if err != nil {
		slog.Error("matchmaker create assignment", "err", err)
		return
	}
	slog.Info("matchmaker assigned", "id", id,
		"e1", best.a.ai.EngineName+":"+aVerTimed+"@"+best.a.coach.CoachID,
		"e2", best.b.ai.EngineName+":"+bVerTimed+"@"+best.b.coach.CoachID,
		"time", chosen.Label, "priority", bestPriority)
}

// OnBothReady is called when both coaches report ready for an assignment.
func (m *MatchMaker) OnBothReady(assignmentID int) {
	go m.executeMatch(assignmentID)
}

// HandleStatus is a debug endpoint showing matchmaker state.
func (m *MatchMaker) HandleStatus(w http.ResponseWriter, r *http.Request) {
	coaches, _ := m.DB.GetOnlineCoaches(90)
	var out []map[string]any
	for _, c := range coaches {
		ais, _ := m.DB.GetAvailableAIs(c.ID)
		out = append(out, map[string]any{
			"coach_id": c.CoachID, "label": c.Label,
			"cores_total": c.CoresTotal, "last_seen": c.LastSeen,
			"ais": len(ais),
		})
	}
	// Count pending assignments
	var pending int
	m.DB.QueryRow("SELECT COUNT(*) FROM match_assignments WHERE status IN ('pending','assigned','accepted','ready','in_progress')").Scan(&pending)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"coaches": out, "pending_assignments": pending})
}

func (m *MatchMaker) countGames(engineID int) int {
	var count int
	m.DB.QueryRow("SELECT COUNT(*) FROM games WHERE black_id=? OR white_id=?", engineID, engineID).Scan(&count)
	return count
}

func (m *MatchMaker) hoursSinceLastMatch(e1, e2 int) float64 {
	var lastTime string
	err := m.DB.QueryRow(`SELECT created_at FROM games WHERE (black_id=? AND white_id=?) OR (black_id=? AND white_id=?) ORDER BY created_at DESC LIMIT 1`,
		e1, e2, e2, e1).Scan(&lastTime)
	if err != nil { return 999.0 }
	t, err := time.Parse(time.RFC3339, lastTime)
	if err != nil { return 999.0 }
	return time.Since(t).Hours()
}

func randomSessionID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// executeMatch runs the actual games over WebSocket relay.
func (m *MatchMaker) executeMatch(assignmentID int) {
	ctx := context.Background()

	// Get assignment details
	var a db.AssignmentRow
	row := m.DB.QueryRow(`SELECT id, engine1_id, engine2_id, coach1_ai_id, coach2_ai_id,
		COALESCE(time_control,'{}'), num_games, COALESCE(session1_id,''), COALESCE(session2_id,''), status
		FROM match_assignments WHERE id=?`, assignmentID)
	if err := row.Scan(&a.ID, &a.Engine1ID, &a.Engine2ID, &a.Coach1AIID, &a.Coach2AIID,
		&a.TimeControl, &a.NumGames, &a.Session1ID, &a.Session2ID, &a.Status); err != nil {
		slog.Error("executeMatch get assignment", "err", err)
		return
	}

	// Wait for both streams
	blackStream, err := m.Relay.WaitForStream(a.Session1ID, 120)
	if err != nil {
		m.DB.UpdateAssignmentStatus(assignmentID, "failed", "timeout waiting for coach 1 (black)")
		return
	}
	whiteStream, err := m.Relay.WaitForStream(a.Session2ID, 120)
	if err != nil {
		m.DB.UpdateAssignmentStatus(assignmentID, "failed", "timeout waiting for coach 2 (white)")
		return
	}

	// Mark in progress
	m.DB.UpdateAssignmentStatus(assignmentID, "in_progress", "")

	// Get engine names for record-keeping
	var e1Name, e1Ver, e2Name, e2Ver string
	m.DB.QueryRow("SELECT name, version FROM engines WHERE id=?", a.Engine1ID).Scan(&e1Name, &e1Ver)
	m.DB.QueryRow("SELECT name, version FROM engines WHERE id=?", a.Engine2ID).Scan(&e2Name, &e2Ver)

	// Play games
	var gameTimeSec float64 = 60
	var tc struct{ Seconds float64 `json:"seconds"` }; json.Unmarshal([]byte(a.TimeControl), &tc); gameTimeSec = tc.Seconds

	totalGames := a.NumGames
	if totalGames == 0 { totalGames = 2 }

	// Play each game alternating colors
	games := playGames(ctx, blackStream, whiteStream, totalGames, gameTimeSec)

	// Store results (retry on DB lock)
	var matchID int
	for attempt := 0; attempt < 5; attempt++ {
		matchID, err = m.storeResults(a, games, e1Name, e1Ver, e2Name, e2Ver)
		if err == nil {
			break
		}
		if strings.Contains(err.Error(), "locked") {
			time.Sleep(time.Duration(500*(attempt+1)) * time.Millisecond)
			continue
		}
		break
	}
	if err != nil {
		slog.Error("executeMatch store results", "err", err)
		m.DB.UpdateAssignmentStatus(assignmentID, "failed", "store results: "+err.Error())
		return
	}

	slog.Info("matchmaker match complete", "assignment", assignmentID, "match", matchID)
	m.DB.UpdateAssignmentStatus(assignmentID, "completed", "")

	// Cleanup relay sessions
	m.Relay.Cleanup(a.Session1ID)
	m.Relay.Cleanup(a.Session2ID)
}

func (m *MatchMaker) storeResults(a db.AssignmentRow, games []gameResult, e1Name, e1Ver, e2Name, e2Ver string) (int, error) {
	// Auto-register engines if needed
	e1ID := a.Engine1ID
	e2ID := a.Engine2ID
	if e1ID == 0 {
		m.DB.Exec("INSERT OR IGNORE INTO engines (name,version) VALUES (?,?)", e1Name, e1Ver)
		e1ID, _ = m.DB.GetEngineID(e1Name, e1Ver)
	}
	if e2ID == 0 {
		m.DB.Exec("INSERT OR IGNORE INTO engines (name,version) VALUES (?,?)", e2Name, e2Ver)
		e2ID, _ = m.DB.GetEngineID(e2Name, e2Ver)
	}

	res, err := m.DB.Exec(`INSERT INTO matches (engine1_id, engine2_id, time_control, total_games, runner_id)
		VALUES (?,?,?,?,?)`, e1ID, e2ID, a.TimeControl, len(games), "matchmaker")
	if err != nil { return 0, err }
	matchID64, _ := res.LastInsertId()
	matchID := int(matchID64)

	wins1, wins2, draws := 0, 0, 0
	for i, g := range games {
		var blackID, whiteID int
		// Determine which engine is black/white in this game
		if i%2 == 0 {
			blackID, whiteID = e1ID, e2ID
		} else {
			blackID, whiteID = e2ID, e1ID
		}
		if g.Result == "1-0" { wins1++ } else if g.Result == "0-1" { wins2++ } else { draws++ }

		_, err := m.DB.Exec(`INSERT INTO games (match_id, game_number, black_id, white_id, result, final_score, opening_line, black_time_s, white_time_s, black_nodes, white_nodes, black_depth, white_depth)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			matchID, i+1, blackID, whiteID, g.Result, g.FinalScore, g.OpeningLine,
			g.BlackTimeS, g.WhiteTimeS, g.BlackNodes, g.WhiteNodes, g.BlackDepth, g.WhiteDepth)
		if err != nil { return matchID, err }
	}

	// Update match stats
	m.DB.Exec("UPDATE matches SET wins_1=?, wins_2=?, draws=? WHERE id=?", wins1, wins2, draws, matchID)

	// Recompute Elo
	m.recomputeElo(e1ID)
	m.recomputeElo(e2ID)

	return matchID, nil
}

// recomputeElo recalculates Elo ratings from scratch for a single engine.
//
// Elo constants (standard chess values, adapted for Othello):
//   - Initial rating: 1500 (baseline for all new players)
//   - K-factor: 32 for first 20 games (provisional), 16 thereafter
//     Higher K means faster adaptation to true strength. Othello is
//     lower-variance than chess (each game is a single data point, not
//     30-40 moves of incremental information), so we use the same
//     provisional period (20 games) as standard Elo.
//   - Scale: 400 (a 400-point difference predicts ~91% win rate)
//     This is the standard chess value, consistent with most Elo systems.
//   - Expected score formula: 1 / (1 + 10^((opponent - player) / 400))
func (m *MatchMaker) recomputeElo(engineID int) {
	m.DB.Exec("DELETE FROM elo_history WHERE engine_id=?", engineID)

	rows, err := m.DB.Query(`SELECT g.id, g.match_id, g.black_id, g.white_id, g.result, g.created_at
		FROM games g WHERE g.black_id=? OR g.white_id=? ORDER BY g.created_at, g.id`, engineID, engineID)
	if err != nil { return }
	defer rows.Close()

	ratings := map[int]float64{engineID: 1500}
	gamesPlayed := map[int]int{engineID: 0}

	for rows.Next() {
		var gid, mid, bid, wid int
		var result, createdAt string
		if err := rows.Scan(&gid, &mid, &bid, &wid, &result, &createdAt); err != nil { continue }
		oppID := bid
		if bid == engineID { oppID = wid }
		if _, ok := ratings[oppID]; !ok { ratings[oppID] = 1500; gamesPlayed[oppID] = 0 }

		rA, rB := ratings[engineID], ratings[oppID]
		gA, gB := gamesPlayed[engineID], gamesPlayed[oppID]

		kA := 16.0
		if gA < 20 { kA = 32.0 }
		kB := 16.0
		if gB < 20 { kB = 32.0 }

		expectedA := 1.0 / (1.0 + math.Pow(10, (rB-rA)/400.0))
		var scoreA float64
		if bid == engineID { // engine is black
			if result == "1-0" { scoreA = 1.0 } else if result == "0-1" { scoreA = 0.0 } else { scoreA = 0.5 }
		} else { // engine is white
			if result == "0-1" { scoreA = 1.0 } else if result == "1-0" { scoreA = 0.0 } else { scoreA = 0.5 }
		}

		nA := rA + kA*(scoreA-expectedA)
		nB := rB + kB*((1.0-scoreA)-(1.0-expectedA))

		wins, losses, draws := 0, 0, 0
		if scoreA == 1.0 { wins = 1 } else if scoreA == 0.0 { losses = 1 } else { draws = 1 }

		m.DB.Exec(`INSERT INTO elo_history (engine_id, opponent_id, match_id, rating_before, rating_after, games, wins, losses, draws)
			VALUES (?,?,?,?,?,1,?,?,?)`, engineID, oppID, mid, rA, nA, wins, losses, draws)

		ratings[engineID] = nA
		ratings[oppID] = nB
		gamesPlayed[engineID]++
		gamesPlayed[oppID]++
	}
}
