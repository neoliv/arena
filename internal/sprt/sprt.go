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
}

// NewAccumulator creates a new SPRT accumulator.
func NewAccumulator(cfg Config) *Accumulator {
	// Pre-compute win probabilities
	pWin0 := winRate(cfg.Elo0)
	pWin1 := winRate(cfg.Elo1)

	return &Accumulator{
		cfg:       cfg,
		lower:     math.Log(cfg.Alpha / (1 - cfg.Beta)),
		upper:     math.Log((1 - cfg.Alpha) / cfg.Beta),
		pWin0:     pWin0,
		pWin1:     pWin1,
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

	// Check decision boundaries
	if a.llr >= a.upper {
		a.result = Accept
	} else if a.llr <= a.lower {
		a.result = Reject
	} else if a.pairs >= a.cfg.MaxGames {
		a.result = Inconclusive
	}
}

// AddGamePair takes two game results (color-swapped) and records the pair.
// The candidate is the engine playing Black in the first game and White
// in the second.
func (a *Accumulator) AddGamePair(blackGame, whiteGame GameOutcome) {
	cScore := blackGame.CandidateScore + whiteGame.CandidateScore
	rScore := blackGame.ReferenceScore + whiteGame.ReferenceScore
	a.AddPair(cScore > rScore)
}

// GameOutcome records the scores from a single game for the SPRT pair.
// The SPRT alternates who plays which color, so each game contributes
// a candidate score (from whichever color the candidate engine played)
// and a reference score.
type GameOutcome struct {
	CandidateScore  int
	ReferenceScore  int
}

// Status returns the current SPRT result.
func (a *Accumulator) Status() Result { return a.result }

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

// WinRate returns the expected win rate for a given Elo difference.
// Uses the standard logistic Elo formula. Draws are negligible at 1-2%
// between strong Othello engines so the model treats each pair as a
// binomial trial.
func winRate(eloDiff float64) float64 {
	return 1.0 / (1.0 + math.Pow(10, -eloDiff/400.0))
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
}

// EngineIdentity identifies an engine build.
type EngineIdentity struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	EngineID string `json:"engine_id,omitempty"`
	Commit   string `json:"commit,omitempty"`
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
