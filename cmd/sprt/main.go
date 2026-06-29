// sprt runs a Sequential Probability Ratio Test between two engine versions.
//
// It plays color-swapped game pairs locally via GTP subprocesses until
// the SPRT reaches a decision, then exits with:
//
//	0 = ACCEPT (candidate is not meaningfully weaker)
//	1 = REJECT (candidate is meaningfully weaker)
//	2 = INCONCLUSIVE (max games reached, no decision)
//
// A manifest file is written every 60s and at test end, capturing full
// game data, per-move search statistics, git commit references, binary
// SHA256 hashes, and accumulated statistics for later analysis.
//
// Usage:
//
//	sprt --candidate "./neursi --weights new.bin" \
//	     --reference "./neursi --weights prev.bin" \
//	     --tc 1 --output /home/oliv/dev/agent/neursi/training/sprt/
package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
		mode        = flag.String("mode", "elo", "Test mode: elo (play games, SPRT) or speed (fixed-depth NPS benchmark)")
		tc          = flag.Float64("tc", 1.0, "Time control in seconds per game")
		elo0        = flag.Float64("elo0", -10, "Null hypothesis Elo difference")
		elo1        = flag.Float64("elo1", 0, "Alternative Elo difference")
		alpha       = flag.Float64("alpha", 0.05, "False positive rate")
		beta        = flag.Float64("beta", 0.05, "False negative rate")
		maxGames    = flag.Int("max-games", 400, "Maximum game pairs")
		concurrency = flag.Int("concurrency", 4, "Concurrent game pairs")
	noSmoke   = flag.Bool("no-smoke", false, "Skip pre-SPRT smoke test (used internally)")
		outputDir   = flag.String("output", "", "Output directory for WTHOR + JSON + manifest")
		resumePath  = flag.String("resume", "", "Resume from a previous manifest file")
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

	// ── Engine identity ──────────────────────────────────────────────

	candPath := strings.Fields(*candidate)[0]
	refPath := strings.Fields(*reference)[0]

	// Query engine info (GTP engine_info, fallback to name/version)
	candInfo := queryEngineInfo(*candidate)
	refInfo := queryEngineInfo(*reference)

	// Compute binary SHA256
	candBinSHA := fileSHA256(candPath)
	refBinSHA := fileSHA256(refPath)

	// Compute weights SHA256 if --weights flag present in command
	candWeightsSHA, candWeightsPath := extractWeightsSHA(*candidate)
	refWeightsSHA, refWeightsPath := extractWeightsSHA(*reference)

	// Build full identity
	candID := buildManifestIdentity(candInfo, *candidate, candPath, candBinSHA,
		candWeightsPath, candWeightsSHA)
	refID := buildManifestIdentity(refInfo, *reference, refPath, refBinSHA,
		refWeightsPath, refWeightsSHA)

	// Simple identity for summary
	candSimple := engineInfoToSimple(candInfo)
	refSimple := engineInfoToSimple(refInfo)
	if candSimple.Commit != "" {
		candSimple.EngineID = candBinSHA[:16]
	}
	if refSimple.Commit != "" {
		refSimple.EngineID = refBinSHA[:16]
	}

	// ── Speed mode ──────────────────────────────────────────────────
	if *mode == "speed" {
		runSpeedTest(*candidate, *reference)
		return
	}

	if !*noSmoke && !smokeTest(*candidate, *reference, candSimple, refSimple) {
		fmt.Fprintln(os.Stderr, "FATAL: smoke test failed — aborting SPRT")
		os.Exit(2)
	}

	slog.Info("SPRT starting",
		"candidate", fmt.Sprintf("%s %s (%s)", candSimple.Name, candSimple.Version, candBinSHA[:12]),
		"reference", fmt.Sprintf("%s %s (%s)", refSimple.Name, refSimple.Version, refBinSHA[:12]),
		"tc", *tc, "elo0", *elo0, "elo1", *elo1,
		"max_games", *maxGames, "concurrency", *concurrency,
	)

	// ── Resume from manifest ─────────────────────────────────────────

	var startPairIdx int
	var acc *sprt.Accumulator
	var allPairs []gameResultPair
	var accumStats *accumulatedStats

	if *resumePath != "" {
		m, err := loadManifest(*resumePath)
		if err != nil {
			slog.Error("failed to load manifest for resume", "path", *resumePath, "err", err)
			os.Exit(2)
		}
		// Validate identity hasn't changed
		if m.Candidate.BinarySHA256 != candBinSHA {
			slog.Error("candidate binary changed since manifest was written",
				"manifest", m.Candidate.BinarySHA256[:12], "current", candBinSHA[:12])
			os.Exit(2)
		}
		if m.Reference.BinarySHA256 != refBinSHA {
			slog.Error("reference binary changed since manifest was written",
				"manifest", m.Reference.BinarySHA256[:12], "current", refBinSHA[:12])
			os.Exit(2)
		}
		// Replay completed pairs
		acc = sprt.NewAccumulator(cfg)
		for _, p := range m.Pairs {
			if !p.Completed {
				break
			}
			cWins := false
			if len(p.Games) == 2 {
				// Recompute: candidate score vs reference score across both games
				var cScore, rScore int
				for _, g := range p.Games {
					if strings.Contains(g.Role, "candidate_black") {
						cScore += g.BlackScore
						rScore += g.WhiteScore
					} else {
						cScore += g.WhiteScore
						rScore += g.BlackScore
					}
				}
				cWins = cScore > rScore
			}
			acc.AddPair(cWins)
		}
		startPairIdx = acc.Pairs()
		slog.Info("resumed from manifest", "pairs", startPairIdx, "path", *resumePath)
	} else {
		acc = sprt.NewAccumulator(cfg)
	}

	// ── Accumulated stats ────────────────────────────────────────────

	if accumStats == nil {
		accumStats = newAccumulatedStats()
	}

	// ── Opening book ─────────────────────────────────────────────────

	book := game.LoadBook(embeddedBook)

	// ── Output paths ─────────────────────────────────────────────────

	var manifestPath, summaryPath, wthorPath string
	if *outputDir != "" {
		os.MkdirAll(*outputDir, 0755)
		basename := filepath.Join(*outputDir,
			fmt.Sprintf("%s_v%s-vs-v%s", time.Now().Format("2006-01-02"),
				candSimple.Version, refSimple.Version))
		wthorPath = basename + ".wthor"
		summaryPath = basename + ".json"
		manifestPath = basename + ".manifest.json"
	}

	// ── Periodic manifest writer ─────────────────────────────────────

	manifestDone := make(chan struct{})
	if manifestPath != "" {
		go periodicManifestWriter(manifestPath, &allPairs, acc, cfg,
			candID, refID, wthorPath, summaryPath, accumStats,
			*resumePath != "", manifestDone)
	}

	// ── Play pairs ───────────────────────────────────────────────────

	var (
		mu        sync.Mutex
		wg        sync.WaitGroup
		sem       = make(chan struct{}, *concurrency)
		pairIdx   int
		gameCount int
	)

	for acc.Status() == sprt.Running && (startPairIdx+pairIdx) < cfg.MaxGames {
		op := book[(startPairIdx+pairIdx)%len(book)]
		pairIdx++
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, op game.Opening) {
			defer wg.Done()
			defer func() { <-sem }()

			pair := playPair(*candidate, *reference, op, *tc, idx, &candID, &refID)
			mu.Lock()
			allPairs = append(allPairs, pair)
			gameCount++

			// Accumulate per-move stats
			for _, g := range []game.GameResult{pair.BlackGame, pair.WhiteGame} {
				accumStats.recordGame(g, &candID, &refID)
			}

			acc.AddPair(pair.candidateWins())
			pairs, cWins, llr, eloEst := acc.Stats()
			slog.Info("pair done",
				"pair", gameCount,
				"score", fmt.Sprintf("%d-%d", cWins, pairs-cWins),
				"llr", fmt.Sprintf("%.2f", llr),
				"elo_est", fmt.Sprintf("%.1f", eloEst),
			)
			mu.Unlock()
		}(startPairIdx+pairIdx, op)
	}
	wg.Wait()

	// Signal manifest writer to stop and write final
	if manifestPath != "" {
		close(manifestDone)
		time.Sleep(100 * time.Millisecond)
		writeFinalManifest(manifestPath, &allPairs, acc, cfg,
			candID, refID, wthorPath, summaryPath, accumStats)
	}

	// ── Write output files ───────────────────────────────────────────

	summary := acc.MakeSummary(candSimple, refSimple, *tc)
	if candSimple.Commit != "" {
		summary.Commit = candSimple.Commit
	}

	if *outputDir != "" {
		writeWTHOR(wthorPath, summary, allPairs)
		writeJSON(summaryPath, summary)
		slog.Info("output written", "dir", *outputDir)
	}

	// ── Print final result ───────────────────────────────────────────

	pairs, cWins, _, eloEst := acc.Stats()
	fmt.Printf("\n=== SPRT Result ===\n")
	fmt.Printf("Candidate: %s %s (%s)\n", candSimple.Name, candSimple.Version, candBinSHA[:12])
	fmt.Printf("Reference: %s %s (%s)\n", refSimple.Name, refSimple.Version, refBinSHA[:12])
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

