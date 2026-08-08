// Command calibrate — measure H-level strength against a fixed reference.
//
// For each H level 1..20, plays a sample of games between the candidate
// hgtp engine at that level and a fixed clean reference engine (Edax at a
// fixed search depth, deterministic best-move play, default depth 6 ≈ 2000
// Elo human / "very good club player"), alternating colors and using the
// balanced 8-ply opening book. The win rate per level is converted to an
// Elo estimate (vs the reference) and written as JSON.
//
// Usage:
//   calibrate --hgtp /path/to/hgtp --eval /path/to/eval.dat \
//             [--ref-depth 6] [--games-per-level 60] [--tc 90] \
//             [--concurrency 4] [--out h-calib.json] [--levels 1-20]
//
// TIME CONTROL: the arena's PlayGame enforces SUDDEN-DEATH total wall time
// per side (tc * 1.05 seconds for the whole game). hgtp ignores game_time
// for budgeting and uses its own --budget per move, so tc must be large
// enough that no side ever hits the wall-clock cap (a full game is ~30
// moves/side; at 2000ms/move budget that's ~60s/side → tc=90 is safe).
// A too-small tc silently turns slow engines into forfeits (the reference
// "loses" every game once its clock runs out), which corrupts the curve.
//
// The JSON output is consumed by the GUI (settings win% label + adaptive
// controller). Example row: {"level":14,"games":120,"wins":76,"draws":4,
// "losses":40,"win_rate":0.65,"elo_vs_ref":108}

package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync"

	"github.com/neoliv/arena/internal/game"
)

type LevelResult struct {
	Level    int     `json:"level"`
	Games    int     `json:"games"`
	Wins     int     `json:"wins"`
	Draws    int     `json:"draws"`
	Losses   int     `json:"losses"`
	WinRate  float64 `json:"win_rate"`
	EloVsRef float64 `json:"elo_vs_ref"`
}

