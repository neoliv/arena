package web

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

func (h *Handler) renderStatsBars(w http.ResponseWriter, r *http.Request, chart string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	type engineStats struct {
		Name                                string
		Games, AvgPly, Timeouts, TotalMoves int
		UnspentPct, AvgTimeS                float64
		AvgDurationS                        float64
	}
	var stats []engineStats

	rows, _ := h.DB.Query(`SELECT e.name, COUNT(DISTINCT g.id) as games,
			CAST(COALESCE(AVG(CASE WHEN g.black_id=e.id THEN g.black_depth+g.white_depth ELSE g.white_depth+g.black_depth END),0) AS INTEGER) as avg_ply,
			COALESCE((SELECT COUNT(*) FROM speed_stats ss WHERE ss.engine_id=e.id AND ss.timeouts>0),0) as timeouts,
			COALESCE((SELECT COUNT(*) FROM speed_stats ss WHERE ss.engine_id=e.id),0) as moves,
			CAST(COALESCE(AVG(CASE WHEN g.black_id=e.id THEN 100.0*(1.0-g.black_time_s/NULLIF(COALESCE(g.game_time_sec,60),0)) ELSE 100.0*(1.0-g.white_time_s/NULLIF(COALESCE(g.game_time_sec,60),0)) END),0) AS REAL) as unspent_pct,
			CAST(COALESCE(AVG(CASE WHEN g.black_id=e.id THEN g.black_time_s ELSE g.white_time_s END),0) AS REAL) as avg_time_s,
			CAST(COALESCE(AVG(g.black_time_s+g.white_time_s),0) AS REAL) as avg_duration_s
			FROM engines e LEFT JOIN games g ON g.black_id=e.id OR g.white_id=e.id
			GROUP BY e.name HAVING games>0 ORDER BY games DESC`)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var s engineStats
			rows.Scan(&s.Name, &s.Games, &s.AvgPly, &s.Timeouts, &s.TotalMoves, &s.UnspentPct, &s.AvgTimeS, &s.AvgDurationS)
			stats = append(stats, s)
		}
	}

	if len(stats) == 0 {
		io.WriteString(w, "<p>No data yet.</p>"+pageFoot)
		return
	}

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
	fmtSig3 := func(v float64) string {
		switch {
		case v >= 1e6:
			return fmt.Sprintf("%.2fM", v/1e6)
		case v >= 1e5:
			return fmt.Sprintf("%.1fk", v/1e3)
		case v >= 1e4:
			return fmt.Sprintf("%.2fk", v/1e3)
		case v >= 1e3:
			return fmt.Sprintf("%.2fk", v/1e3)
		case v >= 100:
			return fmt.Sprintf("%.0f", v)
		case v >= 10:
			return fmt.Sprintf("%.1f", v)
		default:
			return fmt.Sprintf("%.2f", v)
		}
	}

	// SVG bar chart helper
	maxW := 600
	barH := 20
	gap := 4
	maxLabelW := 0
	for _, s := range stats {
		if len(s.Name) > maxLabelW {
			maxLabelW = len(s.Name)
		}
	}
	labelX := maxLabelW*7 + 10
	rightPad := 96
	drawBars := func(title, unit string, getVal func(engineStats) float64, getMax func() float64, color string, fmtLabel func(float64) string, barLink func(string) string) string {
		if fmtLabel == nil {
			fmtLabel = fmtVal
		}
		var svg strings.Builder
		maxVal := getMax()
		if maxVal == 0 {
			maxVal = 1
		}
		height := len(stats)*(barH+gap) + 10
		totalW := labelX + maxW + rightPad
		fmt.Fprintf(&svg, `<h3>%s</h3><svg viewBox="0 0 %d %d" style="width:100%%;max-width:%dpx">`, title, totalW, height, totalW)
		for i, s := range stats {
			val := getVal(s)
			w := int(val / maxVal * float64(maxW))
			if w < 2 {
				w = 2
			}
			y := i * (barH + gap)
			link := ""
			if barLink != nil {
				link = barLink(s.Name)
			}
			if link != "" {
				fmt.Fprintf(&svg, `<a href="%s" style="text-decoration:none">`, link)
			}
			fmt.Fprintf(&svg, `<g class="filter-item"><text x="%d" y="%d" fill="var(--fg)" font-size="11" text-anchor="end">%s</text>`, labelX-6, y+12, s.Name)
			fmt.Fprintf(&svg, `<rect x="%d" y="%d" width="%d" height="%d" fill="%s" rx="2"><title>%s: %s%s — click for time distribution</title></rect>`, labelX, y, w, barH, color, s.Name, fmtLabel(val), unit)
			fmt.Fprintf(&svg, `<text x="%d" y="%d" fill="var(--fg)" font-size="11" font-weight="600">%s</text></g>`, labelX+w+8, y+12, fmtLabel(val)+unit)
			if link != "" {
				fmt.Fprintf(&svg, `</a>`)
			}
		}
		svg.WriteString(`</svg>`)
		return svg.String()
	}
	getGames := func(s engineStats) float64 { return float64(s.Games) }
	getMaxGames := func() float64 {
		m := 0.0
		for _, s := range stats {
			if float64(s.Games) > m {
				m = float64(s.Games)
			}
		}
		return m
	}
	switch chart {
	case "games":
		sort.Slice(stats, func(i, j int) bool { return stats[i].Games > stats[j].Games })
		var totalGames int
		h.DB.QueryRow("SELECT COUNT(*) FROM games").Scan(&totalGames)
		fmt.Fprintf(w, `<p style="color:var(--fg);margin-bottom:.5em;font-size:1.1em">Total games: <strong>%s</strong></p>`, commaNum(totalGames))
		io.WriteString(w, drawBars("Games per Engine", "", getGames, getMaxGames, chartColors[0], nil, nil))
		io.WriteString(w, h.renderPairBalance())
	case "unspent":
		barLink := func(name string) string {
			return "/stats?tab=unspent&engine=" + url.QueryEscape(name)
		}
		sort.Slice(stats, func(i, j int) bool { return stats[i].AvgDurationS > stats[j].AvgDurationS })
		getAvgDuration := func(s engineStats) float64 { return s.AvgDurationS }
		getMaxAvgDuration := func() float64 {
			m := 0.0
			for _, s := range stats {
				if s.AvgDurationS > m {
					m = s.AvgDurationS
				}
			}
			return m
		}
		io.WriteString(w, drawBars("Average Game Duration", " s", getAvgDuration, getMaxAvgDuration, chartColors[0], fmtSig3, barLink))
		sort.Slice(stats, func(i, j int) bool { return stats[i].UnspentPct > stats[j].UnspentPct })
		getUnspent := func(s engineStats) float64 { return s.UnspentPct }
		getMaxUnspent := func() float64 {
			m := 0.0
			for _, s := range stats {
				if s.UnspentPct > m {
					m = s.UnspentPct
				}
			}
			return m
		}
		io.WriteString(w, drawBars("Unspent Time (%)", "%", getUnspent, getMaxUnspent, chartColors[3], nil, barLink))
		// Time distribution histogram for a selected engine.
		if eng := r.URL.Query().Get("engine"); eng != "" {
			h.renderTimeDist(w, r, eng)
		}
	}

	switch chart {
	case "games":
		io.WriteString(w, `<p style="color:var(--muted);margin-top:2em">Number of games completed by each engine across all time controls.</p>`)
	case "unspent":
		io.WriteString(w, `<p style="color:var(--muted);margin-top:2em">How much of the time budget goes unused. Higher % means the engine finishes early.</p>`)
	}
}