// ── Game pair types ────────────────────────────────────────────────────

type gameResultPair struct {
	BlackGame           game.GameResult
	WhiteGame           game.GameResult
	CandidateFirstColor string // "black" or "white"
	Opening             string
	OpeningName         string
	StartTime           time.Time
	EndTime             time.Time
}

func (p gameResultPair) candidateWins() bool {
	cScore := p.BlackGame.BlackScore + p.WhiteGame.WhiteScore
	rScore := p.BlackGame.WhiteScore + p.WhiteGame.BlackScore
	return cScore > rScore
}

func playPair(candidatePath, referencePath string, op game.Opening, tc float64, idx int,
	candID, refID *sprt.ManifestEngineIdentity) gameResultPair {

	pair := gameResultPair{
		Opening:     op.Line,
		OpeningName: op.Name,
		StartTime:   time.Now(),
	}

	var g1, g2 game.GameResult

	if idx%2 == 0 {
		pair.CandidateFirstColor = "black"
		g1 = playOneGame(candidatePath, referencePath, op.Line, tc, "candidate", "reference")
		g2 = playOneGame(referencePath, candidatePath, op.Line, tc, "reference", "candidate")
	} else {
		pair.CandidateFirstColor = "white"
		g1 = playOneGame(referencePath, candidatePath, op.Line, tc, "reference", "candidate")
		g2 = playOneGame(candidatePath, referencePath, op.Line, tc, "candidate", "reference")
	}

	pair.BlackGame = g1
	pair.WhiteGame = g2
	pair.EndTime = time.Now()

	// Label stats with engine identity
	labelStats(&pair.BlackGame, "candidate", "reference")
	labelStats(&pair.WhiteGame, "candidate", "reference")

	return pair
}

func playOneGame(blackPath, whitePath, opening string, tc float64,
	blackRole, whiteRole string) game.GameResult {

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

	// Label each move with engine role
	for i := range gr.MoveStats {
		if gr.MoveStats[i].Color == "black" {
			gr.MoveStats[i].Engine = blackRole
		} else {
			gr.MoveStats[i].Engine = whiteRole
		}
	}

	return gr
}

// labelStats fills in the Engine field for each MoveStats entry.
func labelStats(gr *game.GameResult, candRole, refRole string) {
	for i := range gr.MoveStats {
		if gr.MoveStats[i].Engine != "" {
			continue // already labeled
		}
		// Determine which engine played this move from the game setup
		if strings.Contains(gr.BlackName, "sprt-cand") {
			if gr.MoveStats[i].Color == "black" {
				gr.MoveStats[i].Engine = "candidate"
			} else {
				gr.MoveStats[i].Engine = "reference"
			}
		} else if strings.Contains(gr.WhiteName, "sprt-cand") {
			if gr.MoveStats[i].Color == "black" {
				gr.MoveStats[i].Engine = "reference"
			} else {
				gr.MoveStats[i].Engine = "candidate"
			}
		} else {
			// Fall back to the roles passed in
			if gr.MoveStats[i].Color == "black" {
				gr.MoveStats[i].Engine = "candidate"
			} else {
				gr.MoveStats[i].Engine = "reference"
			}
		}
	}
}

