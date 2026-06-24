package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

func (h *Handler) handleGameDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, pageHead+navHTML)

	var gid, mid, gnum, finalScore, blackNodes, whiteNodes, blackDepth, whiteDepth int
	var result, opening, bName, bVer, wName, wVer, tcJSON string
	var bTime, wTime, gameTimeSec float64
	var bElo, wElo, bEloBefore, wEloBefore float64
	err := h.DB.QueryRow(
		"SELECT g.id, g.match_id, g.game_number, g.result, COALESCE(g.final_score,0), COALESCE(g.opening_line,''), "+
			"COALESCE(g.black_time_s,0), COALESCE(g.white_time_s,0), COALESCE(g.black_nodes,0), COALESCE(g.white_nodes,0), "+
			"COALESCE(g.black_depth,0), COALESCE(g.white_depth,0), eb.name, eb.version, ew.name, ew.version, "+
			"COALESCE(m.time_control,'{}'), "+
			"COALESCE((SELECT rating_after FROM elo_history WHERE engine_id=g.black_id ORDER BY created_at DESC LIMIT 1), 1500.0), "+
			"COALESCE((SELECT rating_after FROM elo_history WHERE engine_id=g.white_id ORDER BY created_at DESC LIMIT 1), 1500.0), "+
			"COALESCE((SELECT rating_before FROM elo_history WHERE engine_id=g.black_id AND match_id=g.match_id ORDER BY created_at DESC LIMIT 1), 0.0), "+
			"COALESCE((SELECT rating_before FROM elo_history WHERE engine_id=g.white_id AND match_id=g.match_id ORDER BY created_at DESC LIMIT 1), 0.0) "+
			"FROM games g JOIN engines eb ON g.black_id=eb.id JOIN engines ew ON g.white_id=ew.id JOIN matches m ON m.id=g.match_id WHERE g.id=?",
		id).Scan(&gid, &mid, &gnum, &result, &finalScore, &opening, &bTime, &wTime, &blackNodes, &whiteNodes, &blackDepth, &whiteDepth, &bName, &bVer, &wName, &wVer, &tcJSON, &bElo, &wElo, &bEloBefore, &wEloBefore)
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

	// Line 1: game number + score, Elos right/left aligned
	fmt.Fprintf(w, `<div style="display:flex;align-items:baseline;justify-content:space-between;margin-bottom:.2em">
		<div style="flex:1"><h1 style="margin:0;font-size:1.4em">#%s <span style="color:var(--muted);font-weight:400">%d-%d</span></h1></div>
		<div style="flex:1;text-align:right;font-size:1.1em;padding-right:1.5em"><span style="color:%s">(%.0f %+d)</span></div>
		<div style="flex:1;text-align:left;font-size:1.1em;padding-left:1.5em"><span style="color:%s">(%+d %.0f)</span></div>
		<div style="flex:1"></div></div>`,
		id, bScore, wScore, deltaColor(bDelta), bElo, int(bDelta), deltaColor(wDelta), int(wDelta), wElo)
	// Line 2: player names — Black right-aligned, White left-aligned
	fmt.Fprintf(w, `<div style="display:flex;justify-content:space-between;margin-bottom:.6em">
		<div style="flex:1"></div>
		<div style="flex:1;text-align:right;font-size:1.15em;padding-right:1.5em"><a href="/engines/%s">%s %s</a></div>
		<div style="flex:1;text-align:left;font-size:1.15em;padding-left:1.5em"><a href="/engines/%s">%s %s</a></div>
		<div style="flex:1"></div></div>`,
		htmlEscape(bName), htmlEscape(bName), htmlEscape(bVer), htmlEscape(wName), htmlEscape(wName), htmlEscape(wVer))

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
	io.WriteString(w, `<br><br><div style="text-align:center;margin-bottom:.3em;color:var(--muted);font-size:.85em">`)
	fmt.Fprintf(w, `Match <a href="/matches/%d">#%d</a> (game %d) | <a href="/games/%d">other game</a>`, mid, mid, gnum, otherGame)
	if tcLabel != "" {
		fmt.Fprintf(w, ` | Time: %s (unspent B:%.0fs W:%.0fs)`, tcLabel, bUnspent, wUnspent)
	}
	if opening != "" {
		fmt.Fprintf(w, ` | Opening: %s`, htmlEscape(opening))
	}
	io.WriteString(w, `</div><br>`)

	mRows, _ := h.DB.Query("SELECT move_num, side, move, nodes, depth, time_ms, score FROM game_moves WHERE game_id=? ORDER BY move_num", gid)
	if mRows != nil {
		defer mRows.Close()
		type moveRow struct {
			num            int
			side, move     string
			nodes          int
			depth          int
			timeMs         float64
			nps            int
			score          int
		}
		var moves []moveRow
		maxTime, maxNodes, maxNPS, maxBScore, maxWScore := 0.0, 0.0, 0.0, 0.0, 0.0
		for mRows.Next() {
			var m moveRow
			mRows.Scan(&m.num, &m.side, &m.move, &m.nodes, &m.depth, &m.timeMs, &m.score)
				if m.timeMs > 0 { m.nps = int(float64(m.nodes) * 1000.0 / m.timeMs) }
			moves = append(moves, m)
			if m.timeMs > maxTime {
				maxTime = m.timeMs
			}
			if float64(m.nodes) > maxNodes { maxNodes = float64(m.nodes) }
			if float64(m.nps) > maxNPS { maxNPS = float64(m.nps) }
			if m.side == "b" { abs := float64(m.score); if abs < 0 { abs = -abs }; if abs > maxBScore { maxBScore = abs } }
			if m.side == "w" { abs := float64(m.score); if abs < 0 { abs = -abs }; if abs > maxWScore { maxWScore = abs } }
		}

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
			chartH := 320; topPad := 30
			totalPlies := openingPlies + len(moves)
			chartW := fmt.Sprintf("%d", max(600, totalPlies*14+50))
			if tab == "" { tab = "time" }
			io.WriteString(w, `<nav class="chart-tabs" style="margin-top:0;margin-bottom:1em">`)
			for _, t := range []struct{ key, label string }{
				{"time", "Time"}, {"nodes", "Nodes"}, {"nps", "NpS"}, {"diff", "Diff"}, {"score", "Score"},
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
				fmt.Fprintf(w, `<div style="display:flex;justify-content:space-between;align-items:baseline;margin-bottom:6px"><span style="color:#22d3ee;font-size:14px;font-weight:600">%s</span><span style="color:#d4c4a8;font-size:14px;font-weight:600">%s</span></div>`, bName, wName)
				io.WriteString(w, fmt.Sprintf(`<svg width="%s" height="%d">`, chartW, chartH+82))
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
					tickColor := "#6a6"; if metric == "score" { tickColor = "#22d3ee" }
				fmt.Fprintf(w, `<text x="0" y="%d" fill="%s" font-size="11">%s%s</text>`, y, tickColor, fmtVal(val), unit)
					fmt.Fprintf(w, `<line x1="34" y1="%d" x2="100%%" y2="%d" stroke="#2a4a2a" stroke-width="0.5"/>`, chartH-pct*chartH/100, chartH-pct*chartH/100)
				}
				if metric == "diff" {
					z := chartH/2 + topPad
					fmt.Fprintf(w, `<line x1="34" y1="%d" x2="100%%" y2="%d" stroke="#2a4a2a" stroke-width="1" stroke-dasharray="4,4"/>`, z, z)
				}
				if metric == "score" && maxValR > 0 {
					niceStepR := maxValR / 4
					if niceStepR >= 100 { niceStepR = float64(int(niceStepR/100+0.5)) * 100 } else if niceStepR >= 10 { niceStepR = float64(int(niceStepR/10+0.5)) * 10 } else { niceStepR = float64(int(niceStepR + 0.5)) }
					for pct := 0; pct <= 100; pct += 25 {
						y := chartH - pct*chartH/100 + 44
						valR := float64(pct) / 100.0 * niceStepR * 4; if pct == 100 { valR = maxValR }
						fmt.Fprintf(w, `<text x="100%%" y="%d" fill="#d4c4a8" font-size="10" text-anchor="end">%s%s</text>`, y, fmtVal(valR), unit)
					}
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
						maxVal = maxBScore; if m.side == "w" { maxVal = maxWScore }
						if maxVal < 1 { maxVal = 1 }
						val = float64(m.score)
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
					if h < 2 { h = 2 }
					barY := chartH - h + topPad
					if metric == "score" {
						mid := float64(chartH / 2)
						h = int((val / maxVal) * mid)
						if h < 0 { h = -h }
						if h < 2 { h = 2 }
						if val >= 0 { barY = int(mid) - h + topPad } else { barY = int(mid) + topPad }
					}
					color := "#22d3ee"
					if m.side == "w" { color = "#d4c4a8" }
					x := 34 + (openingPlies+i)*14
					titleVal := fmtVal(val)
					if metric == "diff" { titleVal = fmt.Sprintf("%+d", discDiffs[i]) }
					tip := fmt.Sprintf("%s %s: %s%s", m.side, m.move, titleVal, unit)
					switch metric {
					case "time": tip = fmt.Sprintf("%s %s: %.0fms, %s nodes", m.side, m.move, m.timeMs, fmtVal(float64(m.nodes)))
					case "nodes": tip = fmt.Sprintf("%s %s: %s nodes, depth %d", m.side, m.move, fmtVal(float64(m.nodes)), m.depth)
					case "nps": tip = fmt.Sprintf("%s %s: %s n/s, %.0fms", m.side, m.move, fmtVal(float64(m.nps)), m.timeMs)
					case "score": tip = fmt.Sprintf("%s %s: %+d cP", m.side, m.move, m.score)
					case "diff": tip = fmt.Sprintf("%s %s: %+d discs", m.side, m.move, discDiffs[i])
					}
					// Show ply number alongside move name
					plyLabel := fmt.Sprintf("%d:%s", openingPlies+i+1, m.move)
					fmt.Fprintf(w, `<rect x="%d" y="%d" width="12" height="%d" fill="%s" rx="1"><title>%s</title></rect>`, x, barY, h, color, tip)
					fmt.Fprintf(w, `<text x="%d" y="%d" fill="%s" font-size="9" text-anchor="middle">%s</text>`, x+6, chartH+20+topPad, color, htmlEscape(plyLabel))
				}
				io.WriteString(w, `</svg></div>`)
			}
			renderChart("time", maxTime, 0, "ms", "Time per move (ms)")
			renderChart("nodes", maxNodes, 0, "", "Nodes explored")
			renderChart("nps", maxNPS, 0, "", "Nodes per second")
			renderChart("diff", maxDiscDiff, 0, "", "Disc diff (B-W)")
			renderChart("score", maxBScore, maxWScore, "", "Score (cP)")

			io.WriteString(w, `<table style="margin-top:1.5em"><tr><th>#</th><th>Side</th><th>Move</th><th>Time</th><th>Nodes</th><th>Depth</th><th>NPS</th><th>Score</th></tr>`)
			for _, m := range moves {
				side := "Black"
				if m.side == "w" {
					side = "White"
				}
				fmt.Fprintf(w, `<tr class="filter-row"><td>%d</td><td>%s</td><td>%s</td><td>%.1fms</td><td>%d</td><td>%d</td><td>%d</td><td>%+d</td></tr>`,
					openingPlies+m.num, side, m.move, m.timeMs, m.nodes, m.depth, m.nps, m.score)
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

func (h *Handler) OLD_handleGameDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, pageHead+navHTML)
	fmt.Fprintf(w, "<h1>Game #%s</h1>", id)

	var gid, mid, gnum, finalScore, blackNodes, whiteNodes, blackDepth, whiteDepth int
	var result, opening, bName, bVer, wName, wVer string
	var bTime, wTime float64
	var bElo, wElo float64
	err := h.DB.QueryRow(
		"SELECT g.id, g.match_id, g.game_number, g.result, COALESCE(g.final_score,0), COALESCE(g.opening_line,''), "+
			"COALESCE(g.black_time_s,0), COALESCE(g.white_time_s,0), COALESCE(g.black_nodes,0), COALESCE(g.white_nodes,0), "+
			"COALESCE(g.black_depth,0), COALESCE(g.white_depth,0), eb.name, eb.version, ew.name, ew.version, "+
			"COALESCE((SELECT rating_after FROM elo_history WHERE engine_id=g.black_id ORDER BY created_at DESC LIMIT 1), 1500.0), "+
			"COALESCE((SELECT rating_after FROM elo_history WHERE engine_id=g.white_id ORDER BY created_at DESC LIMIT 1), 1500.0) "+
			"FROM games g JOIN engines eb ON g.black_id=eb.id JOIN engines ew ON g.white_id=ew.id WHERE g.id=?",
		id).Scan(&gid, &mid, &gnum, &result, &finalScore, &opening, &bTime, &wTime, &blackNodes, &whiteNodes, &blackDepth, &whiteDepth, &bName, &bVer, &wName, &wVer, &bElo, &wElo)
	if err != nil {
		io.WriteString(w, "<p>Game not found.</p>"+pageFoot)
		return
	}

	badge := `<span class="badge draw">D</span>`
	if result == "1-0" {
		badge = `<span class="badge win">W</span>`
	} else if result == "0-1" {
		badge = `<span class="badge loss">L</span>`
	}
	fmt.Fprintf(w, `<table class="stats-table">`)
	fmt.Fprintf(w, `<tr><td>Match</td><td><a href="/matches/%d">#%d</a> (game %d)</td></tr>`, mid, mid, gnum)
	fmt.Fprintf(w, `<tr><td>Black</td><td><a href="/engines/%s">%s %s</a> <small style="color:var(--muted)">(%.0f)</small></td></tr>`, bName, bName, bVer, bElo)
	fmt.Fprintf(w, `<tr><td>White</td><td><a href="/engines/%s">%s %s</a> <small style="color:var(--muted)">(%.0f)</small></td></tr>`, wName, wName, wVer, wElo)
	fmt.Fprintf(w, `<tr><td>Result</td><td>%s %s</td></tr>`, result, badge)
	fmt.Fprintf(w, `<tr><td>Score</td><td>%+d</td></tr>`, finalScore)
	fmt.Fprintf(w, `<tr><td>Opening</td><td>%s</td></tr>`, opening)
	fmt.Fprintf(w, `<tr><td>Black time</td><td>%.1fs</td></tr>`, bTime)
	fmt.Fprintf(w, `<tr><td>White time</td><td>%.1fs</td></tr>`, wTime)
	fmt.Fprintf(w, `<tr><td>Black stats</td><td>%d nodes / depth %d</td></tr>`, blackNodes, blackDepth)
	fmt.Fprintf(w, `<tr><td>White stats</td><td>%d nodes / depth %d</td></tr></table>`, whiteNodes, whiteDepth)
	io.WriteString(w, pageFoot)
}


