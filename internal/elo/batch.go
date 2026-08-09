package elo

import (
	"log/slog"
	"math"
	"math/rand"
	"time"
)

// GameRecord is a single game between two engines for batch rating
// estimation. Result: 1 = black wins, 0 = white wins, 0.5 = draw.
type GameRecord struct {
	BlackID, WhiteID int
	Result           float64 // black score: 1, 0, 0.5
	Skip             bool    // exclude from estimation (timeout/disconnect)
}

// Replay computes ratings by processing games in the given order with the
// standard incremental Elo update (K=32 provisional, 16 established), the
// same rules the matchmaker uses. Returns final rating per engine id.
func Replay(games []GameRecord) map[int]float64 {
	ratings := map[int]float64{}
	counts := map[int]int{}
	for _, g := range games {
		if g.Skip {
			continue
		}
		rB, okB := ratings[g.BlackID]
		rW, okW := ratings[g.WhiteID]
		if !okB {
			rB = 1500
		}
		if !okW {
			rW = 1500
		}
		nB, _ := Update(rB, rW, g.Result, counts[g.BlackID])
		nW, _ := Update(rW, rB, 1-g.Result, counts[g.WhiteID])
		ratings[g.BlackID] = nB
		ratings[g.WhiteID] = nW
		counts[g.BlackID]++
		counts[g.WhiteID]++
	}
	return ratings
}

// ReplayInto is like Replay but reuses caller-provided maps, avoiding
// per-call allocation in tight loops (e.g. shuffle averaging).
func ReplayInto(games []GameRecord, ratings map[int]float64, counts map[int]int) {
	for k := range ratings {
		delete(ratings, k)
	}
	for k := range counts {
		delete(counts, k)
	}
	for _, g := range games {
		if g.Skip {
			continue
		}
		rB, okB := ratings[g.BlackID]
		rW, okW := ratings[g.WhiteID]
		if !okB {
			rB = 1500
		}
		if !okW {
			rW = 1500
		}
		nB, _ := Update(rB, rW, g.Result, counts[g.BlackID])
		nW, _ := Update(rW, rB, 1-g.Result, counts[g.WhiteID])
		ratings[g.BlackID] = nB
		ratings[g.WhiteID] = nW
		counts[g.BlackID]++
		counts[g.WhiteID]++
	}
}

// ShuffleAverage de-biases the order-dependence of incremental Elo by
// replaying the games in many random orders and averaging the final ratings.
// Returns the mean rating per engine and the std-dev across shuffles
// (an uncertainty estimate of the order effect).
//
// Convergence: every checkEvery iterations, compares the cumulative mean to
// the cumulative mean checkEvery iterations earlier; stops when the max
// per-engine drift is below convergeDelta, with a minimum of minIters.
func ShuffleAverage(games []GameRecord, maxIters int, checkEvery int, convergeDelta float64, minIters int) (mean, std map[int]float64, iters int) {
	mean = map[int]float64{}
	std = map[int]float64{}
	sum := map[int]float64{}
	sumSq := map[int]float64{}
	prevSum := map[int]float64{}
	idx := make([]int, len(games))
	for i := range idx {
		idx[i] = i
	}
	shuffled := make([]GameRecord, len(games))
	ratings := map[int]float64{}
	counts := map[int]int{}
	rng := rand.New(rand.NewSource(1))
	start := time.Now()
	var lastCheck int
	for it := 1; it <= maxIters; it++ {
		rng.Shuffle(len(idx), func(i, j int) { idx[i], idx[j] = idx[j], idx[i] })
		for i, j := range idx {
			shuffled[i] = games[j]
		}
		ReplayInto(shuffled, ratings, counts)
		for id, r := range ratings {
			sum[id] += r
			sumSq[id] += r * r
		}
		if it%checkEvery == 0 {
			if it >= minIters {
				maxDelta := 0.0
				for id, s := range sum {
					cur := s / float64(it)
					prev := prevSum[id] / float64(lastCheck)
					d := math.Abs(cur - prev)
					if d > maxDelta {
						maxDelta = d
					}
					prevSum[id] = s
				}
				if maxDelta < convergeDelta {
					iters = it
					break
				}
			} else {
				for id, s := range sum {
					prevSum[id] = s
				}
			}
			lastCheck = it
		}
	}
	if iters == 0 {
		iters = maxIters
	}
	slog.Info("shuffle average", "games", len(games), "iters", iters, "elapsed_ms", time.Since(start).Milliseconds())
	for id, s := range sum {
		m := s / float64(iters)
		mean[id] = m
		v := sumSq[id]/float64(iters) - m*m
		if v < 0 {
			v = 0
		}
		std[id] = math.Sqrt(v)
	}
	return mean, std, iters
}