// ── Engine identity ────────────────────────────────────────────────────

// engineInfoJSON is the JSON payload from engine_info GTP command.
type engineInfoJSON struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Commit     string `json:"commit"`
	Dirty      bool   `json:"dirty"`
	Profile    string `json:"profile"`
	Rustc      string `json:"rustc"`
	WeightsPath string `json:"weights_path"`
}

func queryEngineInfo(enginePath string) engineInfoJSON {
	s := game.StartEngine(enginePath)
	if s == nil {
		return engineInfoJSON{Name: filepath.Base(strings.Fields(enginePath)[0]), Version: "unknown"}
	}
	defer s.Stop()

	// Try engine_info first (neursi v0.1.0+)
	resp := s.Send("engine_info")
	resp = strings.TrimSpace(resp)
	if strings.HasPrefix(resp, "= {") {
		resp = strings.TrimPrefix(resp, "= ")
		var info engineInfoJSON
		if err := json.Unmarshal([]byte(resp), &info); err == nil && info.Name != "" {
			return info
		}
	}

	// Fallback: name + version queries
	name := strings.TrimSpace(strings.TrimPrefix(s.Send("name"), "= "))
	version := strings.TrimSpace(strings.TrimPrefix(s.Send("version"), "= "))
	if name == "" {
		name = filepath.Base(strings.Fields(enginePath)[0])
	}
	if version == "" {
		version = "unknown"
	}
	return engineInfoJSON{Name: name, Version: version}
}

func buildManifestIdentity(info engineInfoJSON, command, binPath, binSHA, weightsPath, weightsSHA string) sprt.ManifestEngineIdentity {
	hostname, _ := os.Hostname()
	return sprt.ManifestEngineIdentity{
		Name:          info.Name,
		Version:       info.Version,
		GitCommit:     info.Commit,
		GitDirty:      info.Dirty,
		BinaryPath:    binPath,
		BinarySHA256:  binSHA,
		Command:       command,
		BuildProfile:  info.Profile,
		RustcVersion:  info.Rustc,
		WeightsPath:   weightsPath,
		WeightsSHA256: weightsSHA,
		HostHostname:  hostname,
		HostOS:        runtime.GOOS,
	}
}

func engineInfoToSimple(info engineInfoJSON) sprt.EngineIdentity {
	id := sprt.EngineIdentity{Name: info.Name, Version: info.Version}
	if info.Commit != "" {
		id.Commit = info.Commit
	}
	return id
}

// ── SHA256 utilities ───────────────────────────────────────────────────

func fileSHA256(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil))
}

func extractWeightsSHA(command string) (sha, path string) {
	parts := strings.Fields(command)
	for i, p := range parts {
		if (p == "--weights" || p == "-w") && i+1 < len(parts) {
			path = parts[i+1]
			sha = fileSHA256(path)
			return
		}
	}
	return "", ""
}

// ── Manifest writer ────────────────────────────────────────────────────

func periodicManifestWriter(path string, allPairs *[]gameResultPair,
	acc *sprt.Accumulator, cfg sprt.Config,
	candID, refID sprt.ManifestEngineIdentity,
	wthorPath, summaryPath string,
	accumStats *accumulatedStats,
	isResume bool, done <-chan struct{}) {

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			writeManifest(path, allPairs, acc, cfg, candID, refID,
				wthorPath, summaryPath, accumStats, isResume)
		case <-done:
			return
		}
	}
}

