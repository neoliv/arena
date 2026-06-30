package web

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/neoliv/arena/internal/game"
)

// moveRow is a single row from the game_moves table.
type moveRow struct {
	num            int
	side, move     string
	nodes          int
	depth          int
	timeMs         float64
	nps            int
	score          int
}

// computeBoardStates replays the entire game from the opening line through
// every engine move using game.Board.ApplyMove.
func computeBoardStates(opening string, moves []moveRow) []boardState {
	var states []boardState
	board := game.NewBoard()
	for i := 0; i+1 < len(opening); i += 2 {
		mv := strings.ToUpper(opening[i : i+2])
		sq := game.SqFromString(mv)
		if sq < 0 {
			continue
		}
		side := "b"
		player := board.Black()
		if i%4 != 0 {
			side = "w"
			player = board.White()
		}
		board = board.ApplyMove(player, sq)
		states = append(states, boardState{board, sq, side})
	}
	for _, m := range moves {
		player := board.Black()
		if m.side == "w" {
			player = board.White()
		}
		if m.move == "PASS" {
			states = append(states, boardState{board, -1, m.side})
			continue
		}
		sq := game.SqFromString(m.move)
		if sq < 0 {
			states = append(states, boardState{board, -1, m.side})
			continue
		}
		board = board.ApplyMove(player, sq)
		states = append(states, boardState{board, sq, m.side})
	}
	return states
}



