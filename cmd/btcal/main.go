// btcal — Bayesian Bradley-Terry calibration of the H* + S* ladders on ONE
// scale, anchored at the Edax d6-clean reference (≈2000 Elo).
//
// Motivation: vs-anchor win rates saturate for the upper S* levels (≥97% vs
// d6), where pairwise Elo is ill-conditioned. Instead of manual anchor
// chains, btcal pools ALL games — the existing vs-d6 aggregates (h-calib,
// calibrate --sparring runs with identical configs) plus NEW close-matchup
// games (adjacent S levels) — into a single Zermelo Bradley-Terry fit
// (arena/internal/elo), which is exactly what the Arena web UI uses for its
// "BT" Elo estimates. Bootstrap resampling gives per-level CIs.
//
// Usage:
//
//	btcal --hgtp hgtp --eval eval.dat \
//	      --hcalib h-calib.json --scalib2 s-calib2.json --scalib3 s-calib3.json \
//	      [--pairs-games 400] [--budget 2000] [--concurrency 16] \
//	      [--bootstrap 300] --out bt-calib.json
//
// Optional diagnostics:
//
//	--games-log games.jsonl   write one JSON line per played game (result,
//	                          per-side clock seconds, timeout flags).
//	--pairs "19-20"           restrict the adjacent-pair schedule to a
//	                          comma-separated list of "A-B" pairs.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/neoliv/arena/internal/elo"
	"github.com/neoliv/arena/internal/game"
)

// ── Players ────────────────────────────────────────────────────────────
// Fixed ID space: 0 = d6-clean reference (anchor), 1-20 = H1-H20,
// 21-40 = S1-S20 (final v3 configs).
const (
	refID   = 0
	hBase   = 1  // H1 -> hBase+0, H20 -> hBase+19
	sBase   = 21 // S1 -> sBase+0, S20 -> sBase+19
	nH      = 20
	nS      = 20
	nPlayer = 1 + nH + nS
)

var sNames = []string{
	"d4c", "d6fw2", "d6fw1.5", "d6fw0.5", "d6fw0.75", "d6fw0.25", "d6fw0.05",
	"d7fw0.5", "d7c", "d8g5", "d8c", "d10c", "d12c",
	"d14es10", "d18es12", "d24es14", "d30es16", "d36es16", "d42es16", "d48es16",
}

type pairAgg struct {
	wins, draws, losses int
}

func (a pairAgg) total() int { return a.wins + a.draws + a.losses }

// pair key: (minID, maxID) — same convention as elo.pairKey.
func pk(a, b int) [2]int {
	if a < b {
		return [2]int{a, b}
	}
	return [2]int{b, a}
}

// ── Config-version mapping (old calibrate tables -> final v3 S levels) ──
// Level N of run v1 (s-calib2, first new table) -> v3 S level, or 0 = drop.
// Level N of run v1 (s-calib2) -> final v3 S level, or 0 = drop.
// Index N-1 (level 1 = index 0). The leading-0 style of an early draft
// shifted every level by one and silently corrupted all S vs-d6 edges —
// the arrays were re-verified entry by entry (Aug 12 2026).
var v1ToV3 = []int{
	1, 2, 3, 0, 0, 7, 9, 8, 0, 10, 11, 0, 12, 0, 13, 14, 15, 16, 17, 19,
}

// Level N of run v2/v3 (s-calib3, confirm run) -> v3 S level (S4/S5 swap).
var v3ToV3 = []int{
	1, 2, 3, 5, 4, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
}

// ── Existing aggregate JSON (calibrate output) ─────────────────────────
type levelResult struct {
	Level    int     `json:"level"`
	Games    int     `json:"games"`
	Wins     int     `json:"wins"`
	Draws    int     `json:"draws"`
	Losses   int     `json:"losses"`
	WinRate  float64 `json:"win_rate"`
	EloVsRef float64 `json:"elo_vs_ref"`
}

func loadAggregates(path, kind string, mapping []int, pairs map[[2]int]pairAgg) {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Error("read aggregates", "path", path, "err", err)
		os.Exit(1)
	}
	var res []levelResult
	if err := json.Unmarshal(data, &res); err != nil {
		slog.Error("parse aggregates", "path", path, "err", err)
		os.Exit(1)
	}
	used := 0
	for _, r := range res {
		if kind == "H" {
			// JSON wins are from the candidate's (H level's) perspective.
			addPair(pairs, hBase+r.Level-1, refID, r.Wins, r.Draws, r.Losses)
			used++
			continue
		}
		s := 0
		if r.Level-1 < len(mapping) {
			s = mapping[r.Level-1]
		}
		if s == 0 {
			continue // config not in the final ladder — drop
		}
		// calibrate reports wins from the candidate's perspective.
		addPair(pairs, sBase+s-1, refID, r.Wins, r.Draws, r.Losses)
		used++
	}
	slog.Info("reused aggregates", "kind", kind, "levels", used)
}