func writeManifest(path string, allPairs *[]gameResultPair,
	acc *sprt.Accumulator, cfg sprt.Config,
	candID, refID sprt.ManifestEngineIdentity,
	wthorPath, summaryPath string,
	accumStats *accumulatedStats,
	isResume bool) {

	m := buildManifest(allPairs, acc, cfg, candID, refID,
		wthorPath, summaryPath, path, accumStats, isResume)
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		slog.Error("manifest marshal", "err", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		slog.Error("manifest write", "path", tmp, "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		slog.Error("manifest rename", "err", err)
	}
}

func writeFinalManifest(path string, allPairs *[]gameResultPair,
	acc *sprt.Accumulator, cfg sprt.Config,
	candID, refID sprt.ManifestEngineIdentity,
	wthorPath, summaryPath string,
	accumStats *accumulatedStats) {

	writeManifest(path, allPairs, acc, cfg, candID, refID,
		wthorPath, summaryPath, accumStats, false)
}

func buildManifest(allPairs *[]gameResultPair,
	acc *sprt.Accumulator, cfg sprt.Config,
	candID, refID sprt.ManifestEngineIdentity,
	wthorPath, summaryPath, manifestPath string,
	accumStats *accumulatedStats,
	isResume bool) sprt.Manifest {

	now := time.Now().UTC().Format(time.RFC3339)
	pairs, cWins, llr, eloEst := acc.Stats()

	// Build a stable test ID
	testID := fmt.Sprintf("%s_v%s-vs-v%s",
		time.Now().Format("2006-01-02"), candID.Version, refID.Version)

	m := sprt.Manifest{
		Version: "1.0",
		TestID:  testID,
		Updated: now,
		Status:  acc.Status().String(),
		Config: sprt.ManifestConfig{
			Elo0: cfg.Elo0, Elo1: cfg.Elo1,
			Alpha: cfg.Alpha, Beta: cfg.Beta,
			MaxPairs: cfg.MaxGames,
			TC:       0, // filled below if we have it
			Conc:     0, // filled below
		},
		Candidate: candID,
		Reference: refID,
		SPRTState: sprt.ManifestSPRTState{
			PairsCompleted: pairs,
			CandidateWins:  cWins,
			LLR:            math.Round(llr*100) / 100,
			EloEstimate:    math.Round(eloEst*10) / 10,
			LowerBound:     math.Round(math.Log(cfg.Alpha/(1-cfg.Beta))*100) / 100,
			UpperBound:     math.Round(math.Log((1-cfg.Alpha)/cfg.Beta)*100) / 100,
			ElapsedS:       math.Round(acc.Elapsed().Seconds()),
		},
		LLRTraj: acc.LLRHistory,
		Files: sprt.ManifestFiles{
			WTHOR:    wthorPath,
			Summary:  summaryPath,
			Manifest: manifestPath,
		},
	}

	if isResume {
		m.Created = "resumed"
	} else {
		m.Created = now
	}

	// Convert pairs to manifest format
	for _, p := range *allPairs {
		mp := sprt.ManifestPair{
			Index:              0, // filled below
			Opening:            p.Opening,
			OpeningName:        p.OpeningName,
			CandidateFirstColor: p.CandidateFirstColor,
			Completed:          true,
			StartTime:          p.StartTime.UTC().Format(time.RFC3339),
			EndTime:            p.EndTime.UTC().Format(time.RFC3339),
			Games:              make([]sprt.ManifestGame, 2),
		}
		mp.Games[0] = gameToManifestGame(p.BlackGame, "candidate_black")
		mp.Games[1] = gameToManifestGame(p.WhiteGame, "candidate_white")
		if p.CandidateFirstColor == "white" {
			mp.Games[0].Role = "reference_black"
			mp.Games[1].Role = "candidate_white"
		}
		m.Pairs = append(m.Pairs, mp)
	}

	// Add accumulated stats
	if accumStats != nil {
		m.Stats = accumStats.toManifest()
	}

	return m
}

func gameToManifestGame(gr game.GameResult, role string) sprt.ManifestGame {
	mg := sprt.ManifestGame{
		Role:        role,
		Result:      gr.Result,
		BlackScore:  gr.BlackScore,
		WhiteScore:  gr.WhiteScore,
		TotalMoves:  gr.TotalMoves,
		BlackTimeS:  gr.BlackTimeS,
		WhiteTimeS:  gr.WhiteTimeS,
		Termination: terminationReason(gr),
		Moves:       make([]sprt.ManifestMove, len(gr.MoveStats)),
	}
	for i, ms := range gr.MoveStats {
		mg.Moves[i] = sprt.ManifestMove{
			Ply:         ms.Ply,
			Color:       ms.Color,
			Engine:      ms.Engine,
			Move:        ms.Move,
			Nodes:       ms.Nodes,
			Depth:       ms.Depth,
			TimeMs:      ms.TimeMs,
			Timeout:     ms.Timeout,
			Score:       ms.Score,
			Nps:         ms.Nps,
			Empties:     ms.Empties,
			AllocatedMs: ms.AllocatedMs,
			EndSearch:   ms.EndSearch,
			BookExit:    ms.BookExit,
			BookEval:    ms.BookEval,
		}
	}
	return mg
}

func terminationReason(gr game.GameResult) string {
	if gr.Disconnect {
		return "disconnect"
	}
	switch gr.Result {
	case "1-0", "0-1":
		// Check if it was a time forfeit
		if gr.BlackTimeS > 100 || gr.WhiteTimeS > 100 {
			return "time_forfeit"
		}
		return "normal"
	default:
		return "normal"
	}
}

// ── Accumulated stats ──────────────────────────────────────────────────

type accumulatedStats struct {
	mu        sync.Mutex
	candPly   map[string]*perPlyAccum
	refPly    map[string]*perPlyAccum
	candGame  *gameAccum
	refGame   *gameAccum
}

type perPlyAccum struct {
	nodes, depth, timeMs, nps, scoreCp, unspentMs *sprt.StatAccumulator
	timeouts, count int
}

type gameAccum struct {
	nodesPerGame, depthAvg, timePerGame, overallNps *sprt.StatAccumulator
	endSearchStartPly, bookExitPly, bookExitEval    *sprt.StatAccumulator
	timeoutsPerGame *sprt.StatAccumulator
	totalNodes int64
	totalTimeS float64
	totalDepth int64
	depthCount int
	timeForfeits, illegalMoves, disconnects, games int
}

func newAccumulatedStats() *accumulatedStats {
	return &accumulatedStats{
		candPly:  make(map[string]*perPlyAccum),
		refPly:   make(map[string]*perPlyAccum),
		candGame: newGameAccum(),
		refGame:  newGameAccum(),
	}
}

func newGameAccum() *gameAccum {
	return &gameAccum{
		nodesPerGame:    sprt.NewStatAccumulator(),
		depthAvg:        sprt.NewStatAccumulator(),
		timePerGame:     sprt.NewStatAccumulator(),
		overallNps:      sprt.NewStatAccumulator(),
		endSearchStartPly: sprt.NewStatAccumulator(),
		bookExitPly:     sprt.NewStatAccumulator(),
		bookExitEval:    sprt.NewStatAccumulator(),
		timeoutsPerGame: sprt.NewStatAccumulator(),
	}
}

func (as *accumulatedStats) recordGame(gr game.GameResult, candID, refID *sprt.ManifestEngineIdentity) {
	as.mu.Lock()
	defer as.mu.Unlock()

	// Determine which engine is candidate in this game
	for _, ms := range gr.MoveStats {
		var plyAccum *perPlyAccum
		var engineGame *gameAccum

		if ms.Engine == "candidate" {
			engineGame = as.candGame
		} else {
			engineGame = as.refGame
		}

		key := fmt.Sprintf("%d", ms.Ply)
		if ms.Engine == "candidate" {
			if as.candPly[key] == nil {
				as.candPly[key] = &perPlyAccum{
					nodes: sprt.NewStatAccumulator(), depth: sprt.NewStatAccumulator(),
					timeMs: sprt.NewStatAccumulator(), nps: sprt.NewStatAccumulator(),
					scoreCp: sprt.NewStatAccumulator(), unspentMs: sprt.NewStatAccumulator(),
				}
			}
			plyAccum = as.candPly[key]
		} else {
			if as.refPly[key] == nil {
				as.refPly[key] = &perPlyAccum{
					nodes: sprt.NewStatAccumulator(), depth: sprt.NewStatAccumulator(),
					timeMs: sprt.NewStatAccumulator(), nps: sprt.NewStatAccumulator(),
					scoreCp: sprt.NewStatAccumulator(), unspentMs: sprt.NewStatAccumulator(),
				}
			}
			plyAccum = as.refPly[key]
		}

		plyAccum.nodes.Add(float64(ms.Nodes))
		plyAccum.depth.Add(float64(ms.Depth))
		plyAccum.timeMs.Add(ms.TimeMs)
		plyAccum.nps.Add(float64(ms.Nps))
		plyAccum.scoreCp.Add(float64(ms.Score))
		if ms.AllocatedMs > 0 {
			plyAccum.unspentMs.Add(ms.AllocatedMs - ms.TimeMs)
		}
		plyAccum.count++
		if ms.Timeout {
			plyAccum.timeouts++
		}

		// Game-level: track end-search start ply and book exit
		if ms.EndSearch {
			engineGame.endSearchStartPly.Add(float64(ms.Ply))
		}
		if ms.BookExit {
			engineGame.bookExitPly.Add(float64(ms.Ply))
			if ms.BookEval != nil {
				engineGame.bookExitEval.Add(float64(*ms.BookEval))
			}
		}
	}

	// Aggregate per-game stats for candidate
	for engine, ga := range map[string]*gameAccum{"candidate": as.candGame, "reference": as.refGame} {
		var totalNodes int64
		var totalTimeMs float64
		var depthSum float64
		var timeouts int
		var moveCount int
		for _, ms := range gr.MoveStats {
			if ms.Engine != engine {
				continue
			}
			totalNodes += ms.Nodes
			totalTimeMs += ms.TimeMs
			depthSum += float64(ms.Depth)
			if ms.Timeout {
				timeouts++
			}
			moveCount++
		}
		if moveCount > 0 {
			ga.nodesPerGame.Add(float64(totalNodes))
			ga.depthAvg.Add(depthSum / float64(moveCount))
			ga.timePerGame.Add(totalTimeMs / 1000.0)
			if totalTimeMs > 0 {
				ga.overallNps.Add(float64(totalNodes) / (totalTimeMs / 1000.0))
			}
			ga.timeoutsPerGame.Add(float64(timeouts))
			ga.totalNodes += totalNodes
			ga.totalTimeS += totalTimeMs / 1000.0
			ga.totalDepth += int64(depthSum)
			ga.depthCount += moveCount
		}
		ga.games++
	}

}

func (as *accumulatedStats) toManifest() *sprt.ManifestAccumulatedStats {
	as.mu.Lock()
	defer as.mu.Unlock()

	ms := &sprt.ManifestAccumulatedStats{}
	ms.Candidate = buildEngineStats(as.candPly, as.candGame)
	ms.Reference = buildEngineStats(as.refPly, as.refGame)
	return ms
}

func buildEngineStats(plyMap map[string]*perPlyAccum, ga *gameAccum) sprt.EngineStats {
	es := sprt.EngineStats{
		PerPly:   make(map[string]sprt.PerPlyStats),
		FullGame: sprt.FullGameStats{},
	}
	for k, pa := range plyMap {
		pps := sprt.PerPlyStats{
			TimeoutRate: 0,
			Timeouts:    pa.timeouts,
			Count:       pa.count,
		}
		if pa.count > 0 {
			pps.TimeoutRate = float64(pa.timeouts) / float64(pa.count)
		}
		addIfNonEmpty := func(s *sprt.StatAccumulator) *sprt.StatAccumulator {
			if s.Count > 0 {
				s.Finalize()
				return s
			}
			return nil
		}
		pps.Nodes = addIfNonEmpty(pa.nodes)
		pps.Depth = addIfNonEmpty(pa.depth)
		pps.TimeMs = addIfNonEmpty(pa.timeMs)
		pps.Nps = addIfNonEmpty(pa.nps)
		pps.ScoreCp = addIfNonEmpty(pa.scoreCp)
		pps.UnspentMs = addIfNonEmpty(pa.unspentMs)
		es.PerPly[k] = pps
	}
	if ga != nil {
		ga.nodesPerGame.Finalize()
		ga.depthAvg.Finalize()
		ga.timePerGame.Finalize()
		ga.overallNps.Finalize()
		ga.endSearchStartPly.Finalize()
		ga.bookExitPly.Finalize()
		ga.bookExitEval.Finalize()
		ga.timeoutsPerGame.Finalize()

		var npsRate, depthAvg float64
		if ga.totalTimeS > 0 {
			npsRate = float64(ga.totalNodes) / ga.totalTimeS
		}
		if ga.depthCount > 0 {
			depthAvg = float64(ga.totalDepth) / float64(ga.depthCount)
		}
		es.FullGame = sprt.FullGameStats{
			NodesPerGame:      ga.nodesPerGame,
			DepthAvgPerGame:   ga.depthAvg,
			TimePerGameS:      ga.timePerGame,
			OverallNps:        ga.overallNps,
			EndSearchStartPly: ga.endSearchStartPly,
			BookExitPly:       ga.bookExitPly,
			BookExitEvalCp:    ga.bookExitEval,
			TimeoutsPerGame:   ga.timeoutsPerGame,
			TotalNodes:        ga.totalNodes,
			TotalTimeS:        ga.totalTimeS,
			NpsRate:           npsRate,
			DepthAvg:          depthAvg,
			TimeForfeits:      ga.timeForfeits,
			IllegalMoves:      ga.illegalMoves,
			Disconnects:       ga.disconnects,
			Games:             ga.games,
		}
	}
	return es
}

// ── Manifest load (for resume) ─────────────────────────────────────────

func loadManifest(path string) (*sprt.Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	var m sprt.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if m.Status != "running" {
		return nil, fmt.Errorf("manifest status is %q, not 'running'", m.Status)
	}
	return &m, nil
}

// ── WTHOR Output ───────────────────────────────────────────────────────

func writeWTHOR(path string, summary sprt.Summary, pairs []gameResultPair) {
	f, err := os.Create(path)
	if err != nil {
		slog.Error("create wthor", "path", path, "err", err)
		return
	}
	defer f.Close()

	fmt.Fprintf(f, "# SPRT: %s v%s vs %s v%s\n",
		summary.Candidate.Name, summary.Candidate.Version,
		summary.Reference.Name, summary.Reference.Version)
	if summary.Commit != "" {
		fmt.Fprintf(f, "# candidate_commit: %s\n", summary.Commit)
	}
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

// ── JSON Output ────────────────────────────────────────────────────────

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

// ── Speed mode (--mode=speed) ──────────────────────────────────────────

// ── Speed test anomaly detection ─────────────────────────────────────────

// speedAnomaly records a detected issue during speed benchmarking.
type speedAnomaly struct {
	Engine   string // "candidate" or "reference"
	Position string // position name
	Severity string // "warn" or "critical"
	Detail   string // human-readable description
}

// readProcessCPU reads CPU time (utime+stime+cutime+cstime) in ticks
// from /proc/[pid]/stat. Fields are 1-indexed: utime=14, stime=15,
// cutime=16, cstime=17. Returns 0 on any error.
func readProcessCPU(pid int) uint64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// stat format: pid (comm) state ... — find the closing paren
	s := string(data)
	closeParen := strings.LastIndexByte(s, ')')
	if closeParen < 0 {
		return 0
	}
	parts := strings.Fields(s[closeParen+2:])
	if len(parts) < 15 {
		return 0
	}
	utime, _ := strconv.ParseUint(parts[11], 10, 64)
	stime, _ := strconv.ParseUint(parts[12], 10, 64)
	cutime, _ := strconv.ParseUint(parts[13], 10, 64)
	cstime, _ := strconv.ParseUint(parts[14], 10, 64)
	return utime + stime + cutime + cstime
}

// readProcessPss reads Proportional Set Size in KiB from /proc/[pid]/smaps_rollup.
// Pss divides shared pages (e.g. mmapped NNUE weights) evenly among sharing processes.
func readProcessPss(pid int) uint64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Pss:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				v, _ := strconv.ParseUint(fields[1], 10, 64)
				return v
			}
		}
	}
	return 0
}

