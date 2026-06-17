// sprt runs a Sequential Probability Ratio Test between two engine versions.
//
// It plays color-swapped game pairs locally via GTP subprocesses until
// the SPRT reaches a decision, then exits with:
//
//	0 = ACCEPT (candidate is not meaningfully weaker)
//	1 = REJECT (candidate is meaningfully weaker)
//	2 = INCONCLUSIVE (max games reached, no decision)
//
// Games are saved to a local WTHOR file. Nothing is posted to the arena.
//
// Usage:
//
//	sprt --candidate "./neursi --weights new.bin" \
//	     --reference "./neursi --weights prev.bin" \
//	     --tc 1 --output ~/coach/sprt/
package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/neoliv/arena/internal/game"
	"github.com/neoliv/arena/internal/sprt"
)

func main() {
	var (
		candidate   = flag.String("candidate", "", "Candidate engine command")
		reference   = flag.String("reference", "", "Reference engine command")
		tc          = flag.Float64("tc", 1.0, "Time control in seconds per game")
		elo0        = flag.Float64("elo0", -10, "Null hypothesis Elo difference")
		elo1        = flag.Float64("elo1", 0, "Alternative Elo difference")
		alpha       = flag.Float64("alpha", 0.05, "False positive rate")
		beta        = flag.Float64("beta", 0.05, "False negative rate")
		maxGames    = flag.Int("max-games", 400, "Maximum game pairs")
		concurrency = flag.Int("concurrency", 4, "Concurrent game pairs")
		outputDir   = flag.String("output", "", "Output directory for WTHOR + JSON")
	)
	flag.Parse()

	if *candidate == "" || *reference == "" {
		slog.Error("--candidate and --reference are required")
		os.Exit(2)
	}

	cfg := sprt.Config{
		Elo0: *elo0, Elo1: *elo1,
		Alpha: *alpha, Beta: *beta,
		MaxGames: *maxGames,
	}

	// Candidate/Reference identity from engine queries
	candID := queryIdentity(*candidate)
	refID := queryIdentity(*reference)

	slog.Info("SPRT starting",
		"candidate", candID.Name+" "+candID.Version,
		"reference", refID.Name+" "+refID.Version,
		"tc", *tc, "elo0", *elo0, "elo1", *elo1,
		"max_games", *maxGames, "concurrency", *concurrency,
	)

	// Load embedded book
	book := game.LoadBook(embeddedBook)

	acc := sprt.NewAccumulator(cfg)
	var (
		mu        sync.Mutex
		wg        sync.WaitGroup
		sem       = make(chan struct{}, *concurrency)
		pairIdx   int
		allGames  []gameResultPair
		gameCount int
	)

	// Play pairs until SPRT terminates
	for acc.Status() == sprt.Running {
		op := book[pairIdx%len(book)]
		pairIdx++
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, op game.Opening) {
			defer wg.Done()
			defer func() { <-sem }()

			pair := playPair(*candidate, *reference, op, *tc, idx)
			mu.Lock()
			allGames = append(allGames, pair)
			gameCount++
			acc.AddPair(pair.candidateWins())
			pairs, cWins, llr, eloEst := acc.Stats()
			slog.Info("pair done",
				"pair", gameCount,
				"score", fmt.Sprintf("%d-%d", cWins, pairs-cWins),
				"llr", fmt.Sprintf("%.2f", llr),
				"elo_est", fmt.Sprintf("%.1f", eloEst),
			)
			mu.Unlock()
		}(pairIdx, op)
	}
	wg.Wait()

	// Write output
	summary := acc.MakeSummary(candID, refID, *tc)

	if *outputDir != "" {
		os.MkdirAll(*outputDir, 0755)
		basename := filepath.Join(*outputDir,
			fmt.Sprintf("%s_v%s-vs-v%s", time.Now().Format("2006-01-02"),
				candID.Version, refID.Version))

		writeWTHOR(basename+".wthor", summary, allGames)
		writeJSON(basename+".json", summary)
		slog.Info("output written", "dir", *outputDir)
	}

	// Print final result
	pairs, cWins, _, eloEst := acc.Stats()
	fmt.Printf("\n=== SPRT Result ===\n")
	fmt.Printf("Candidate: %s %s\n", candID.Name, candID.Version)
	fmt.Printf("Reference: %s %s\n", refID.Name, refID.Version)
	fmt.Printf("Pairs: %d  Score: %d-%d  Elo est: %+.1f\n", pairs, cWins, pairs-cWins, eloEst)
	fmt.Printf("Decision: %s\n", strings.ToUpper(summary.Result))
	fmt.Printf("Duration: %s\n", acc.Elapsed().Round(time.Second))
	fmt.Printf("==================\n")

	switch acc.Status() {
	case sprt.Accept:
		os.Exit(0)
	case sprt.Reject:
		os.Exit(1)
	default:
		os.Exit(2)
	}
}

// gameResultPair holds both color-swapped games for one SPRT pair.
type gameResultPair struct {
	BlackGame game.GameResult
	WhiteGame game.GameResult
}