func addPair(pairs map[[2]int]pairAgg, a, b, wins, draws, losses int) {
	// wins are from a's perspective; orient so ID order is preserved.
	// Elo fitter is order-independent (pair key + result), so just store
	// wins from the lower-ID player's perspective... simpler: store from a.
	key := pk(a, b)
	// flip if the stored direction is (a=loser) — the fitter's GameRecord
	// uses BlackID/WhiteID order; we expand later from the perspective of
	// `a` as the "black" side of this pair.
	p := pairs[key]
	if a < b {
		p.wins += wins
		p.draws += draws
		p.losses += losses
	} else {
		p.losses += wins
		p.draws += draws
		p.wins += losses
	}
	pairs[key] = p
}

// ── New adjacent-pair schedule ─────────────────────────────────────────
// Close matchups for the upper ladder where vs-d6 saturates. Each pair is
// (S-level a, S-level b) — the schedule below is in (lower, higher) order.
var adjacentPairs = [][2]int{
	{11, 12}, {12, 13}, {13, 14}, {14, 15}, {15, 16},
	{16, 17}, {17, 18}, {18, 19}, {19, 20},
}

// ── Per-game diagnostics log ───────────────────────────────────────────
// gameRecord is one JSON line per played game. timeout_side is the color
// that exceeded the arena clock (tc*1.05 s/side) and thus lost on time;
// such losses are forced by the arena regardless of the board and are NOT
// flagged as Disconnect (so they were previously invisible in aggregates).
type gameRecord struct {
	Pair        [2]int  `json:"pair"`
	Game        int     `json:"game"`
	Opening     string  `json:"opening"`
	Result      string  `json:"result"`
	Moves       int     `json:"moves"`
	BlackTimeS  float64 `json:"black_time_s"`
	WhiteTimeS  float64 `json:"white_time_s"`
	TimeoutSide string  `json:"timeout_side"` // "" | "black" | "white"
	Disconnect  bool    `json:"disconnect"`
	Investigate bool    `json:"investigate"`
	Skipped     bool    `json:"skipped"`
	// Per-move search stats (from hgtp `# arena-stats` comments):
	// how many moves hit the per-move budget (time management timeouts).
	PMMoves    int     `json:"pm_moves"`
	PMTimeouts int     `json:"pm_timeouts"`
	PMAvgMs    float64 `json:"pm_avg_ms"`
}

type gamesLog struct {
	f  *os.File
	w  *bufio.Writer
	mu sync.Mutex
}

func newGamesLog(path string) *gamesLog {
	if path == "" {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		slog.Error("create games-log", "path", path, "err", err)
		os.Exit(1)
	}
	return &gamesLog{f: f, w: bufio.NewWriter(f)}
}

func (g *gamesLog) write(rec gameRecord) {
	if g == nil {
		return
	}
	b, err := json.Marshal(rec)
	if err != nil {
		slog.Error("games-log marshal", "err", err)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.w.Write(append(b, '\n'))
}

func (g *gamesLog) close() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.w.Flush()
	g.f.Close()
}