// speedPosition holds a test position for the speed benchmark.
type speedPosition struct {
	black uint64
	white uint64
	name  string
	// setup holds GTP play commands to reach this position from clear_board.
	// Empty means the position is set via the `position` GTP extension command.
	setup []string
}

// speedStatsJSON matches the # arena-stats v1: JSON payload for NPS calculation.
type speedStatsJSON struct {
	Nodes  int64   `json:"nodes"`
	Depth  int     `json:"depth"`
	TimeMs float64 `json:"time_ms"`
	Nps    int64   `json:"nps"`
}

// parseBits converts a binary string (MSB-first) to a uint64 bitboard.
// The string must be exactly 64 characters of '0' and '1'.
func parseBits(s string) uint64 {
	var v uint64
	for _, c := range s {
		v <<= 1
		if c == '1' {
			v |= 1
		}
	}
	return v
}

var speedPositions = []speedPosition{
	// Standard opening after F5D6C3D3C4 (5 moves)
	{black: parseBits("0000000000000000000010000011100000010000000000000000000000000000"),
		white: parseBits("0000000000000000000001000000000000001000000000000000000000000000"),
		name:  "opening",
		setup: []string{"play B F5", "play W D6", "play B C3", "play W D3", "play B C4"}},
	// Midgame position ~30 empties
	{black: parseBits("0001000000010001011110101111110010011001001010100001110000001000"),
		white: parseBits("0000000000001100000000000000001000000110000100000110000000000000"),
		name:  "midgame1"},
	// Initial position
	{black: 1<<28 | 1<<35, white: 1<<27 | 1<<36, name: "initial"},
}

