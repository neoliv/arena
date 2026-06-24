// Package matchmaker schedules and executes distributed matches.
package matchmaker

import (
	"context"
	"sync"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	crand "crypto/rand"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/neoliv/arena/internal/coach"
	"github.com/neoliv/arena/internal/db"
	"github.com/neoliv/arena/internal/elo"
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
		TickInterval:     15 * time.Second,
	}
}

// MatchMaker schedules matches and executes them.
type MatchMaker struct {
	DB     *db.DB
	eloMu  sync.Mutex
	Relay  interface {
		WaitForStream(sessionID string, timeoutSec int) (coach.Stream, error)
		Cleanup(sessionID string)
	}
	Config Config
	ticker *time.Ticker
	wakeup chan struct{} // signals when a match completes
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
		wakeup: make(chan struct{}, 8),
	}
}

// Run starts the scheduler loop. Call as a goroutine.
func (m *MatchMaker) Run() {
	slog.Info("matchmaker started")
	m.DB.Exec("UPDATE match_assignments SET status='failed', decline_reason='server restarted' WHERE status IN ('pending','assigned','accepted','ready','in_progress','declined')")
	for {
		select {
		case <-m.ticker.C:
		case <-m.wakeup:
		}
		m.tick()
	}
}

// tick runs one scheduling cycle.
func (m *MatchMaker) tick() {
	if err := m.DB.RetryExpiredAssignments(); err != nil {
		slog.Error("matchmaker retry", "err", err)
	}
	m.DB.Exec("UPDATE match_assignments SET status='failed', decline_reason='timeout' WHERE (status='in_progress' AND in_progress_at < datetime('now','-5 minutes')) OR (status='ready' AND assigned_at < datetime('now','-2 minutes'))")
	m.DB.Exec("UPDATE match_assignments SET status='retry', decline_reason='accepted timeout', retry_after=datetime('now') WHERE status='accepted' AND assigned_at < datetime('now','-90 seconds')")
	if err := m.DB.FailStaleAssignments(); err != nil {
		slog.Error("matchmaker fail stale", "err", err)
	}

	coaches, err := m.DB.GetOnlineCoaches(90)
	if err != nil {
		slog.Error("matchmaker get online coaches", "err", err)
		return
	}
	if len(coaches) == 0 {
		return
	}
	slog.Info("matchmaker tick", "coaches", len(coaches))
	for _, c := range coaches {
		rows, _ := m.DB.Query("SELECT engine_name, engine_version, instances_running, max_instances, is_available FROM coach_ais WHERE coach_id=?", c.ID)
		if rows != nil {
			for rows.Next() {
				var n, v string; var run, max, avail int
				rows.Scan(&n, &v, &run, &max, &avail)
				slog.Info("  ai", "coach", c.CoachID, "name", n, "ver", v, "running", run, "max", max, "avail", avail)
			}
			rows.Close()
		}
	}

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
	if len(allAIs) < 2 {
		slog.Info("matchmaker not enough AIs", "available", len(allAIs), "coaches", len(coaches))
		for _, c := range coaches {
			rows, _ := m.DB.Query("SELECT engine_name, engine_version, instances_running, max_instances FROM coach_ais WHERE coach_id=?", c.ID)
			if rows != nil {
				for rows.Next() {
					var name, ver string; var running, max int
					rows.Scan(&name, &ver, &running, &max)
					slog.Info("  coach ai", "coach", c.CoachID, "ai", name+":"+ver, "running", running, "max", max)
				}
				rows.Close()
			}
		}
		return
	}

	type pair struct {
		a        availAI
		b        availAI
		priority float64
		aEngID   int
		bEngID   int
	}
	var pairs []pair
	for i := 0; i < len(allAIs); i++ {
		for j := i + 1; j < len(allAIs); j++ {
			a, b := allAIs[i], allAIs[j]
			if a.ai.EngineName == b.ai.EngineName && a.ai.EngineVersion == b.ai.EngineVersion {
				continue
			}
			if a.coach.ID == b.coach.ID {
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
			}

			aEngID, _ := m.DB.GetEngineID(a.ai.EngineName, a.ai.EngineVersion)
			bEngID, _ := m.DB.GetEngineID(b.ai.EngineName, b.ai.EngineVersion)
			if aEngID == 0 || bEngID == 0 { continue }

			aGames := m.countGames(aEngID)
			bGames := m.countGames(bEngID)
			ciA := 400.0 / math.Sqrt(math.Max(float64(aGames), 1))
			ciB := 400.0 / math.Sqrt(math.Max(float64(bGames), 1))
			priority := math.Sqrt(ciA*ciA+ciB*ciB) * (1.0 + m.hoursSinceLastMatch(aEngID, bEngID)/48.0)

			pairs = append(pairs, pair{a: a, b: b, priority: priority, aEngID: aEngID, bEngID: bEngID})
		}
	}

	if len(pairs) == 0 {
		slog.Info("matchmaker no feasible pairs", "coaches", len(coaches), "ais", len(allAIs))
		return
	}

	rand.Shuffle(len(pairs), func(i, j int) { pairs[i], pairs[j] = pairs[j], pairs[i] })
	var best pair
	bestPriority := -1.0
	for _, p := range pairs {
		if p.priority > bestPriority { bestPriority = p.priority; best = p }
	}
	if best.a.ai.ID == 0 { return }

	ciA := 400.0 / math.Sqrt(math.Max(float64(m.countGames(best.aEngID)), 1))
	ciB := 400.0 / math.Sqrt(math.Max(float64(m.countGames(best.bEngID)), 1))
	combinedCI := (ciA + ciB) / 2.0
	uncertaintyBoost := combinedCI / 400.0

	var totalWeight float64
	var weights []float64
	for _, tc := range m.Config.TimeControls {
		w := tc.Weight + tc.Weight*uncertaintyBoost*2
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

	timeSuffix := fmt.Sprintf("-%ds", chosen.Seconds)
	aVerTimed := best.a.ai.EngineVersion + timeSuffix
	bVerTimed := best.b.ai.EngineVersion + timeSuffix

	aEngID, _ := m.DB.GetEngineID(best.a.ai.EngineName, aVerTimed)
	if aEngID == 0 {
		m.DB.Exec("INSERT OR IGNORE INTO engines (name, version, created) SELECT ?, ?, COALESCE((SELECT created FROM engines WHERE name=? AND version=?), datetime('now'))", best.a.ai.EngineName, aVerTimed, best.a.ai.EngineName, best.a.ai.EngineVersion)
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

func (m *MatchMaker) NotifyNewPlayers() {
	select { case m.wakeup <- struct{}{}: default: }
}

func (m *MatchMaker) OnBothReady(assignmentID int) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("match execution panicked", "assignment", assignmentID, "panic", r)
				m.DB.UpdateAssignmentStatus(assignmentID, "failed", "internal error")
				var a db.AssignmentRow
				if err := m.DB.QueryRow("SELECT COALESCE(session1_id,''), COALESCE(session2_id,'') FROM match_assignments WHERE id=?", assignmentID).Scan(&a.Session1ID, &a.Session2ID); err == nil {
					if a.Session1ID != "" { m.Relay.Cleanup(a.Session1ID) }
					if a.Session2ID != "" { m.Relay.Cleanup(a.Session2ID) }
				}
			}
		}()
		m.executeMatch(assignmentID)
	}()
}

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
	crand.Read(b)
	return fmt.Sprintf("%x", b)
}