func main() {
	var (
		hgtpPath    = flag.String("hgtp", "", "path to the hgtp GTP binary")
		evalPath    = flag.String("eval", "", "path to Edax eval.dat")
		hcalib      = flag.String("hcalib", "", "h-calib.json (H* vs d6 aggregates)")
		scalib2     = flag.String("scalib2", "", "s-calib2.json (v1 table, identical-config games)")
		scalib3     = flag.String("scalib3", "", "s-calib3.json (v3 confirm table)")
		pairsGames  = flag.Int("pairs-games", 400, "games per adjacent pair")
		budget      = flag.Int("budget", 2000, "per-move budget in ms")
		tc          = flag.Float64("tc", 90.0, "sudden-death seconds per side")
		concurrency = flag.Int("concurrency", 16, "parallel game pairs")
		bootstrapN  = flag.Int("bootstrap", 300, "bootstrap resamples")
		outPath     = flag.String("out", "bt-calib.json", "output JSON")
		bookPath    = flag.String("book", "openings_8ply.txt", "fair openings file")
		fitOnly     = flag.Bool("fit-only", false, "skip the adjacent-pair schedule; fit from the loaded aggregates only")
		pairsFile   = flag.String("pairs-file", "", "JSON of pre-played pair aggregates {\"11-12\":{\"w\":77,\"d\":22,\"l\":301},...}")
		gamesLogP   = flag.String("games-log", "", "append one JSON line per played game to this file")
		pairsSel    = flag.String("pairs", "", "restrict schedule to comma-separated A-B pairs (e.g. \"19-20,18-19\"); empty = all")
	)
	flag.Parse()
	if *hgtpPath == "" || *evalPath == "" || *hcalib == "" {
		slog.Error("--hgtp, --eval and --hcalib are required")
		os.Exit(1)
	}

	pairs := map[[2]int]pairAgg{}
	loadAggregates(*hcalib, "H", nil, pairs)
	if *scalib2 != "" {
		loadAggregates(*scalib2, "S", v1ToV3, pairs)
	}
	if *scalib3 != "" {
		loadAggregates(*scalib3, "S", v3ToV3, pairs)
	}
	if *pairsFile != "" {
		// Load pre-played pair aggregates (e.g. from a previous schedule
		// run that this run extends) — format {"11-12":{"w":77,"d":22,"l":301}}.
		data, err := os.ReadFile(*pairsFile)
		if err != nil {
			slog.Error("read pairs-file", "err", err)
			os.Exit(1)
		}
		var seeded map[string]struct {
			W int `json:"w"`
			D int `json:"d"`
			L int `json:"l"`
		}
		if err := json.Unmarshal(data, &seeded); err != nil {
			slog.Error("parse pairs-file", "err", err)
			os.Exit(1)
		}
		for k, v := range seeded {
			var a, b int
			if _, err := fmt.Sscanf(k, "%d-%d", &a, &b); err != nil {
				slog.Error("bad pair key", "key", k)
				os.Exit(1)
			}
			if a < 1 || a > 20 || b < 1 || b > 20 || a == b {
				slog.Error("pair out of range", "key", k)
				os.Exit(1)
			}
			addPair(pairs, sBase+a-1, sBase+b-1, v.W, v.D, v.L)
			slog.Info("seeded pair", "pair", k, "agg", fmt.Sprintf("%d-%d-%d", v.W, v.D, v.L))
		}
	}

	bookData, err := os.ReadFile(*bookPath)
	if err != nil {
		slog.Error("read book", "err", err)
		os.Exit(1)
	}
	book := game.LoadBook(string(bookData))
	if len(book) == 0 {
		slog.Error("no openings loaded")
		os.Exit(1)
	}

	// Restrict the adjacent-pair schedule if --pairs was given. Requested
	// pairs that are NOT in the default schedule (e.g. a 18-20 probe) are
	// added — --pairs is additive, not just a filter.
	schedule := adjacentPairs
	if *pairsSel != "" {
		want := map[[2]int]bool{}
		ordered := [][2]int{}
		for _, spec := range strings.Split(*pairsSel, ",") {
			spec = strings.TrimSpace(spec)
			if spec == "" {
				continue
			}
			var a, b int
			if n, _ := fmt.Sscanf(spec, "%d-%d", &a, &b); n != 2 {
				slog.Error("bad --pairs spec", "spec", spec)
				os.Exit(1)
			}
			if a > b {
				a, b = b, a
			}
			if a < 1 || a > 20 || b < 1 || b > 20 || a == b {
				slog.Error("bad --pairs spec", "spec", spec)
				os.Exit(1)
			}
			want[[2]int{a, b}] = true
			ordered = append(ordered, [2]int{a, b})
		}
		schedule = nil
		for _, pr := range adjacentPairs {
			if want[[2]int{pr[0], pr[1]}] || want[[2]int{pr[1], pr[0]}] {
				schedule = append(schedule, pr)
			}
		}
		for _, pr := range ordered {
			found := false
			for _, s := range schedule {
				if s == pr {
					found = true
					break
				}
			}
			if !found {
				schedule = append(schedule, pr)
			}
		}
		slog.Info("restricted schedule", "pairs", schedule)
	}

	logger := newGamesLog(*gamesLogP)
	defer logger.close()

	if *fitOnly {
		slog.Info("fit-only: skipping adjacent-pair games")
	} else {
		type pairResult struct {
			players     [2]int
			agg         pairAgg
			timeouts    int
			skippedBads int
		}
		var (
			mu   sync.Mutex
			outs = make([]pairResult, 0, len(schedule))
			wg   sync.WaitGroup
			sem  = make(chan struct{}, *concurrency)
		)
		for _, pr := range schedule {
			pr := pr
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				a, b := sBase+pr[0]-1, sBase+pr[1]-1
				cmdA := fmt.Sprintf("%s --sparring --level %d --eval %s --budget %d", *hgtpPath, pr[0], *evalPath, *budget)
				cmdB := fmt.Sprintf("%s --sparring --level %d --eval %s --budget %d", *hgtpPath, pr[1], *evalPath, *budget)
				slog.Info("playing adjacent pair", "S", pr, "games", *pairsGames)
				var agg pairAgg
				var timeouts, skippedBads int
				var pmMoves, pmTimeouts int
				var pmTime float64
				for g := 0; g < *pairsGames; g++ {
					op := book[(pr[0]*31+g)%len(book)]
					var gr game.GameResult
					lowerFirst := g%2 == 0
					if lowerFirst {
						gr = playOneGame(cmdA, cmdB, op.Line, *tc)
					} else {
						gr = playOneGame(cmdB, cmdA, op.Line, *tc)
					}
					// Per-move search stats: hgtp emits `# arena-stats`
					// after each genmove; a move whose search consumed its
					// per-move budget is a time-management timeout.
					gPMoves, gPMTimeouts := 0, 0
					var gPMTime float64
					for _, ms := range gr.MoveStats {
						gPMoves++
						gPMTime += ms.TimeMs
						if strings.Contains(ms.Flags, "timeout") ||
							(ms.AllocatedMs > 0 && ms.TimeMs >= ms.AllocatedMs*0.98) {
							gPMTimeouts++
						}
					}
					rec := gameRecord{
						Pair:        pr,
						Game:        g,
						Opening:     op.Line,
						Result:      gr.Result,
						Moves:       gr.TotalMoves,
						BlackTimeS:  gr.BlackTimeS,
						WhiteTimeS:  gr.WhiteTimeS,
						Disconnect:  gr.Disconnect,
						Investigate: gr.InvestigationNeeded,
						PMMoves:     gPMoves,
						PMTimeouts:  gPMTimeouts,
						PMAvgMs:     gPMTime,
					}
					if rec.PMAvgMs > 0 {
						rec.PMAvgMs /= float64(gPMoves)
					}
					// The arena clock (loop.go) forces a loss when a side's
					// accumulated wall-clock reaches tc*1.05. Detect it here
					// because hgtp emits no per-move stats and timeouts are
					// otherwise indistinguishable from normal losses.
					timeLimit := *tc * 1.05
					switch {
					case gr.BlackTimeS >= timeLimit:
						rec.TimeoutSide = "black"
					case gr.WhiteTimeS >= timeLimit:
						rec.TimeoutSide = "white"
					}
					if gr.Disconnect || gr.InvestigationNeeded {
						rec.Skipped = true
						skippedBads++
						logger.write(rec)
						slog.Warn("bad game skipped", "pair", pr, "g", g, "disconnect", gr.Disconnect, "investigation", gr.InvestigationNeeded)
						continue
					}
					if rec.TimeoutSide != "" {
						timeouts++
					}
					pmMoves += gPMoves
					pmTimeouts += gPMTimeouts
					pmTime += gPMTime
					logger.write(rec)
					// record from the LOWER level's perspective (a)
					switch gr.Result {
					case "1-0":
						if lowerFirst {
							agg.wins++
						} else {
							agg.losses++
						}
					case "0-1":
						if lowerFirst {
							agg.losses++
						} else {
							agg.wins++
						}
					default:
						agg.draws++
					}
				}
				mu.Lock()
				outs = append(outs, pairResult{players: [2]int{a, b}, agg: agg, timeouts: timeouts, skippedBads: skippedBads})
				mu.Unlock()
				pmPct := 0.0
				if pmMoves > 0 {
					pmPct = float64(pmTimeouts) / float64(pmMoves) * 100
				}
				slog.Info("adjacent pair done", "S", pr, "agg", fmt.Sprintf("%d-%d-%d", agg.wins, agg.draws, agg.losses),
					"timeouts", timeouts, "skipped", skippedBads,
					"pm_moves", pmMoves, "pm_timeouts", pmTimeouts, "pm_timeout_pct", fmt.Sprintf("%.0f%%", pmPct))
			}()
		}
		wg.Wait()
		for _, o := range outs {
			pairs[pk(o.players[0], o.players[1])] = o.agg
		}
	}

	// ── Fit + bootstrap ─────────────────────────────────────────────────
	ratings := fit(pairs)
	base := ratings[refID]
	anchorShift := 2000.0 - base
	slog.Info("fit done", "d6_ref_elo", fmt.Sprintf("%.1f", base), "anchor_shift", fmt.Sprintf("%.1f", anchorShift))

	sd := make([]float64, nPlayer)
	if *bootstrapN > 0 {
		keys := make([][2]int, 0, len(pairs))
		aggs := make(map[[2]int]pairAgg)
		for k, v := range pairs {
			keys = append(keys, k)
			aggs[k] = v
		}
		rng := rand.New(rand.NewSource(12345))
		eloSum := make([]float64, nPlayer)
		eloSq := make([]float64, nPlayer)
		for it := 0; it < *bootstrapN; it++ {
			bs := map[[2]int]pairAgg{}
			for _, k := range keys {
				a := aggs[k]
				// resample the pair multinomially (same game count)
				n := a.total()
				var w, d, l int
				for i := 0; i < n; i++ {
					r := rng.Float64()
					switch {
					case r < float64(a.wins)/float64(n):
						w++
					case r < float64(a.wins+a.draws)/float64(n):
						d++
					default:
						l++
					}
				}
				bs[k] = pairAgg{w, d, l}
			}
			r := fit(bs)
			shift := 2000.0 - r[refID]
			for id := 0; id < nPlayer; id++ {
				e := r[id] + shift
				eloSum[id] += e
				eloSq[id] += e * e
			}
		}
		for id := 0; id < nPlayer; id++ {
			mean := eloSum[id] / float64(*bootstrapN)
			v := eloSq[id]/float64(*bootstrapN) - mean*mean
			sd[id] = math.Sqrt(math.Max(0, v))
		}
		slog.Info("bootstrap done", "n", *bootstrapN)
	}

	// ── Output ──────────────────────────────────────────────────────────
	type outPlayer struct {
		ID     int     `json:"id"`
		Kind   string  `json:"kind"`
		Level  int     `json:"level"`
		Config string  `json:"config"`
		Elo    float64 `json:"elo"`
		Ci95   float64 `json:"ci95"`
		Games  int     `json:"games"`
	}
	var out []outPlayer
	gamesPer := map[int]int{}
	for k, v := range pairs {
		gamesPer[k[0]] += v.total()
		gamesPer[k[1]] += v.total()
	}
	for id := 0; id < nPlayer; id++ {
		op := outPlayer{ID: id, Elo: ratings[id] + anchorShift, Ci95: 1.96 * sd[id], Games: gamesPer[id]}
		switch {
		case id == refID:
			op.Kind, op.Config = "ref", "d6-clean"
		case id < sBase:
			op.Kind, op.Level, op.Config = "H", id-hBase+1, fmt.Sprintf("H%d", id-hBase+1)
		default:
			op.Kind, op.Level, op.Config = "S", id-sBase+1, fmt.Sprintf("S%d (%s)", id-sBase+1, sNames[id-sBase])
		}
		out = append(out, op)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Elo < out[j].Elo })
	data, _ := json.MarshalIndent(out, "", "  ")
	if err := os.WriteFile(*outPath, data, 0644); err != nil {
		slog.Error("write output", "err", err)
		os.Exit(1)
	}
	fmt.Println("\n=== BT calibration (d6-clean anchor = 2000) ===")
	for _, o := range out {
		fmt.Printf("%-6s %4d  %-12s Elo %7.1f  ±%5.1f  (%d games)\n",
			o.Kind, o.Level, o.Config, o.Elo, o.Ci95, o.Games)
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

// fit expands the pair aggregates into game records and runs the arena's
// Zermelo Bradley-Terry fitter. Returns per-ID Elo (mean anchored at 1500).
func fit(pairs map[[2]int]pairAgg) map[int]float64 {
	var recs []elo.GameRecord
	for k, v := range pairs {
		n := v.total()
		if n == 0 {
			continue
		}
		for i := 0; i < v.wins; i++ {
			recs = append(recs, elo.GameRecord{BlackID: k[0], WhiteID: k[1], Result: 1})
		}
		for i := 0; i < v.draws; i++ {
			recs = append(recs, elo.GameRecord{BlackID: k[0], WhiteID: k[1], Result: 0.5})
		}
		for i := 0; i < v.losses; i++ {
			recs = append(recs, elo.GameRecord{BlackID: k[0], WhiteID: k[1], Result: 0})
		}
	}
	return elo.BradleyTerry(recs, 5000)
}