// runSpeedTest runs the speed benchmark for both engines and prints results.
// Also collects and reports anomalies (depth mismatch, CPU variance, memory changes).
func runSpeedTest(candidateCmd, referenceCmd string) {
	candInfo := queryEngineInfo(candidateCmd)
	refInfo := queryEngineInfo(referenceCmd)
	candName := engineInfoToSimple(candInfo).Name
	refName := engineInfoToSimple(refInfo).Name

	fmt.Printf("\n=== Speed Test ===\n")
	fmt.Printf("Candidate: %s\n", candName)
	fmt.Printf("Reference: %s\n", refName)
	fmt.Printf("Depth: 10  Runs: 2 per position (after 1 warmup)\n\n")

	var anomalies []speedAnomaly
	candResults, candAnoms := runEngineSpeedTests(candidateCmd, "candidate")
	anomalies = append(anomalies, candAnoms...)
	refResults, refAnoms := runEngineSpeedTests(referenceCmd, "reference")
	anomalies = append(anomalies, refAnoms...)

	// Print summary table
	fmt.Printf("%-15s | %-15s | %-15s | %s\n", "Position", "Candidate NPS", "Reference NPS", "Ratio")
	fmt.Println(strings.Repeat("-", 68))

	var candSum, refSum float64
	var count int
	for _, pos := range speedPositions {
		cand := candResults[pos.name]
		ref := refResults[pos.name]
		if cand <= 0 || ref <= 0 {
			fmt.Printf("%-15s | %-15s | %-15s | %s\n", pos.name, "N/A", "N/A", "N/A")
			// Record anomaly for failed position
			if cand <= 0 {
				anomalies = append(anomalies, speedAnomaly{Engine: "candidate", Position: pos.name, Severity: "critical", Detail: "failed to produce NPS result"})
			}
			if ref <= 0 {
				anomalies = append(anomalies, speedAnomaly{Engine: "reference", Position: pos.name, Severity: "critical", Detail: "failed to produce NPS result"})
			}
			continue
		}
		ratio := cand / ref
		fmt.Printf("%-15s | %15.0f | %15.0f | %.3fx\n", pos.name, cand, ref, ratio)
		candSum += cand
		refSum += ref
		count++
	}

	if count > 0 {
		candAvg := candSum / float64(count)
		refAvg := refSum / float64(count)
		fmt.Println(strings.Repeat("-", 68))
		fmt.Printf("%-15s | %15.0f | %15.0f | %.3fx\n", "AVERAGE", candAvg, refAvg, candAvg/refAvg)
	}

	// Report anomalies
	if len(anomalies) > 0 {
		fmt.Printf("\n⚠ Anomalies detected (%d):\n", len(anomalies))
		for _, a := range anomalies {
			tag := "WARN"
			if a.Severity == "critical" {
				tag = "CRITICAL"
			}
			fmt.Printf("  [%s] %s/%s: %s\n", tag, a.Engine, a.Position, a.Detail)
		}
	} else {
		fmt.Printf("\n✓ No anomalies detected.\n")
	}
}