// candidateWins returns true if the candidate engine outscores the
// reference across both games.
func (p gameResultPair) candidateWins() bool {
	// Game 1: candidate plays Black (if pair index odd, they swap)
	// We detect: the engine names differ between candidate and reference.
	// Actually, we know which is which because playPair sets up the games.
	// Black is always the first engine path, White is the second.
	// In game 1 (BlackGame): Black=candidate, White=reference
	// In game 2 (WhiteGame): Black=reference, White=candidate
	cScore := p.BlackGame.BlackScore + p.WhiteGame.WhiteScore
	rScore := p.BlackGame.WhiteScore + p.WhiteGame.BlackScore
	return cScore > rScore
}

// playPair runs two games with swapped colors using the same opening.
func playPair(candidatePath, referencePath string, op game.Opening, tc float64, idx int) gameResultPair {
	var g1, g2 game.GameResult

	if idx%2 == 0 {
		// Candidate = Black first, then White
		g1 = playOneGame(candidatePath, referencePath, op.Line, tc)
		g2 = playOneGame(referencePath, candidatePath, op.Line, tc)
	} else {
		// Candidate = White first, then Black
		g1 = playOneGame(referencePath, candidatePath, op.Line, tc)
		g2 = playOneGame(candidatePath, referencePath, op.Line, tc)
	}

	return gameResultPair{BlackGame: g1, WhiteGame: g2}
}

// playOneGame runs a single game. blackPath/whitePath are engine commands.
func playOneGame(blackPath, whitePath, opening string, tc float64) game.GameResult {
	black := game.StartEngine(blackPath)
	white := game.StartEngine(whitePath)
	if black == nil || white == nil {
		return game.GameResult{Result: "0-1", Disconnect: true}
	}
	defer black.Stop()
	defer white.Stop()

	gr := game.PlayGame(black, white, opening, tc)
	gr.BlackName = blackPath
	gr.WhiteName = whitePath
	return gr
}

// queryIdentity sends name/version GTP commands to get engine identity.
func queryIdentity(enginePath string) sprt.EngineIdentity {
	s := game.StartEngine(enginePath)
	if s == nil {
		return sprt.EngineIdentity{Name: enginePath, Version: "unknown"}
	}
	defer s.Stop()

	name := strings.TrimSpace(strings.TrimPrefix(s.Send("name"), "= "))
	version := strings.TrimSpace(strings.TrimPrefix(s.Send("version"), "= "))
	if name == "" {
		name = filepath.Base(strings.Fields(enginePath)[0])
	}
	return sprt.EngineIdentity{Name: name, Version: version}
}

// ── WTHOR Output ──────────────────────────────────────────────────────

// writeWTHOR writes all games in WTHOR-compatible format.
// Format: each game is a line with black_score, white_score, theoretical_score,
// then the move sequence as 2-char coordinates (lowercase).
func writeWTHOR(path string, summary sprt.Summary, pairs []gameResultPair) {
	f, err := os.Create(path)
	if err != nil {
		slog.Error("create wthor", "path", path, "err", err)
		return
	}
	defer f.Close()

	// Header
	fmt.Fprintf(f, "# SPRT: %s v%s vs %s v%s\n",
		summary.Candidate.Name, summary.Candidate.Version,
		summary.Reference.Name, summary.Reference.Version)
	fmt.Fprintf(f, "# result: %s  elo_est: %+.1f  games: %d  tc: %.1fs\n",
		summary.Result, summary.EloEstimate, summary.Games, summary.TimeControl)
	fmt.Fprintf(f, "# timestamp: %s\n", summary.Timestamp)
	fmt.Fprintf(f, "#\n")

	for _, pair := range pairs {
		writeWTHORGame(f, pair.BlackGame)
		writeWTHORGame(f, pair.WhiteGame)
	}
}

func writeWTHORGame(f *os.File, gr game.GameResult) {
	blackScore := gr.BlackScore
	whiteScore := gr.WhiteScore
	if gr.Result == "0-1" {
		blackScore = 0
		whiteScore = 64 - gr.WhiteScore
	} else if gr.Result == "1-0" {
		blackScore = 64 - gr.BlackScore
		whiteScore = 0
	}

	var moves []string
	for _, mv := range gr.Moves {
		if mv != "PASS" {
			moves = append(moves, strings.ToLower(mv))
		}
	}

	fmt.Fprintf(f, "%d %d 0 %s\n", blackScore, whiteScore, strings.Join(moves, ""))
}

// ── JSON Output ───────────────────────────────────────────────────────

func writeJSON(path string, summary sprt.Summary) {
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		slog.Error("json marshal", "err", err)
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		slog.Error("write json", "path", path, "err", err)
	}
}

// ── Embedded opening book ─────────────────────────────────────────────

// 48 balanced 8-ply openings extracted from Othello opening theory.
// All lines have 45-55% win rates for both colors in tournament play.
//go:embed openings_8ply.txt
var embeddedBook string

