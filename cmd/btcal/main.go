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
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"os"
	"sort"
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
var v1ToV3 = []int{
	0, 1, 2, 3, 0, 0, 7, 8, 0, 10, 11, 0, 12, 0, 13, 14, 15, 16, 17, 19,
}

// Level N of run v2/v3 (s-calib3, confirm run) -> v3 S level (S4/S5 swap).
var v3ToV3 = []int{
	0, 1, 2, 3, 5, 4, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
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
	if *fitOnly {
		slog.Info("fit-only: skipping adjacent-pair games")
	} else {
		type pairResult struct {
			players [2]int
			agg     pairAgg
		}
		var (
			mu   sync.Mutex
			outs = make([]pairResult, 0, len(adjacentPairs))
			wg   sync.WaitGroup
			sem  = make(chan struct{}, *concurrency)
		)
		for _, pr := range adjacentPairs {
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
				for g := 0; g < *pairsGames; g++ {
					op := book[(pr[0]*31+g)%len(book)]
					var gr game.GameResult
					if g%2 == 0 {
						gr = playOneGame(cmdA, cmdB, op.Line, *tc)
					} else {
						gr = playOneGame(cmdB, cmdA, op.Line, *tc)
					}
					if gr.Disconnect || gr.InvestigationNeeded {
						slog.Warn("bad game skipped", "pair", pr, "g", g, "disconnect", gr.Disconnect, "investigation", gr.InvestigationNeeded)
						continue
					}
					// record from the LOWER level's perspective (a)
					lowerFirst := g%2 == 0
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
				outs = append(outs, pairResult{players: [2]int{a, b}, agg: agg})
				mu.Unlock()
				slog.Info("adjacent pair done", "S", pr, "agg", fmt.Sprintf("%d-%d-%d", agg.wins, agg.draws, agg.losses))
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