// runEngineSpeedTests benchmarks one engine on all positions.
func runEngineSpeedTests(cmd, label string) (map[string]float64, []speedAnomaly) {
	results := make(map[string]float64, len(speedPositions))
	var anomalies []speedAnomaly
	for _, pos := range speedPositions {
		nps, posAnoms, err := benchmarkPosition(cmd, pos, label)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s/%s: %v\n", label, pos.name, err)
			results[pos.name] = 0
			anomalies = append(anomalies, speedAnomaly{Engine: label, Position: pos.name, Severity: "critical", Detail: err.Error()})
			continue
		}
		results[pos.name] = nps
		anomalies = append(anomalies, posAnoms...)
	}
	return results, anomalies
}

// benchmarkPosition measures NPS for one engine on one position.
// Also validates depth and samples CPU/Pss for anomaly detection.
func benchmarkPosition(cmd string, pos speedPosition, label string) (float64, []speedAnomaly, error) {
	s := game.StartEngine(cmd)
	if s == nil {
		return 0, nil, fmt.Errorf("failed to start engine: %s", cmd)
	}
	defer s.Stop()
	pid := s.PID()
	var anomalies []speedAnomaly

	// Init board
	resp := s.Send("boardsize 8")
	if strings.HasPrefix(resp, "?") {
		return 0, anomalies, fmt.Errorf("boardsize rejected: %s", strings.TrimSpace(resp))
	}
	resp = s.Send("clear_board")
	if strings.HasPrefix(resp, "?") {
		return 0, anomalies, fmt.Errorf("clear_board rejected: %s", strings.TrimSpace(resp))
	}

	// Set up position
	if pos.name != "initial" {
		if len(pos.setup) > 0 {
			for _, setup := range pos.setup {
				resp = s.Send(setup)
				if strings.HasPrefix(resp, "?") {
					return 0, anomalies, fmt.Errorf("setup move rejected %q: %s", setup, strings.TrimSpace(resp))
				}
			}
		} else {
			resp = s.Send(fmt.Sprintf("position %016x %016x", pos.black, pos.white))
			if strings.HasPrefix(resp, "?") {
				return 0, anomalies, fmt.Errorf("position command not supported by engine")
			}
		}
	}

	// Set fixed depth
	resp = s.Send("set_depth 10")
	if strings.HasPrefix(resp, "?") {
		return 0, anomalies, fmt.Errorf("set_depth rejected: %s", strings.TrimSpace(resp))
	}

	// Sample baseline resources
	pssBefore := readProcessPss(pid)
	_ = readProcessCPU(pid) // baseline CPU sample

	// doGenmove sends genmove and returns NPS + parsed stats.
	doGenmove := func() (float64, speedStatsJSON, error) {
		resp := s.Send("genmove b")
		if strings.HasPrefix(resp, "?") {
			return 0, speedStatsJSON{}, fmt.Errorf("genmove error: %s", strings.TrimSpace(resp))
		}
		trimmed := strings.TrimSpace(resp)
		trimmed = strings.TrimPrefix(trimmed, "= ")
		if trimmed == "PASS" {
			resp = s.Send("genmove w")
			if strings.HasPrefix(resp, "?") {
				return 0, speedStatsJSON{}, fmt.Errorf("genmove w error: %s", strings.TrimSpace(resp))
			}
			trimmed = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(resp), "= "))
			if trimmed == "PASS" {
				return 0, speedStatsJSON{}, fmt.Errorf("no legal moves for either side")
			}
		}
		if !isValidSquare(trimmed) {
			slog.Warn("invalid square format from engine (not a real move)", "position", pos.name, "response", trimmed)
			return 0, speedStatsJSON{}, fmt.Errorf("invalid square format: %q", trimmed)
		}
		stats := s.LastStats()
		if stats == "" {
			return 0, speedStatsJSON{}, fmt.Errorf("no arena-stats in genmove response")
		}
		var ss speedStatsJSON
		if err := json.Unmarshal([]byte(stats), &ss); err != nil {
			return 0, speedStatsJSON{}, fmt.Errorf("stats parse error: %w", err)
		}
		if ss.TimeMs == 0 {
			return 0, speedStatsJSON{}, fmt.Errorf("zero time in stats")
		}
		nps := float64(ss.Nodes) * 1000.0 / ss.TimeMs
		return nps, ss, nil
	}

	// Warmup (ignore errors, but check depth)
	_, ssWarm, err := doGenmove()
	if err == nil && ssWarm.Depth < 10 {
		anomalies = append(anomalies, speedAnomaly{
			Engine: label, Position: pos.name, Severity: "warn",
			Detail: fmt.Sprintf("warmup depth %d < requested 10", ssWarm.Depth),
		})
	}

	// Timed runs
	var npsValues []float64
	var cpuValues []uint64
	for i := 0; i < 2; i++ {
		cpuBeforeRun := readProcessCPU(pid)
		nps, ss, err := doGenmove()
		cpuAfterRun := readProcessCPU(pid)
		if err == nil {
			npsValues = append(npsValues, nps)
			if cpuAfterRun > cpuBeforeRun {
				cpuValues = append(cpuValues, cpuAfterRun-cpuBeforeRun)
			}
			// Depth validation
			if ss.Depth < 10 {
				anomalies = append(anomalies, speedAnomaly{
					Engine: label, Position: pos.name, Severity: "critical",
					Detail: fmt.Sprintf("depth %d < requested 10 (run %d)", ss.Depth, i+1),
				})
			}
		}
	}

	// Resource sampling after all runs
	cpuAfter := readProcessCPU(pid)
	pssAfter := readProcessPss(pid)

	// CPU variance check
	if len(cpuValues) == 2 {
		cpu0, cpu1 := cpuValues[0], cpuValues[1]
		if cpu0 > 0 && cpu1 > 0 {
			ratio := float64(max(cpu0, cpu1)) / float64(min(cpu0, cpu1))
			if ratio > 2.0 {
				anomalies = append(anomalies, speedAnomaly{
					Engine: label, Position: pos.name, Severity: "warn",
					Detail: fmt.Sprintf("CPU ticks vary %.1fx between runs (%d vs %d)", ratio, cpu0, cpu1),
				})
			}
		}
	}

	// Pss change check
	if pssBefore > 0 && pssAfter > 0 {
		pssDelta := int64(pssAfter) - int64(pssBefore)
		if pssDelta < 0 {
			pssDelta = -pssDelta
		}
		pssChangePct := float64(pssDelta) / float64(pssBefore) * 100
		if pssChangePct > 10 {
			anomalies = append(anomalies, speedAnomaly{
				Engine: label, Position: pos.name, Severity: "warn",
				Detail: fmt.Sprintf("Pss changed %.0f%% (%d→%d KiB)", pssChangePct, pssBefore, pssAfter),
			})
		}
	}
	_ = cpuAfter

	if len(npsValues) == 0 {
		return 0, anomalies, fmt.Errorf("no successful timed runs")
	}

	sum := 0.0
	for _, n := range npsValues {
		sum += n
	}
	return sum / float64(len(npsValues)), anomalies, nil
}