// renderTimeDist plots the distribution of per-game thinking time for a
// single engine (its own clock time in each game, whether it played black
// or white). Rendered when an engine bar is clicked on the Time tab.
func (h *Handler) renderTimeDist(w http.ResponseWriter, r *http.Request, engine string) {
	var times []float64
	rows, err := h.DB.Query(`SELECT CASE WHEN g.black_id=e.id THEN g.black_time_s ELSE g.white_time_s END
		FROM games g JOIN engines e ON e.id=g.black_id OR e.id=g.white_id
		WHERE e.name=? AND g.black_time_s>0 AND g.white_time_s>0`, engine)
	if err == nil && rows != nil {
		defer rows.Close()
		for rows.Next() {
			var t float64
			if rows.Scan(&t) == nil {
				times = append(times, t)
			}
		}
	}
	if len(times) == 0 {
		io.WriteString(w, `<h3 style="margin-top:1.5em">`+htmlEscape(engine)+` — Time Distribution</h3><p style="color:var(--muted)">No per-game time data.</p>`)
		return
	}

	// Determine the time budget from the games' game_time_sec.
	var budget float64
	h.DB.QueryRow(`SELECT COALESCE(MAX(game_time_sec),0) FROM games g JOIN engines e ON e.id=g.black_id OR e.id=g.white_id WHERE e.name=?`, engine).Scan(&budget)
	if budget <= 0 {
		budget = 60
	}

	// Bin by whole seconds up to the budget (cap at budget to keep the chart readable).
	bins := int(math.Ceil(budget)) + 1
	if bins > 120 {
		bins = 120
	}
	counts := make([]int, bins)
	for _, t := range times {
		b := int(t)
		if b >= bins {
			b = bins - 1
		}
		counts[b]++
	}
	maxCount := 1
	for _, c := range counts {
		if c > maxCount {
			maxCount = c
		}
	}

	const svgw, svgh = 960, 320
	const left, right, top, bottom = 60, 20, 20, 40
	plotW := svgw - left - right
	plotH := float64(svgh - top - bottom)
	barW := float64(plotW) / float64(bins)

	fmt.Fprintf(w, `<h3 id="time-dist" style="margin-top:1.5em">%s — Time Distribution (%d games, %.0fs budget)</h3>`, htmlEscape(engine), len(times), budget)
	io.WriteString(w, `<div style="overflow-x:auto">`)
	fmt.Fprintf(w, `<svg viewBox="0 0 %d %d" style="min-width:%dpx">`, svgw, svgh, svgw)
	// Grid lines + Y labels
	for i := 0; i <= 4; i++ {
		val := maxCount * i / 4
		yy := top + plotH - float64(i)*plotH/4
		fmt.Fprintf(w, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" stroke="var(--border)" stroke-width="0.5"/>`, left, yy, svgw-right, yy)
		fmt.Fprintf(w, `<text x="%d" y="%.1f" fill="var(--muted)" font-size="10" text-anchor="end">%d</text>`, left-6, yy+4, val)
	}
	// Bars
	for b := 0; b < bins; b++ {
		if counts[b] == 0 {
			continue
		}
		hh := float64(counts[b]) / float64(maxCount) * plotH
		x := left + float64(b)*barW
		col := chartColors[0]
		if b%2 == 1 {
			col = chartColors[3]
		}
		fmt.Fprintf(w, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s" rx="1"><title>%ds: %d games (%.1f%%)</title></rect>`,
			x+0.5, top+plotH-hh, barW-1, hh, col, b, counts[b], 100.0*float64(counts[b])/float64(len(times)))
	}
	// X tick labels every 10s (or 5s if budget small)
	step := 10
	if budget <= 60 {
		step = 5
	}
	for b := 0; b <= bins-1; b += step {
		x := left + float64(b)*barW
		fmt.Fprintf(w, `<text x="%.1f" y="%d" fill="var(--muted)" font-size="10" text-anchor="middle">%d</text>`, x, svgh-15, b)
	}
	fmt.Fprintf(w, `<text x="%.1f" y="%d" fill="var(--muted)" font-size="11" text-anchor="middle">Engine thinking time per game (s)</text>`, float64(left+plotW)/2, svgh-3)
	fmt.Fprintf(w, `<text x="14" y="%d" fill="var(--muted)" font-size="10" text-anchor="middle" transform="rotate(-90 14 %d)">Games</text>`, svgh/2, svgh/2)
	fmt.Fprintf(w, `</svg></div><p style="color:var(--muted);margin-top:.5em"><a href="/stats?tab=unspent">← back to Time overview</a></p>`)
	// Scroll the distribution into view after the page renders.
	io.WriteString(w, `<script>window.addEventListener('load',function(){var el=document.getElementById('time-dist');if(el)el.scrollIntoView({behavior:'smooth',block:'start'})})</script>`)
}

// renderPairBalance shows how evenly matches are distributed across engine
// pairs. With pair-balanced scheduling every pair should converge to a
// similar game count; sparse pairs (<5) are the ones the matchmaker is
// actively filling.
func (h *Handler) renderPairBalance() string {
	var sb strings.Builder
	var nEngines int
	h.DB.QueryRow("SELECT COUNT(*) FROM engines").Scan(&nEngines)
	if nEngines < 2 {
		return ""
	}
	possible := nEngines * (nEngines - 1) / 2

	// Bucket pairs by games played. The "0" bucket (pairs that never met)
	// starts at possible−played and shrinks as the round-robin fills in.
	type bucket struct {
		label string
		n     int
	}
	buckets := []bucket{
		{"0", 0}, {"<5", 0}, {"5-9", 0}, {"10-19", 0}, {"20-39", 0}, {"40-79", 0}, {"80-159", 0}, {"160+", 0},
	}
	rows, err := h.DB.Query(`SELECT COUNT(*) as c FROM games GROUP BY MIN(black_id,white_id)||'|'||MAX(black_id,white_id)`)
	if err != nil || rows == nil {
		return ""
	}
	defer rows.Close()
	played := 0
	for rows.Next() {
		var c int
		if rows.Scan(&c) != nil {
			continue
		}
		played++
		switch {
		case c < 5:
			buckets[1].n++
		case c < 10:
			buckets[2].n++
		case c < 20:
			buckets[3].n++
		case c < 40:
			buckets[4].n++
		case c < 80:
			buckets[5].n++
		case c < 160:
			buckets[6].n++
		default:
			buckets[7].n++
		}
	}
	buckets[0].n = possible - played
	maxN := 1
	for _, b := range buckets {
		if b.n > maxN {
			maxN = b.n
		}
	}

	sb.WriteString(`<h2>Pair Balance</h2>`)
	fmt.Fprintf(&sb, `<p style="color:var(--muted);font-size:.95em">Games per engine pair — %d of %d pairs played so far (%d never met). Sparse pairs (&lt;5 games) are prioritized by the matchmaker.</p>`, played, possible, possible-played)
	sb.WriteString(`<table style="width:auto;min-width:400px"><tr><th style="text-align:right">Games/pair</th><th style="text-align:left">Pairs</th><th style="width:220px"></th></tr>`)
	for _, b := range buckets {
		w := int(float64(b.n) / float64(maxN) * 200)
		if w < 2 {
			w = 2
		}
		fmt.Fprintf(&sb, `<tr class="filter-row"><td style="text-align:right;padding-right:1em">%s</td><td style="text-align:right;padding-right:.6em">%d</td><td><span class="bar" style="width:%dpx"></span></td></tr>`, b.label, b.n, w)
	}
	sb.WriteString(`</table>`)
	return sb.String()
}