func (m *MatchMaker) executeMatch(assignmentID int) {
	ctx := context.Background()

	var a db.AssignmentRow
	row := m.DB.QueryRow(`SELECT id, engine1_id, engine2_id, coach1_ai_id, coach2_ai_id,
		COALESCE(time_control,'{}'), num_games, COALESCE(session1_id,''), COALESCE(session2_id,''), status
		FROM match_assignments WHERE id=?`, assignmentID)
	if err := row.Scan(&a.ID, &a.Engine1ID, &a.Engine2ID, &a.Coach1AIID, &a.Coach2AIID,
		&a.TimeControl, &a.NumGames, &a.Session1ID, &a.Session2ID, &a.Status); err != nil {
		slog.Error("executeMatch get assignment", "err", err)
		return
	}

	defer m.Relay.Cleanup(a.Session1ID)
	defer m.Relay.Cleanup(a.Session2ID)

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

	m.DB.UpdateAssignmentStatus(assignmentID, "in_progress", "")

	var e1Name, e1Ver, e2Name, e2Ver string
	m.DB.QueryRow("SELECT name, version FROM engines WHERE id=?", a.Engine1ID).Scan(&e1Name, &e1Ver)
	m.DB.QueryRow("SELECT name, version FROM engines WHERE id=?", a.Engine2ID).Scan(&e2Name, &e2Ver)

	var gameTimeSec float64 = 60
	var tc struct{ Seconds float64 `json:"seconds"` }; json.Unmarshal([]byte(a.TimeControl), &tc); gameTimeSec = tc.Seconds

	totalGames := a.NumGames
	if totalGames == 0 { totalGames = 2 }

	games := playGames(ctx, blackStream, whiteStream, totalGames, gameTimeSec)
	// Only discard games that had 0 moves (infrastructure failure:
	// coach restart, network drop before play began). Games with
	// moves are stored regardless of disconnect — timeouts, kills,
	// and forfeits are real results that must count for Elo.
	realGames := games[:0]
	for _, g := range games {
		if len(g.Moves) > 0 { realGames = append(realGames, g) }
	}
	games = realGames
	if len(games) == 0 {
		slog.Warn("all games ended without moves, skipping store", "assignment", assignmentID)
		m.DB.UpdateAssignmentStatus(assignmentID, "failed", "no moves played")
		m.Relay.Cleanup(a.Session1ID)
		m.Relay.Cleanup(a.Session2ID)
		select { case m.wakeup <- struct{}{}: default: }
		return
	}
	slog.Info("playGames completed", "assignment", assignmentID, "games", len(games))

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

	m.Relay.Cleanup(a.Session1ID)
	m.Relay.Cleanup(a.Session2ID)
	select { case m.wakeup <- struct{}{}: default: }
}