// BradleyTerry fits the logistic (Bradley-Terry) model to all pairwise
// results via iterative proportional fitting (Zermelo's algorithm). This is
// order-independent by construction and naturally weights pairs by their
// game counts, removing both the ordering bias and the pair-allocation bias.
//
// It returns ratings anchored so the mean rating is 1500.
func BradleyTerry(games []GameRecord, maxIters int) map[int]float64 {
	wins := map[[2]int]float64{} // ordered pair key: [winner, loser]
	plays := map[[2]int]int{}    // ordered pair key: [i, j] games played (both directions counted once)
	engineSet := map[int]bool{}
	for _, g := range games {
		if g.Skip {
			continue
		}
		engineSet[g.BlackID] = true
		engineSet[g.WhiteID] = true
		key := pairKey(g.BlackID, g.WhiteID)
		plays[key]++
		switch g.Result {
		case 1:
			wins[[2]int{g.BlackID, g.WhiteID}]++
		case 0:
			wins[[2]int{g.WhiteID, g.BlackID}]++
		case 0.5:
			wins[[2]int{g.BlackID, g.WhiteID}] += 0.5
			wins[[2]int{g.WhiteID, g.BlackID}] += 0.5
		}
	}
	if len(engineSet) == 0 {
		return map[int]float64{}
	}

	// Zermelo iteration: r_i = W_i / Σ_j (n_ij / (r_i + r_j))
	// where W_i = total wins of i (draws count half), n_ij = games i vs j.
	r := map[int]float64{}
	for id := range engineSet {
		r[id] = 1.0
	}
	totalWins := map[int]float64{}
	for id := range engineSet {
		var w float64
		for j := range engineSet {
			if j == id {
				continue
			}
			w += wins[[2]int{id, j}]
		}
		totalWins[id] = w
	}
	ids := make([]int, 0, len(engineSet))
	for id := range engineSet {
		ids = append(ids, id)
	}
	for it := 0; it < maxIters; it++ {
		maxDelta := 0.0
		for _, i := range ids {
			var denom float64
			for _, j := range ids {
				if j == i {
					continue
				}
				n := plays[pairKey(i, j)]
				if n == 0 {
					continue
				}
				denom += float64(n) / (r[i] + r[j])
			}
			if denom == 0 {
				continue
			}
			newR := totalWins[i] / denom
			if newR <= 0 {
				newR = 1e-9
			}
			d := math.Abs(newR - r[i])
			if d > maxDelta {
				maxDelta = d
			}
			r[i] = newR
		}
		if maxDelta < 1e-9 {
			break
		}
	}

	// Convert to Elo and anchor the mean at 1500.
	ratings := map[int]float64{}
	var sumElo float64
	for id := range engineSet {
		e := 400.0 * math.Log10(r[id])
		ratings[id] = e
		sumElo += e
	}
	mean := sumElo / float64(len(engineSet))
	offset := 1500 - mean
	for id := range ratings {
		ratings[id] += offset
	}
	return ratings
}

func pairKey(a, b int) [2]int {
	if a < b {
		return [2]int{a, b}
	}
	return [2]int{b, a}
}