func main() {
	var (
		hgtpPath       = flag.String("hgtp", "", "path to the hgtp GTP binary")
		evalPath       = flag.String("eval", "", "path to Edax eval.dat")
		refDepth       = flag.Int("ref-depth", 6, "reference search depth (clean deterministic Edax)")
		gamesPerLevel  = flag.Int("games-per-level", 60, "games per level (half as black, half as white)")
		tc             = flag.Float64("tc", 90.0, "sudden-death total seconds per side (see note above)")
		budget         = flag.Int("budget", 2000, "per-move budget in ms passed to hgtp --budget")
		concurrency    = flag.Int("concurrency", 4, "parallel game pairs")
		levelsArg      = flag.String("levels", "1-20", "level range, e.g. 1-20 or 1,5,10,15,20")
		outPath        = flag.String("out", "h-calib.json", "output JSON path")
		deterministicC = flag.Bool("deterministic-candidate", false, "also play the candidate deterministically (noise off)")
		probe          = flag.Bool("probe", false, "candidate uses hgtp raw probe table (--probe)")
		dumpGame       = flag.Int("dump", -1, "print details of the game with this index (debug)")
		adjacent       = flag.Bool("adjacent", false, "candidate L vs reference L+step (monotonicity probe)")
		adjStep        = flag.Int("adjacent-step", 1, "step for --adjacent mode")
	)
	flag.Parse()

	if *hgtpPath == "" || *evalPath == "" {
		slog.Error("--hgtp and --eval are required")
		os.Exit(1)
	}
	levels := parseLevels(*levelsArg)
	book := game.LoadBook(embeddedBook)
	if len(book) == 0 {
		slog.Error("no openings loaded")
		os.Exit(1)
	}

	refCmd := fmt.Sprintf("%s --ref-depth %d --eval %s --budget %d", *hgtpPath, *refDepth, *evalPath, *budget)
	slog.Info("reference engine", "cmd", refCmd)

	var (
		mu      sync.Mutex
		results = make([]LevelResult, 0, len(levels))
		wg      sync.WaitGroup
		sem     = make(chan struct{}, *concurrency)
	)

	for _, lvl := range levels {
		lvl := lvl
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			refDepthFor := *refDepth
			if *adjacent {
				refDepthFor = lvl + *adjStep
			}
			candCmd := fmt.Sprintf("%s --level %d --eval %s --budget %d", *hgtpPath, lvl, *evalPath, *budget)
			if *deterministicC {
				candCmd += " --deterministic"
			}
			if *probe {
				candCmd += " --probe"
			}
			refCmd := fmt.Sprintf("%s --ref-depth %d --eval %s --budget %d", *hgtpPath, refDepthFor, *evalPath, *budget)
			slog.Info("calibrating level", "level", lvl, "ref_depth", refDepthFor, "cmd", candCmd)

			var wins, draws, losses int
			// Alternate colors so the first-move advantage cancels out.
			for g := 0; g < *gamesPerLevel; g++ {
				op := book[(lvl*7+g)%len(book)]
				var gr game.GameResult
				if g%2 == 0 {
					gr = playOneGame(candCmd, refCmd, op.Line, *tc)
				} else {
					gr = playOneGame(refCmd, candCmd, op.Line, *tc)
				}
				if *dumpGame >= 0 && g == *dumpGame {
					fmt.Printf("\n--- DUMP level=%d game=%d opening=%s ---\n", lvl, g, op.Line)
					fmt.Printf("result=%s score=%d-%d moves=%d disconnect=%v\n",
						gr.Result, gr.BlackScore, gr.WhiteScore, gr.TotalMoves, gr.Disconnect)
					if gr.InvestigationNeeded {
						fmt.Printf("INVESTIGATION: %s\n", gr.InvestigationReason)
					}
					fmt.Printf("scores=%d-%d result=%s\n", gr.BlackScore, gr.WhiteScore, gr.Result)
					fmt.Printf("moves: %s\n", strings.Join(gr.Moves, " "))
				}
				switch gr.Result {
				case "1-0":
					if g%2 == 0 {
						wins++
					} else {
						losses++
					}
				case "0-1":
					if g%2 == 0 {
						losses++
					} else {
						wins++
					}
				default:
					draws++
				}
			}

			games := wins + draws + losses
			rate := 0.0
			if games > 0 {
				rate = (float64(wins) + 0.5*float64(draws)) / float64(games)
			}
			// Elo from win rate (logistic). Positive = candidate stronger
			// than reference.
			elo := eloFromRate(rate)
			res := LevelResult{
				Level: lvl, Games: games, Wins: wins, Draws: draws, Losses: losses,
				WinRate: rate, EloVsRef: elo,
			}
			slog.Info("level done",
				"level", lvl, "score", fmt.Sprintf("%d-%d-%d", wins, draws, losses),
				"win_rate", fmt.Sprintf("%.3f", rate), "elo", fmt.Sprintf("%+.1f", elo))

			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}()
	}
	wg.Wait()

	// Sort by level (results arrive out of order due to concurrency).
	sortResults(results)
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		slog.Error("json marshal", "err", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*outPath, data, 0644); err != nil {
		slog.Error("write output", "err", err)
		os.Exit(1)
	}
	fmt.Printf("\n=== Calibration complete ===\n")
	for _, r := range results {
		fmt.Printf("H%-2d  %3d games  %4.1f%%  Elo %+6.1f\n",
			r.Level, r.Games, r.WinRate*100, r.EloVsRef)
	}
	fmt.Printf("Written to %s\n", *outPath)
}

func playOneGame(blackCmd, whiteCmd, opening string, tc float64) game.GameResult {
	black := game.StartEngine(blackCmd)
	white := game.StartEngine(whiteCmd)
	if black == nil || white == nil {
		return game.GameResult{Result: "0-1", Disconnect: true}
	}
	defer black.Stop()
	defer white.Stop()
	return game.PlayGame(black, white, opening, tc)
}

// eloFromRate converts a win rate (draws split) to an Elo difference.
// Logistic model: p = 1 / (1 + 10^(-elo/400)).
func eloFromRate(p float64) float64 {
	if p <= 0.0 {
		return -800.0
	}
	if p >= 1.0 {
		return 800.0
	}
	return -400.0 * mathLog10(1.0/p-1.0)
}

func mathLog10(x float64) float64 {
	return math.Log10(x)
}

func parseLevels(arg string) []int {
	var out []int
	for _, part := range strings.Split(arg, ",") {
		part = strings.TrimSpace(part)
		if strings.Contains(part, "-") {
			var lo, hi int
			fmt.Sscanf(part, "%d-%d", &lo, &hi)
			for l := lo; l <= hi; l++ {
				out = append(out, l)
			}
		} else if part != "" {
			var l int
			fmt.Sscanf(part, "%d", &l)
			out = append(out, l)
		}
	}
	return out
}

func sortResults(r []LevelResult) {
	for i := 1; i < len(r); i++ {
		for j := i; j > 0 && r[j-1].Level > r[j].Level; j-- {
			r[j-1], r[j] = r[j], r[j-1]
		}
	}
}

// ── Embedded opening book ──────────────────────────────────────────────

// Same balanced 8-ply openings the SPRT tool uses.
//
//go:embed openings_8ply.txt
var embeddedBook string
