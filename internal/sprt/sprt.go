// Package sprt implements a Sequential Probability Ratio Test for
// determining whether a candidate engine is meaningfully weaker than
// a reference engine.
package sprt

import (
	"math"
	"time"
)

// Config holds SPRT parameters.
type Config struct {
	Elo0     float64 // null hypothesis: candidate Elo - reference Elo <= elo0
	Elo1     float64 // alternative: candidate Elo - reference Elo >= elo1
	Alpha    float64 // false positive rate (accept worse)
	Beta     float64 // false negative rate (reject equal)
	MaxGames int     // hard cap
}

// DefaultConfig returns sensible SPRT defaults for engine regression testing.
func DefaultConfig() Config {
	return Config{
		Elo0:     -10,
		Elo1:     0,
		Alpha:    0.05,
		Beta:     0.05,
		MaxGames: 400,
	}
}

// Result is the outcome of an SPRT run.
type Result int

const (
	Running      Result = iota // still accumulating
	Accept                     // H1: candidate is not worse
	Reject                     // H0: candidate is worse
	Inconclusive               // max games reached without decision
)

func (r Result) String() string {
	switch r {
	case Accept:
		return "accepted"
	case Reject:
		return "rejected"
	case Inconclusive:
		return "inconclusive"
	default:
		return "running"
	}
}

// Accumulator tracks SPRT state across game pairs.
type Accumulator struct {
	cfg    Config
	llr    float64
	pairs  int
	cWins  int // candidate pair-wins
	result Result

	lower float64
	upper float64
	pWin0 float64 // P(win | H0)
	pWin1 float64 // P(win | H1)

	startTime time.Time

	// LLR trajectory for manifest (snapshot after each pair)
	LLRHistory []LLRPoint
}

// LLRPoint is one entry in the LLR trajectory.
type LLRPoint struct {
	Pair   int      `json:"pair"`
	LLR    float64  `json:"llr"`
	CWins  int      `json:"c_wins"`
	EloEst *float64 `json:"elo_est"` // null until at least 1 win and 1 loss
}

// NewAccumulator creates a new SPRT accumulator.
func NewAccumulator(cfg Config) *Accumulator {
	pWin0 := winRate(cfg.Elo0)
	pWin1 := winRate(cfg.Elo1)

	return &Accumulator{
		cfg:    cfg,
		lower:  math.Log(cfg.Alpha / (1 - cfg.Beta)),
		upper:  math.Log((1 - cfg.Alpha) / cfg.Beta),
		pWin0:  pWin0,
		pWin1:  pWin1,
		startTime: time.Now(),
	}
}

// AddPair records one game pair result. A pair is won if the candidate
// outscores the reference across both colors.
func (a *Accumulator) AddPair(candidateWins bool) {
	if a.result != Running {
		return
	}

	a.pairs++
	if candidateWins {
		a.cWins++
		a.llr += math.Log(a.pWin1 / a.pWin0)
	} else {
		a.llr += math.Log((1 - a.pWin1) / (1 - a.pWin0))
	}

	// Record LLR trajectory
	var eloEst *float64
	if a.pairs > 0 && a.cWins > 0 && a.pairs > a.cWins {
		winPct := float64(a.cWins) / float64(a.pairs)
		e := -400 * math.Log10(1/winPct-1)
		eloEst = &e
	}
	a.LLRHistory = append(a.LLRHistory, LLRPoint{
		Pair: a.pairs, LLR: a.llr, CWins: a.cWins, EloEst: eloEst,
	})

	// Check decision boundaries
	if a.llr >= a.upper {
		a.result = Accept
	} else if a.llr <= a.lower {
		a.result = Reject
	} else if a.pairs >= a.cfg.MaxGames {
		a.result = Inconclusive
	}
}

// GameOutcome records the scores from a single game for the SPRT pair.
type GameOutcome struct {
	CandidateScore int
	ReferenceScore int
}

// AddGamePair takes two game results (color-swapped) and records the pair.
func (a *Accumulator) AddGamePair(blackGame, whiteGame GameOutcome) {
	cScore := blackGame.CandidateScore + whiteGame.CandidateScore
	rScore := blackGame.ReferenceScore + whiteGame.ReferenceScore
	a.AddPair(cScore > rScore)
}

// Status returns the current SPRT result.
func (a *Accumulator) Status() Result { return a.result }

// Pairs returns the number of completed pairs.
func (a *Accumulator) Pairs() int { return a.pairs }

// Stats returns current accumulator statistics.
func (a *Accumulator) Stats() (pairs, cWins int, llr float64, eloEst float64) {
	pairs = a.pairs
	cWins = a.cWins
	llr = a.llr
	if a.pairs > 0 && a.pairs > a.cWins {
		winPct := float64(a.cWins) / float64(a.pairs)
		if winPct > 0 && winPct < 1 {
			eloEst = -400 * math.Log10(1/winPct-1)
		}
	}
	return
}