func (m *MatchMaker) storeResults(a db.AssignmentRow, games []gameResult, e1Name, e1Ver, e2Name, e2Ver string) (int, error) {
	e1ID := a.Engine1ID
	e2ID := a.Engine2ID
	if e1ID == 0 {
		m.DB.Exec("INSERT OR IGNORE INTO engines (name,version,created) VALUES (?,?,datetime('now'))", e1Name, e1Ver)
		e1ID, _ = m.DB.GetEngineID(e1Name, e1Ver)
	}
	if e2ID == 0 {
		m.DB.Exec("INSERT OR IGNORE INTO engines (name,version,created) VALUES (?,?,datetime('now'))", e2Name, e2Ver)
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
		if i%2 == 0 {
			blackID, whiteID = e1ID, e2ID
		} else {
			blackID, whiteID = e2ID, e1ID
		}
		if g.Result == "1-0" {
				if blackID == e1ID { wins1++ } else { wins2++ }
			} else if g.Result == "0-1" {
				if whiteID == e1ID { wins1++ } else { wins2++ }
			} else { draws++ }

		res, err := m.DB.Exec(`INSERT INTO games (match_id, game_number, black_id, white_id, result, final_score, opening_line, black_time_s, white_time_s, black_nodes, white_nodes, black_depth, white_depth)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			matchID, i+1, blackID, whiteID, g.Result, g.FinalScore, g.OpeningLine,
			g.BlackTimeS, g.WhiteTimeS, g.BlackNodes, g.WhiteNodes, g.BlackDepth, g.WhiteDepth)
		if err != nil { return matchID, err }
		gameID, _ := res.LastInsertId()
		if len(g.Moves) > 0 {
			var buf strings.Builder
			var args []any
			buf.WriteString("INSERT INTO game_moves (game_id, move_num, side, move, nodes, depth, time_ms, score) VALUES ")
			for mi, mv := range g.Moves {
				if mi > 0 { buf.WriteString(", ") }
				buf.WriteString("(?,?,?,?,?,?,?,?)")
				args = append(args, gameID, mi+1, mv.Side, mv.Move, mv.Nodes, mv.Depth, mv.TimeMs, mv.Score)
			}
			if _, err := m.DB.Exec(buf.String(), args...); err != nil {
				slog.Error("store game_moves FAILED; check DB schema", "game", gameID, "moves", len(g.Moves), "err", err)
				fmt.Fprintf(os.Stderr, "store game_moves FAILED: game=%d moves=%d err=%v\n", gameID, len(g.Moves), err)
			}
		}
	}

	m.DB.Exec("UPDATE matches SET wins_1=?, wins_2=?, draws=? WHERE id=?", wins1, wins2, draws, matchID)

	// Incremental Elo: update rating for each game result.
	for i, g := range games {
		var blackID, whiteID int
		if i%2 == 0 { blackID, whiteID = e1ID, e2ID } else { blackID, whiteID = e2ID, e1ID }
		var scoreA float64
		switch {
		case g.Result == "1-0": scoreA = 1.0
		case g.Result == "0-1": scoreA = 0.0
		default: scoreA = 0.5
		}
		// For the black player, scoreA is already correct.
		// For the white player, invert.
		m.updateElo(blackID, whiteID, matchID, scoreA)
		m.updateElo(whiteID, blackID, matchID, 1.0-scoreA)
	}

	return matchID, nil
}

// updateElo appends one Elo history row for engine after a game against opponent.
func (m *MatchMaker) updateElo(engineID, opponentID, matchID int, scoreA float64) {
	m.eloMu.Lock()
	defer m.eloMu.Unlock()
	var rA, rB float64
	var gamesA int
	m.DB.QueryRow(`SELECT COALESCE((SELECT rating_after FROM elo_history WHERE engine_id=? ORDER BY created_at DESC LIMIT 1), 1500.0)`, engineID).Scan(&rA)
	m.DB.QueryRow(`SELECT COALESCE((SELECT rating_after FROM elo_history WHERE engine_id=? ORDER BY created_at DESC LIMIT 1), 1500.0)`, opponentID).Scan(&rB)
	m.DB.QueryRow(`SELECT COALESCE((SELECT COUNT(*) FROM elo_history WHERE engine_id=?), 0)`, engineID).Scan(&gamesA)

	nA, _ := elo.Update(rA, rB, scoreA, gamesA)

	wins, losses, draws := 0, 0, 0
	if scoreA == 1.0 { wins = 1 } else if scoreA == 0.0 { losses = 1 } else { draws = 1 }

	m.DB.Exec(`INSERT INTO elo_history (engine_id, opponent_id, match_id, rating_before, rating_after, games, wins, losses, draws)
		VALUES (?,?,?,?,?,1,?,?,?)`, engineID, opponentID, matchID, rA, nA, wins, losses, draws)
}