func (h *Handler) handleGameDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, pageHead+navHTML)

	var gid, mid, gnum, finalScore, blackNodes, whiteNodes, blackDepth, whiteDepth, disconnect, investigationNeeded, errorCode int
	var result, opening, bName, bVer, wName, wVer, tcJSON string
	var bTime, wTime, gameTimeSec float64
	var bElo, wElo, bEloBefore, wEloBefore float64
	err := h.DB.QueryRow(
		"SELECT g.id, g.match_id, g.game_number, g.result, COALESCE(g.final_score,0), COALESCE(g.opening_line,''), "+
			"COALESCE(g.black_time_s,0), COALESCE(g.white_time_s,0), COALESCE(g.black_nodes,0), COALESCE(g.white_nodes,0), "+
			"COALESCE(g.black_depth,0), COALESCE(g.white_depth,0), COALESCE(g.disconnect,0), COALESCE(g.investigation_needed,0), COALESCE(g.error_code,0), eb.name, eb.version, ew.name, ew.version, "+
			"COALESCE(m.time_control,'{}'), "+
			"COALESCE((SELECT rating_after FROM elo_history WHERE engine_id=g.black_id ORDER BY created_at DESC LIMIT 1), 1500.0), "+
			"COALESCE((SELECT rating_after FROM elo_history WHERE engine_id=g.white_id ORDER BY created_at DESC LIMIT 1), 1500.0), "+
			"COALESCE((SELECT rating_before FROM elo_history WHERE engine_id=g.black_id AND match_id=g.match_id ORDER BY created_at DESC LIMIT 1), 0.0), "+
			"COALESCE((SELECT rating_before FROM elo_history WHERE engine_id=g.white_id AND match_id=g.match_id ORDER BY created_at DESC LIMIT 1), 0.0) "+
			"FROM games g JOIN engines eb ON g.black_id=eb.id JOIN engines ew ON g.white_id=ew.id JOIN matches m ON m.id=g.match_id WHERE g.id=?",
		id).Scan(&gid, &mid, &gnum, &result, &finalScore, &opening, &bTime, &wTime, &blackNodes, &whiteNodes, &blackDepth, &whiteDepth, &disconnect, &investigationNeeded, &errorCode, &bName, &bVer, &wName, &wVer, &tcJSON, &bElo, &wElo, &bEloBefore, &wEloBefore)
	if err != nil {
		io.WriteString(w, "<p>Game not found.</p>"+pageFoot)
		return
	}

	var tc struct {
		Seconds float64 `json:"seconds"`
		Label   string  `json:"label"`
	}
	json.Unmarshal([]byte(tcJSON), &tc)
	if tc.Seconds > 0 {
		gameTimeSec = tc.Seconds
	}

	bDelta := bElo - bEloBefore
	wDelta := wElo - wEloBefore

	bScore := 0
	wScore := 0
	if result == "1-0" {
		bScore = finalScore
		wScore = 64 - finalScore
	}
	if result == "0-1" {
		wScore = finalScore
		bScore = 64 - finalScore
	}

	// ── Top bar ────────────────────────────────────────────────────
	bTimedOut := gameTimeSec > 0 && bTime > gameTimeSec*1.05
	wTimedOut := gameTimeSec > 0 && wTime > gameTimeSec*1.05
	// ── Error badge (right of score) ──────────────────────────
	// Color: faulty engine (loser) or red for infra/disconnect.
	statusBadge := ""
	if errorCode != 0 || disconnect != 0 || investigationNeeded != 0 {
		errLabel := ""
		errColor := "#f44336" // default: red (infra/disconnect)
		if errorCode != 0 {
			if label, ok := game.ErrorLabel[int8(errorCode)]; ok { errLabel = label }
			// Color by losing engine
			if result == "1-0" { errColor = "#d4c4a8" } else { errColor = "#22d3ee" }
		} else if disconnect != 0 {
			errLabel = "disconnected"
		} else if investigationNeeded != 0 {
			errLabel = "investigation needed"; errColor = "#ff9800"
		}
		bTimedOut = gameTimeSec > 0 && bTime > gameTimeSec*1.05
		wTimedOut = gameTimeSec > 0 && wTime > gameTimeSec*1.05
		if bTimedOut { errLabel = "black timeout"; errColor = "#22d3ee" }
		if wTimedOut { errLabel = "white timeout"; errColor = "#d4c4a8" }
		if errLabel != "" {
			statusBadge = fmt.Sprintf(` <span style="color:%s;font-weight:900;font-size:.7em">[%s]</span>`, errColor, errLabel)
		}
	}
	bNameEsc := htmlEscape(bName)
	wNameEsc := htmlEscape(wName)
	// Line 1: game number (left, bold) + score (center, larger)
	fmt.Fprintf(w, `<div style="display:flex;align-items:baseline;justify-content:space-between;margin-bottom:.3em">
		<div style="flex:2"><span style="font-size:1.6em;font-weight:800"># %s</span></div>
		<div style="flex:1;text-align:center"><span style="font-size:2em;font-weight:900">%d-%d</span>%s</div>
		<div style="flex:2"></div></div>`,
		id, bScore, wScore, statusBadge)
	// Line 2: player info (black right, white left)
	fmt.Fprintf(w, `<div style="display:flex;justify-content:center;gap:3em;margin-bottom:.3em">
		<div style="text-align:right;font-size:1.05em"><span style="color:#22d3ee">%s</span> <span style="color:#e8e6e3">%.0f</span> <span style="color:%s">%+d</span></div>
		<div style="text-align:left;font-size:1.05em"><span style="color:%s">%+d</span> <span style="color:#e8e6e3">%.0f</span> <span style="color:#d4c4a8">%s</span></div></div>`,
		bNameEsc, bElo, deltaColor(bDelta), int(bDelta), deltaColor(wDelta), int(wDelta), wElo, wNameEsc)
	// Line 3: version links
	fmt.Fprintf(w, `<div style="display:flex;justify-content:center;gap:3em;margin-bottom:.6em">
		<div style="text-align:right;font-size:.95em"><a href="/engines/%s">%s</a></div>
		<div style="text-align:left;font-size:.95em"><a href="/engines/%s">%s</a></div></div>`,
		bNameEsc, htmlEscape(bVer), wNameEsc, htmlEscape(wVer))

	bUnspent := 0.0
	wUnspent := 0.0
	if gameTimeSec > 0 {
		bUnspent = max(0, gameTimeSec-bTime)
		wUnspent = max(0, gameTimeSec-wTime)
	}
	tcLabel := tc.Label
	if tcLabel == "" && tc.Seconds > 0 {
		tcLabel = fmt.Sprintf("%.0fs", tc.Seconds)
	}
	otherGame := gid + 1
	if gnum == 2 {
		otherGame = gid - 1
	}
	io.WriteString(w, `<br><div style="text-align:center;margin-bottom:.3em;color:var(--muted);font-size:.85em">`)
	fmt.Fprintf(w, `Match <a href="/matches/%d">#%d</a> (game %d) | <a href="/games/%d">other game</a>`, mid, mid, gnum, otherGame)
	if tcLabel != "" {
		fmt.Fprintf(w, ` | Time: %s (unspent B:%.0fs W:%.0fs)`, tcLabel, bUnspent, wUnspent)
	}
	io.WriteString(w, `</div><br>`)

	mRows, _ := h.DB.Query("SELECT move_num, side, move, nodes, depth, time_ms, score FROM game_moves WHERE game_id=? ORDER BY move_num", gid)
	if mRows != nil {
		defer mRows.Close()

		var moves []moveRow
		maxTime, maxNodes, maxNPS, maxDepth, maxBScore, maxWScore := 0.0, 0.0, 0.0, 0.0, 0.0, 0.0
		for mRows.Next() {
			var m moveRow
			mRows.Scan(&m.num, &m.side, &m.move, &m.nodes, &m.depth, &m.timeMs, &m.score)
			if m.timeMs > 0 {
				m.nps = int(float64(m.nodes) * 1000.0 / m.timeMs)
			}
			moves = append(moves, m)
			if m.timeMs > maxTime {
				maxTime = m.timeMs
			}
			if float64(m.nodes) > maxNodes {
				maxNodes = float64(m.nodes)
			}
			if float64(m.nps) > maxNPS {
				maxNPS = float64(m.nps)
			}
			if float64(m.depth) > maxDepth {
				maxDepth = float64(m.depth)
			}
			if m.side == "b" {
				abs := float64(m.score)
				if abs < 0 {
					abs = -abs
				}
				if abs > maxBScore {
					maxBScore = abs
				}
			}
			if m.side == "w" {
				abs := float64(m.score)
				if abs < 0 {
					abs = -abs
				}
				if abs > maxWScore {
					maxWScore = abs
				}
			}
		}

		boardStates := computeBoardStates(opening, moves)

		discDiffs := make([]int, len(moves))
		bCount, wCount := 2, 2
		maxDiscDiff := 0.0
		for i, m := range moves {
			if m.side == "b" {
				bCount++
			} else {
				wCount++
			}
			discDiffs[i] = bCount - wCount
			adiff := discDiffs[i]
			if adiff < 0 {
				adiff = -adiff
			}
			if float64(adiff) > maxDiscDiff {
				maxDiscDiff = float64(adiff)
			}
		}
		if maxDiscDiff < 1 {
			maxDiscDiff = 1
		}

		if len(moves) > 0 {
			tab := r.URL.Query().Get("tab")
			openingPlies := len(opening) / 2
			chartH := 320
			topPad := 30
			totalPlies := openingPlies + len(moves)
			chartW := fmt.Sprintf("%d", max(600, totalPlies*14+120))
			if tab == "" {
				tab = "time"
			}
			io.WriteString(w, `<nav class="chart-tabs" style="margin-top:0;margin-bottom:1em">`)
			for _, t := range []struct{ key, label string }{
				{"time", "Time"}, {"nodes", "Nodes"}, {"nps", "NpS"}, {"depth", "Depth"}, {"diff", "Diff"}, {"score", "Score"},
			} {
				sel := `class="chart-tab" style="display:inline-block;padding:.35em .7em;border-radius:5px;font-size:1.1em;font-weight:600;text-decoration:none;border:1px solid var(--nav-hl);color:#fff;background:var(--nav-hl)"`
				if tab != t.key {
					sel = `class="chart-tab" style="display:inline-block;padding:.35em .7em;border-radius:5px;font-size:1.1em;font-weight:600;text-decoration:none;border:1px solid var(--nav-hl);color:var(--fg);background:rgba(56,136,85,0.06)"`
				}
				fmt.Fprintf(w, `<a href="?tab=%s" %s>%s</a>`, t.key, sel, t.label)
			}
			io.WriteString(w, `</nav>`)

			fmtVal := func(v float64) string {
				switch {
				case v >= 1e9:
					return fmt.Sprintf("%.1fG", v/1e9)
				case v >= 1e6:
					return fmt.Sprintf("%.0fM", v/1e6)
				case v >= 1e3:
					return fmt.Sprintf("%.0fk", v/1e3)
				default:
					return fmt.Sprintf("%.0f", v)
				}
			}
			renderChart := func(metric string, maxVal, maxValR float64, unit string, yLabel string) {
				if metric != tab {
					return
				}
				io.WriteString(w, fmt.Sprintf(`<div style="background:#2d5a2d;border:1px solid #2a4a2a;border-radius:6px;padding:12px 8px 24px 8px;overflow-x:auto">`))
				fmt.Fprintf(w, `<div style="display:flex;justify-content:space-between;align-items:baseline;margin-bottom:6px"><span style="color:#22d3ee;font-size:18px;font-weight:700">%s</span><span style="color:#d4c4a8;font-size:18px;font-weight:700">%s</span></div>`, bName, wName)
				io.WriteString(w, fmt.Sprintf(`<svg width="%s" height="%d">`, chartW, chartH+82))
				if openingPlies > 0 {
					openW := openingPlies * 14
					fmt.Fprintf(w, `<rect x="%d" y="%d" width="%d" height="%d" fill="#3a3a3a" opacity="0.5"/>`, 34, topPad, openW, chartH)
					fmt.Fprintf(w, `<text x="%d" y="%d" fill="#888" font-size="11" text-anchor="middle" font-style="italic">forced %dpl</text>`, 34+openW/2, chartH+topPad-6, openingPlies)
				}
				niceStep := maxVal / 4
				if niceStep >= 100 {
					niceStep = float64(int(niceStep/100+0.5)) * 100
				} else if niceStep >= 10 {
					niceStep = float64(int(niceStep/10+0.5)) * 10
				} else {
					niceStep = float64(int(niceStep + 0.5))
				}
				for pct := 0; pct <= 100; pct += 25 {
					y := chartH - pct*chartH/100 + 44
					val := float64(pct) / 100.0 * niceStep * 4
					if pct == 100 {
						val = maxVal
					}
					tickColor := "#6a6"
					if metric == "score" {
						tickColor = "#22d3ee"
					}
					fmt.Fprintf(w, `<text x="0" y="%d" fill="%s" font-size="11">%s%s</text>`, y, tickColor, fmtVal(val), unit)
					fmt.Fprintf(w, `<line x1="34" y1="%d" x2="100%%" y2="%d" stroke="#2a4a2a" stroke-width="0.5"/>`, chartH-pct*chartH/100, chartH-pct*chartH/100)
				}
				if metric == "diff" {
					z := chartH/2 + topPad
					fmt.Fprintf(w, `<line x1="34" y1="%d" x2="100%%" y2="%d" stroke="#2a4a2a" stroke-width="1" stroke-dasharray="4,4"/>`, z, z)
				}
				if metric == "score" && maxValR > 0 {
					niceStepR := maxValR / 4
					if niceStepR >= 100 {
						niceStepR = float64(int(niceStepR/100+0.5)) * 100
					} else if niceStepR >= 10 {
						niceStepR = float64(int(niceStepR/10+0.5)) * 10
					} else {
						niceStepR = float64(int(niceStepR + 0.5))
					}
					for pct := 0; pct <= 100; pct += 25 {
						y := chartH - pct*chartH/100 + 44
						valR := float64(pct) / 100.0 * niceStepR * 4
						if pct == 100 {
							valR = maxValR
						}
						fmt.Fprintf(w, `<text x="100%%" y="%d" fill="#d4c4a8" font-size="10" text-anchor="end">%s%s</text>`, y, fmtVal(valR), unit)
					}
				}
				for pl := 10; pl <= totalPlies; pl += 10 {
					tx := 34 + pl*14
					fmt.Fprintf(w, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#3a5a3a" stroke-width="1"/>`, tx, topPad-2, tx, topPad+2)
				}
				lx := 34 + totalPlies*14 + 6
				fmt.Fprintf(w, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#6a6" stroke-width="1" stroke-dasharray="4,4"/>`, lx, topPad, lx, chartH+topPad)
				midY := chartH/2 + topPad
				plyLabel := fmt.Sprintf("%d/60", totalPlies)
				if totalPlies == 60 { plyLabel = "60" }
				fmt.Fprintf(w, `<text x="%d" y="%d" text-anchor="start" fill="#6a6" font-size="10" font-weight="600">%s</text>`, lx+4, midY+4, plyLabel)
				// Ply 60 marker — max board capacity
				px60 := 34 + 60*14 + 6
				if px60 > lx {
					fmt.Fprintf(w, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#555" stroke-width="0.5" stroke-dasharray="2,6"/>`, px60, topPad, px60, chartH+topPad)
				}
				fmt.Fprintf(w, `<text x="50%%" y="%d" text-anchor="middle" fill="#6a6" font-size="12">%s</text>`, chartH+68, yLabel)
				for i, m := range moves {
					var val float64
					switch metric {
					case "time":
						val = m.timeMs
					case "nodes":
						val = float64(m.nodes)
					case "nps":
						val = float64(m.nps)
					case "score":
						maxVal = maxBScore
						if m.side == "w" {
							maxVal = maxWScore
						}
						if maxVal < 1 {
							maxVal = 1
						}
						val = float64(m.score)
					case "depth":
						val = float64(m.depth)
					case "diff":
						val = float64(discDiffs[i])
					}
					h := 0
					if metric == "diff" {
						mid := chartH / 2
						if maxVal > 0 {
							h = int((val + maxVal) / (2 * maxVal) * float64(chartH))
						}
						_ = mid
					} else {
						if maxVal > 0 {
							h = int(val / maxVal * float64(chartH))
						}
					}
					if h < 2 {
						h = 2
					}
					barY := chartH - h + topPad
					if metric == "score" {
						mid := float64(chartH / 2)
						h = int((val / maxVal) * mid)
						if h < 0 {
							h = -h
						}
						if h < 2 {
							h = 2
						}
						if val >= 0 {
							barY = int(mid) - h + topPad
						} else {
							barY = int(mid) + topPad
						}
					}
					color := "#22d3ee"
					if m.side == "w" {
						color = "#d4c4a8"
					}
					x := 34 + (openingPlies+i)*14
					titleVal := fmtVal(val)
					if metric == "diff" {
						titleVal = fmt.Sprintf("%+d", discDiffs[i])
					}
					tip := fmt.Sprintf("%s %s: %s%s", m.side, m.move, titleVal, unit)
					switch metric {
					case "time":
						tip = fmt.Sprintf("%s %s: %.0fms, %s nodes", m.side, m.move, m.timeMs, fmtVal(float64(m.nodes)))
					case "nodes":
						tip = fmt.Sprintf("%s %s: %s nodes, depth %d", m.side, m.move, fmtVal(float64(m.nodes)), m.depth)
					case "nps":
						tip = fmt.Sprintf("%s %s: %s n/s, %.0fms", m.side, m.move, fmtVal(float64(m.nps)), m.timeMs)
					case "depth":
						tip = fmt.Sprintf("%s %s: depth %d", m.side, m.move, m.depth)
					case "score":
						tip = fmt.Sprintf("%s %s: %+d cP", m.side, m.move, m.score)
					case "diff":
						tip = fmt.Sprintf("%s %s: %+d discs", m.side, m.move, discDiffs[i])
					}
					plyLabel := fmt.Sprintf("%d:%s", openingPlies+i+1, m.move)
					fmt.Fprintf(w, `<rect data-board-idx="%d" x="%d" y="%d" width="12" height="%d" fill="%s" rx="1"><title>%s</title></rect>`, openingPlies+i, x, barY, h, color, tip)
					fmt.Fprintf(w, `<text x="%d" y="%d" fill="%s" font-size="9" text-anchor="middle">%s</text>`, x+6, chartH+20+topPad, color, htmlEscape(plyLabel))
				}
				io.WriteString(w, `</svg></div>`)
			}
			renderChart("time", maxTime, 0, "ms", "Time per move (ms)")
			renderChart("nodes", maxNodes, 0, "", "Nodes explored")
			renderChart("nps", maxNPS, 0, "", "Nodes per second")
			renderChart("depth", maxDepth, 0, "", "Search depth")
			renderChart("diff", maxDiscDiff, 0, "", "Disc diff (B-W)")
			renderChart("score", maxBScore, maxWScore, "", "Score (cP)")

			// ── Stats summary ──────────────────────────────────────────
			type sideStats struct {
				moves                                  int
				totalTime, totalNodes                  float64
				times, nodes, depths, npsVals          []float64
				timeMin, timeMax, timeAvg, timeStd     float64
				nodeMin, nodeMax, nodeAvg, nodeStd     float64
				depthMin, depthMax, depthAvg           float64
				npsMin, npsMax, npsAvg, npsStd         float64
				firstES                                 int
			}
			var bStats, wStats sideStats
			for _, m := range moves {
				s := &bStats
				if m.side == "w" {
					s = &wStats
				}
				s.moves++
				s.totalTime += m.timeMs
				s.totalNodes += float64(m.nodes)
				s.times = append(s.times, m.timeMs)
				s.nodes = append(s.nodes, float64(m.nodes))
				s.depths = append(s.depths, float64(m.depth))
				nps := float64(m.nps)
				if nps == 0 && m.timeMs > 0 {
					nps = float64(m.nodes) * 1000.0 / m.timeMs
				}
				s.npsVals = append(s.npsVals, nps)
				if s.firstES == 0 && (m.depth >= 20 || m.score == 6400 || m.score == -6400) {
					s.firstES = openingPlies + m.num
				}
			}
			computeStats := func(s *sideStats) {
				if s.moves == 0 {
					return
				}
				s.timeMin, s.timeMax = s.times[0], s.times[0]
				s.nodeMin, s.nodeMax = s.nodes[0], s.nodes[0]
				s.depthMin, s.depthMax = s.depths[0], s.depths[0]
				s.npsMin, s.npsMax = s.npsVals[0], s.npsVals[0]
				var tSum, nSum, dSum, npsSum float64
				for i := 0; i < s.moves; i++ {
					t, n, d, np := s.times[i], s.nodes[i], s.depths[i], s.npsVals[i]
					tSum += t
					nSum += n
					dSum += d
					npsSum += np
					if t < s.timeMin {
						s.timeMin = t
					}
					if t > s.timeMax {
						s.timeMax = t
					}
					if n < s.nodeMin {
						s.nodeMin = n
					}
					if n > s.nodeMax {
						s.nodeMax = n
					}
					if d < s.depthMin {
						s.depthMin = d
					}
					if d > s.depthMax {
						s.depthMax = d
					}
					if np < s.npsMin {
						s.npsMin = np
					}
					if np > s.npsMax {
						s.npsMax = np
					}
				}
				s.timeAvg = tSum / float64(s.moves)
				s.nodeAvg = nSum / float64(s.moves)
				s.depthAvg = dSum / float64(s.moves)
				s.npsAvg = npsSum / float64(s.moves)
				if s.moves > 1 {
					var tV, nV, npsV float64
					for i := 0; i < s.moves; i++ {
						d := s.times[i] - s.timeAvg
						tV += d * d
						d = s.nodes[i] - s.nodeAvg
						nV += d * d
						d = s.npsVals[i] - s.npsAvg
						npsV += d * d
					}
					s.timeStd = math.Sqrt(tV / float64(s.moves-1))
					s.nodeStd = math.Sqrt(nV / float64(s.moves-1))
					s.npsStd = math.Sqrt(npsV / float64(s.moves-1))
				}
			}
			computeStats(&bStats)
			computeStats(&wStats)

			writeStatRow := func(label, bVal, wVal string) {
				fmt.Fprintf(w, `<tr><td style="text-align:right;padding-right:1.5em;font-weight:600;color:var(--muted)">%s</td><td style="text-align:right;padding-right:1.5em">%s</td><td style="text-align:left;padding-left:1.5em">%s</td></tr>`, label, bVal, wVal)
			}

			io.WriteString(w, `<div style="display:flex;justify-content:center;margin-top:1.5em"><table class="stats-table" style="width:auto;min-width:500px">`)
			io.WriteString(w, `<tr><th></th><th style="text-align:right;padding-right:1.5em;color:#22d3ee">Black</th><th style="text-align:left;padding-left:1.5em;color:#d4c4a8">White</th></tr>`)

			writeStatRow("Score", fmt.Sprintf("%d", bScore), fmt.Sprintf("%d", wScore))
			writeStatRow("Total time", fmt.Sprintf("%.2fs", bTime), fmt.Sprintf("%.2fs", wTime))
			writeStatRow("Moves played", fmt.Sprintf("%d", bStats.moves), fmt.Sprintf("%d", wStats.moves))
			writeStatRow("Total nodes", fmtVal(bStats.totalNodes), fmtVal(wStats.totalNodes))
			writeStatRow("Time/ply (min/avg/max)",
				fmt.Sprintf("%.0f/%.0f/%.0fms", bStats.timeMin, bStats.timeAvg, bStats.timeMax),
				fmt.Sprintf("%.0f/%.0f/%.0fms", wStats.timeMin, wStats.timeAvg, wStats.timeMax))
			writeStatRow("Time/ply stdev", fmt.Sprintf("%.0fms", bStats.timeStd), fmt.Sprintf("%.0fms", wStats.timeStd))
			writeStatRow("Depth/ply (min/avg/max)",
				fmt.Sprintf("%.0f/%.1f/%.0f", bStats.depthMin, bStats.depthAvg, bStats.depthMax),
				fmt.Sprintf("%.0f/%.1f/%.0f", wStats.depthMin, wStats.depthAvg, wStats.depthMax))
			writeStatRow("Nodes/ply (min/avg/max)",
				fmt.Sprintf("%s/%s/%s", fmtVal(bStats.nodeMin), fmtVal(bStats.nodeAvg), fmtVal(bStats.nodeMax)),
				fmt.Sprintf("%s/%s/%s", fmtVal(wStats.nodeMin), fmtVal(wStats.nodeAvg), fmtVal(wStats.nodeMax)))
			writeStatRow("Nodes/ply stdev", fmtVal(bStats.nodeStd), fmtVal(wStats.nodeStd))
			writeStatRow("NpS/ply (min/avg/max)",
				fmt.Sprintf("%s/%s/%s", fmtVal(bStats.npsMin), fmtVal(bStats.npsAvg), fmtVal(bStats.npsMax)),
				fmt.Sprintf("%s/%s/%s", fmtVal(wStats.npsMin), fmtVal(wStats.npsAvg), fmtVal(wStats.npsMax)))

			if bStats.firstES > 0 || wStats.firstES > 0 {
				bES, wES := "-", "-"
				if bStats.firstES > 0 {
					bES = fmt.Sprintf("ply %d", bStats.firstES)
				}
				if wStats.firstES > 0 {
					wES = fmt.Sprintf("ply %d", wStats.firstES)
				}
				writeStatRow("First ES", bES, wES)
			}
			io.WriteString(w, `</table></div>`)

			// ── Board viewer ──────────────────────────────────────
			if len(boardStates) > 0 {
				lastIdx := len(boardStates) - 1
				fmt.Fprintf(w, `<div class="board-viewer" id="board-viewer" data-default-idx="%d" style="text-align:center;margin-bottom:1.5em">`, lastIdx)
				io.WriteString(w, `<div style="display:flex;align-items:center;justify-content:center;gap:12px">`)
				io.WriteString(w, `<div id="board-container" style="background:#1a5c3a;display:inline-block;padding:8px;border-radius:8px">`)
				io.WriteString(w, renderBoardSVG(boardStates[lastIdx].board, boardStates[lastIdx].lastSq))
				io.WriteString(w, `</div>`)
				io.WriteString(w, `<div style="display:flex;flex-direction:column;align-items:center;gap:4px">`)
				fmt.Fprintf(w, `<div id="ply-counter" style="font-size:2.5em;font-weight:700;color:var(--fg);line-height:1">%d</div>`, len(boardStates))
				io.WriteString(w, `<button id="btn-prev" style="background:var(--nav-hl);color:#fff;border:none;border-radius:4px;padding:4px 12px;cursor:pointer;font-size:1.2em" title="Previous move">◀</button>`)
				io.WriteString(w, `<button id="btn-next" style="background:var(--nav-hl);color:#fff;border:none;border-radius:4px;padding:4px 12px;cursor:pointer;font-size:1.2em" title="Next move">▶</button>`)
				io.WriteString(w, `</div>`)
				io.WriteString(w, `</div>`)
				io.WriteString(w, `<div id="board-label" style="color:var(--muted);margin-top:.3em;font-size:.9em">click bar or row to jump · ◀▶ to navigate</div>`)
				// Hidden board data for hover interaction
				io.WriteString(w, `<div id="board-data" style="display:none">`)
				for idx, bs := range boardStates {
					fmt.Fprintf(w, `<div data-idx="%d">%s</div>`, idx, renderBoardSVG(bs.board, bs.lastSq))
				}
				io.WriteString(w, `</div></div>`)
				io.WriteString(w, boardInteractionJS)
			}

			// ── Moves summary (below graphs, above move table) ──
			totalTime := bTime + wTime
			totalMoves := bStats.moves + wStats.moves
			if opening != "" {
				openPlies := len(opening) / 2
				fmt.Fprintf(w, `<div style="margin-top:1em;margin-bottom:.6em;color:var(--muted);font-size:1.1em;text-align:center">Moves: %d forced · %d played · last ply %d · <span style="font-family:monospace">%s</span> · %.2fs · %s nodes</div>`,
					openPlies, totalMoves, totalPlies, htmlEscape(opening), totalTime, fmtVal(bStats.totalNodes+wStats.totalNodes))
			} else {
				fmt.Fprintf(w, `<div style="margin-top:1em;margin-bottom:.6em;color:var(--muted);font-size:1.1em;text-align:center">Moves: %d played · last ply %d · %.2fs · %s nodes</div>`,
					totalMoves, totalPlies, totalTime, fmtVal(bStats.totalNodes+wStats.totalNodes))
			}
			io.WriteString(w, `<table style="margin-top:1.5em"><tr><th>#</th><th>Side</th><th>Move</th><th>Time</th><th>Nodes</th><th>Dp</th><th>NPS</th><th>Score</th></tr>`)
			for i, m := range moves {
				side := "Black"
				if m.side == "w" {
					side = "White"
				}
				fmt.Fprintf(w, `<tr class="filter-row" data-board-idx="%d"><td>%d</td><td>%s</td><td>%s</td><td>%.1fms</td><td>%s</td><td>%d</td><td>%s</td><td>%+d</td></tr>`,
					openingPlies+i, openingPlies+m.num, side, m.move, m.timeMs, fmtVal(float64(m.nodes)), m.depth, fmtVal(float64(m.nps)), m.score)
			}
			io.WriteString(w, "</table>"+`</div>`+pageFoot)
		} else {
			io.WriteString(w, `<p style="color:var(--muted);margin-top:2em">No moves recorded — engines may have timed out or the coach restarted.</p>`)
		}
	} else {
		io.WriteString(w, `<p style="color:var(--muted);font-style:italic">No per-move data; engines may not support move stats.</p>`)
	}
	io.WriteString(w, pageFoot)
}

func deltaColor(d float64) string {
	if d > 0 {
		return "#4caf50"
	} else if d < 0 {
		return "#f44336"
	}
	return "var(--muted)"
}