// Elapsed returns the wall-clock time since the accumulator was created.
func (a *Accumulator) Elapsed() time.Duration {
	return time.Since(a.startTime)
}

// ── Stat accumulator (Welford's online algorithm) ──────────────────────

// StatAccumulator tracks min, max, avg, and stddev for a stream of float64 values.
type StatAccumulator struct {
	Count int     `json:"count"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Avg   float64 `json:"avg"`
	Stddev float64 `json:"stddev"`
	// Welford internals (not serialised)
	m2 float64
}

// NewStatAccumulator creates a new stat accumulator.
func NewStatAccumulator() *StatAccumulator {
	return &StatAccumulator{Min: math.MaxFloat64, Max: -math.MaxFloat64}
}

// Add pushes a new value into the accumulator.
func (s *StatAccumulator) Add(v float64) {
	s.Count++
	if v < s.Min {
		s.Min = v
	}
	if v > s.Max {
		s.Max = v
	}
	delta := v - s.Avg
	s.Avg += delta / float64(s.Count)
	s.m2 += delta * (v - s.Avg)
	if s.Count >= 2 {
		s.Stddev = math.Sqrt(s.m2 / float64(s.Count-1))
	}
}

// Finalize ensures the JSON output is clean (resets Min/Max for zero-count).
func (s *StatAccumulator) Finalize() {
	if s.Count == 0 {
		s.Min = 0
		s.Max = 0
	}
}

// ── Manifest types ────────────────────────────────────────────────────

// Manifest holds the complete SPRT run state for restart and analysis.
type Manifest struct {
	Version   string                  `json:"manifest_version"`
	TestID    string                  `json:"test_id"`
	Created   string                  `json:"created"`
	Updated   string                  `json:"updated"`
	Status    string                  `json:"status"`
	Config    ManifestConfig          `json:"sprt_config"`
	Candidate ManifestEngineIdentity  `json:"candidate"`
	Reference ManifestEngineIdentity  `json:"reference"`
	SPRTState ManifestSPRTState       `json:"sprt_state"`
	Pairs     []ManifestPair          `json:"pairs"`
	Stats     *ManifestAccumulatedStats `json:"accumulated_stats,omitempty"`
	LLRTraj   []LLRPoint              `json:"llr_trajectory"`
	Files     ManifestFiles           `json:"files"`
}

// ManifestConfig records the SPRT parameters used.
type ManifestConfig struct {
	Elo0    float64 `json:"elo0"`
	Elo1    float64 `json:"elo1"`
	Alpha   float64 `json:"alpha"`
	Beta    float64 `json:"beta"`
	MaxPairs int    `json:"max_pairs"`
	TC      float64 `json:"time_control_s"`
	Conc    int     `json:"concurrency"`
}

// ManifestEngineIdentity records everything needed to reproduce an engine build.
type ManifestEngineIdentity struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	GitCommit    string `json:"git_commit"`
	GitDirty     bool   `json:"git_dirty"`
	BinaryPath   string `json:"binary_path"`
	BinarySHA256 string `json:"binary_sha256"`
	Command      string `json:"command"`
	BuildProfile string `json:"build_profile"`
	RustcVersion string `json:"rustc_version"`
	WeightsPath  string `json:"weights_path,omitempty"`
	WeightsSHA256 string `json:"weights_sha256,omitempty"`
	HostHostname string `json:"host_hostname"`
	HostOS       string `json:"host_os"`
}

// ManifestSPRTState holds accumulator state for restart.
type ManifestSPRTState struct {
	PairsCompleted int     `json:"pairs_completed"`
	CandidateWins  int     `json:"candidate_pair_wins"`
	LLR            float64 `json:"llr"`
	EloEstimate    float64 `json:"elo_estimate"`
	LowerBound     float64 `json:"lower_bound"`
	UpperBound     float64 `json:"upper_bound"`
	StartTime      string  `json:"start_time"`
	ElapsedS       float64 `json:"elapsed_s"`
}

// ManifestPair holds both games of a color-swapped pair.
type ManifestPair struct {
	Index              int           `json:"index"`
	Opening            string        `json:"opening"`
	OpeningName        string        `json:"opening_name"`
	CandidateFirstColor string      `json:"candidate_first_color"`
	Completed          bool          `json:"completed"`
	StartTime          string        `json:"start_time"`
	EndTime            string        `json:"end_time"`
	Games              []ManifestGame `json:"games"`
}

// ManifestGame holds one game within a pair.
type ManifestGame struct {
	Role         string        `json:"role"`
	Result       string        `json:"result"`
	BlackScore   int           `json:"black_score"`
	WhiteScore   int           `json:"white_score"`
	TotalMoves   int           `json:"total_moves"`
	BlackTimeS   float64       `json:"black_time_s"`
	WhiteTimeS   float64       `json:"white_time_s"`
	Termination  string        `json:"termination"`
	Moves        []ManifestMove `json:"moves,omitempty"`
}

// ManifestMove is one engine-generated move with search statistics.
type ManifestMove struct {
	Ply         int     `json:"ply"`
	Color       string  `json:"color"`
	Engine      string  `json:"engine"`
	Move        string  `json:"move"`
	Nodes       int64   `json:"nodes"`
	Depth       int     `json:"depth"`
	TimeMs      float64 `json:"time_ms"`
	Timeout     bool    `json:"timeout"`
	Score       int     `json:"score"`
	Nps         int64   `json:"nps"`
	Empties     int     `json:"empties"`
	AllocatedMs float64 `json:"allocated_ms"`
	EndSearch   bool    `json:"end_search"`
	BookExit    bool    `json:"book_exit"`
	BookEval    *int    `json:"book_eval,omitempty"`
}

// ManifestAccumulatedStats holds stats aggregated across all games.
type ManifestAccumulatedStats struct {
	Candidate EngineStats `json:"candidate"`
	Reference EngineStats `json:"reference"`
}

// EngineStats holds per-ply and full-game aggregated statistics for one engine.
type EngineStats struct {
	PerPly   map[string]PerPlyStats `json:"per_ply"`
	FullGame FullGameStats          `json:"full_game"`
}

// PerPlyStats holds aggregated stats for one ply number.
type PerPlyStats struct {
	Nodes       *StatAccumulator `json:"nodes,omitempty"`
	Depth       *StatAccumulator `json:"depth,omitempty"`
	TimeMs      *StatAccumulator `json:"time_ms,omitempty"`
	Nps         *StatAccumulator `json:"nps,omitempty"`
	ScoreCp     *StatAccumulator `json:"score_cp,omitempty"`
	UnspentMs   *StatAccumulator `json:"unspent_ms,omitempty"`
	TimeoutRate float64          `json:"timeout_rate"`
	Timeouts    int              `json:"-"`
	Count       int              `json:"-"`
}

// FullGameStats holds stats aggregated per game.
type FullGameStats struct {
	NodesPerGame      *StatAccumulator `json:"nodes_per_game,omitempty"`
	DepthAvgPerGame   *StatAccumulator `json:"depth_avg_per_game,omitempty"`
	TimePerGameS      *StatAccumulator `json:"time_per_game_s,omitempty"`
	OverallNps        *StatAccumulator `json:"overall_nps,omitempty"`
	EndSearchStartPly *StatAccumulator `json:"end_search_start_ply,omitempty"`
	BookExitPly       *StatAccumulator `json:"book_exit_ply,omitempty"`
	BookExitEvalCp    *StatAccumulator `json:"book_exit_eval_cp,omitempty"`
	TimeoutsPerGame   *StatAccumulator `json:"timeouts_per_game,omitempty"`
	TotalNodes        int64            `json:"total_nodes"`
	TotalTimeS        float64          `json:"total_time_s"`
	TimeForfeits      int              `json:"time_forfeits"`
	IllegalMoves      int              `json:"illegal_moves"`
	Disconnects       int              `json:"disconnects"`
	Games             int              `json:"games"`
}

// ManifestFiles records paths to related output files.
type ManifestFiles struct {
	WTHOR    string `json:"wthor"`
	Summary  string `json:"summary"`
	Manifest string `json:"manifest"`
}

// EngineIdentity identifies an engine build.
type EngineIdentity struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	EngineID string `json:"engine_id,omitempty"`
	Commit   string `json:"commit,omitempty"`
}

// Summary holds a complete SPRT run summary for JSON output.
type Summary struct {
	Candidate   EngineIdentity `json:"candidate"`
	Reference   EngineIdentity `json:"reference"`
	Result      string         `json:"result"`
	EloEstimate float64        `json:"elo_estimate"`
	Games       int            `json:"games"`
	TimeControl float64        `json:"time_control"`
	LLR         float64        `json:"llr_final"`
	DurationSec float64        `json:"duration_sec"`
	Timestamp   string         `json:"timestamp"`
	Commit      string         `json:"commit,omitempty"`
}

// MakeSummary builds a summary for the completed run.
func (a *Accumulator) MakeSummary(candidate, reference EngineIdentity, tc float64) Summary {
	pairs, _, llr, eloEst := a.Stats()
	return Summary{
		Candidate:   candidate,
		Reference:   reference,
		Result:      a.result.String(),
		EloEstimate: math.Round(eloEst*10) / 10,
		Games:       pairs,
		TimeControl: tc,
		LLR:         math.Round(llr*100) / 100,
		DurationSec: math.Round(a.Elapsed().Seconds()),
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}
}

// WinRate returns the expected win rate for a given Elo difference.
func winRate(eloDiff float64) float64 {
	return 1.0 / (1.0 + math.Pow(10, -eloDiff/400.0))
}