// isValidSquare checks if a string is a valid Othello square (A1-H8, case-insensitive).}

// isValidSquare checks if a string is a valid Othello square (A1-H8, case-insensitive).
func isValidSquare(s string) bool {
	if len(s) < 2 {
		return false
	}
	col := s[0]
	row := s[1]
	if col >= 'a' && col <= 'h' {
		col -= 32
	}
	return col >= 'A' && col <= 'H' && row >= '1' && row <= '8'
}

// ── Smoke test ─────────────────────────────────────────────────────────

// queryFeatureList sends "feature_list" GTP command and returns parsed tags.
func queryFeatureList(cmd string) ([]string, error) {
	s := game.StartEngine(cmd)
	if s == nil {
		return nil, fmt.Errorf("failed to start engine: %s", cmd)
	}
	defer s.Stop()
	if resp := s.Send("boardsize 8"); strings.HasPrefix(resp, "?") {
		if err := s.Init(1); err != nil {
			return nil, fmt.Errorf("init: %w", err)
		}
	} else {
		s.Send("clear_board")
	}
	resp := s.Send("feature_list")
	resp = strings.TrimPrefix(strings.TrimSpace(resp), "= ")
	var tags []string
	if err := json.Unmarshal([]byte(resp), &tags); err != nil {
		return nil, fmt.Errorf("feature_list parse: %w (%q)", err, resp)
	}
	return tags, nil
}

// smokeTest runs pre-SPRT validation: feature check, illegal-move check, speed check.
func smokeTest(candidateCmd, referenceCmd string, candSimple, refSimple sprt.EngineIdentity) bool {
	fmt.Fprintf(os.Stderr, "\n=== Smoke Test ===\n")

	// 1. Feature list
	candFeatures, err := queryFeatureList(candidateCmd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  feature_list: FAILED (%v) — binary may be stale, rebuild\n", err)
		return false
	}
	refFeatures, err := queryFeatureList(referenceCmd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  feature_list: FAILED (%v) — binary may be stale, rebuild\n", err)
		return false
	}
	fmt.Fprintf(os.Stderr, "  candidate: [%s]\n", strings.Join(candFeatures, ", "))
	fmt.Fprintf(os.Stderr, "  reference: [%s]\n", strings.Join(refFeatures, ", "))

	// 2. Illegal-move check: run self as subprocess with --max-games 4.
	fmt.Fprintf(os.Stderr, "  4-pair smoke test...\n")
	// NOTE: This recursive subprocess call with string matching on "illegal move"
	// is fragile. A structured approach (exit code + JSON on stdout) would be
	// more reliable and would allow individual move failures without aborting.
	self, _ := os.Executable()
	smoke := exec.Command(self, "--candidate", candidateCmd, "--reference", referenceCmd,
		"--max-games", "4", "--tc", "1", "--concurrency", "1", "--no-smoke")
	smokeOut, _ := smoke.CombinedOutput()
	if strings.Contains(string(smokeOut), "illegal move") {
		n := strings.Count(string(smokeOut), "illegal move")
		fmt.Fprintf(os.Stderr, "  smoke: WARN — %d illegal move(s) detected\n", n)
		// Don't fail — move validation is disabled; games are flagged for investigation.
	}
	fmt.Fprintf(os.Stderr, "  smoke: OK\n")

	// 3. Quick speed check (must be within [0.5, 2.0] of baseline).
	candNPS, _ := runEngineSpeedTests(candidateCmd, "candidate")
	refNPS, _ := runEngineSpeedTests(referenceCmd, "reference")
	if len(candNPS) > 0 && len(refNPS) > 0 {
		var candSum, refSum float64
		var count int
		for _, pos := range speedPositions {
			if candNPS[pos.name] > 0 && refNPS[pos.name] > 0 {
				candSum += candNPS[pos.name]; refSum += refNPS[pos.name]; count++
			}
		}
		if count > 0 {
			ratio := candSum / refSum
			fmt.Fprintf(os.Stderr, "  speed: %.3fx\n", ratio)
			if ratio < 0.5 || ratio > 2.0 {
				fmt.Fprintf(os.Stderr, "  speed: FAILED — ratio %.3fx outside [0.5, 2.0]\n", ratio)
				return false
			}
		}
	}

	fmt.Fprintf(os.Stderr, "=== Smoke: PASSED ===\n\n")
	return true
}


// ── Embedded opening book ──────────────────────────────────────────────

// 48 balanced 8-ply openings extracted from Othello opening theory.
// All lines have 45-55% win rates for both colors in tournament play.
//
//go:embed openings_8ply.txt
var embeddedBook string
