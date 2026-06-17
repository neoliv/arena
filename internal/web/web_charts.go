package web

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

func (h *Handler) renderStatsBars(w http.ResponseWriter, r *http.Request, chart string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
		type engineStats struct {
		Name string; Games, AvgPly, Timeouts, TotalMoves int; UnspentPct, AvgTimeS float64
	}
	var stats []engineStats

	rows, _ := h.DB.Query(`SELECT e.name, COUNT(DISTINCT g.id) as games,
		CAST(COALESCE(AVG(CASE WHEN g.black_id=e.id THEN g.black_depth+g.white_depth ELSE g.white_depth+g.black_depth END),0) AS INTEGER) as avg_ply,
		COALESCE((SELECT COUNT(*) FROM speed_stats ss WHERE ss.engine_id=e.id AND ss.timeouts>0),0) as timeouts,
		COALESCE((SELECT COUNT(*) FROM speed_stats ss WHERE ss.engine_id=e.id),0) as moves,
		CAST(COALESCE(AVG(CASE WHEN g.black_id=e.id THEN 100.0*(1.0-g.black_time_s/NULLIF(COALESCE((SELECT CAST(json_extract(m.time_control,'$.seconds') AS REAL) FROM matches m WHERE m.id=g.match_id),60),0)) ELSE 100.0*(1.0-g.white_time_s/NULLIF(COALESCE((SELECT CAST(json_extract(m.time_control,'$.seconds') AS REAL) FROM matches m WHERE m.id=g.match_id),60),0)) END),0) AS REAL) as unspent_pct,
		CAST(COALESCE(AVG(CASE WHEN g.black_id=e.id THEN g.black_time_s ELSE g.white_time_s END),0) AS REAL) as avg_time_s
		FROM engines e LEFT JOIN games g ON g.black_id=e.id OR g.white_id=e.id
		GROUP BY e.name HAVING games>0 ORDER BY games DESC`)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var s engineStats
			rows.Scan(&s.Name, &s.Games, &s.AvgPly, &s.Timeouts, &s.TotalMoves, &s.UnspentPct, &s.AvgTimeS)
			stats = append(stats, s)
		}
	}

	if len(stats) == 0 { io.WriteString(w, "<p>No data yet.</p>"+pageFoot); return }

	fmtVal := func(v float64) string {
		switch {
		case v >= 1e9: return fmt.Sprintf("%.1fG", v/1e9)
		case v >= 1e6: return fmt.Sprintf("%.0fM", v/1e6)
		case v >= 1e3: return fmt.Sprintf("%.0fk", v/1e3)
		default: return fmt.Sprintf("%.0f", v)
		}
	}

	// SVG bar chart helper
	maxW := 600; barH := 20; gap := 4
	maxLabelW := 0
	for _, s := range stats { if len(s.Name) > maxLabelW { maxLabelW = len(s.Name) } }
	labelX := maxLabelW*7 + 10
	rightPad := 96
	drawBars := func(title, unit string, getVal func(engineStats) float64, getMax func() float64, color string) string {
		var svg strings.Builder
		maxVal := getMax()
		if maxVal == 0 { maxVal = 1 }
		height := len(stats)*(barH+gap) + 10
		totalW := labelX + maxW + rightPad
		fmt.Fprintf(&svg, `<h3>%s</h3><svg viewBox="0 0 %d %d" style="width:100%%;max-width:%dpx">`, title, totalW, height, totalW)
		for i, s := range stats {
			val := getVal(s)
			w := int(val / maxVal * float64(maxW))
			if w < 2 { w = 2 }
			y := i*(barH+gap)
			fmt.Fprintf(&svg, `<g class="filter-item"><text x="%d" y="%d" fill="var(--fg)" font-size="11" text-anchor="end">%s</text>`, labelX-6, y+12, s.Name)
			fmt.Fprintf(&svg, `<rect x="%d" y="%d" width="%d" height="%d" fill="%s" rx="2"/>`, labelX, y, w, barH, color)
			fmt.Fprintf(&svg, `<text x="%d" y="%d" fill="var(--fg)" font-size="11" font-weight="600">%s</text></g>`, labelX+w+8, y+12, fmtVal(val)+unit)
		}
		svg.WriteString(`</svg>`)
		return svg.String()
	}
	getGames := func(s engineStats) float64 { return float64(s.Games) }
	getMaxGames := func() float64 { m := 0.0; for _, s := range stats { if float64(s.Games) > m { m = float64(s.Games) } }; return m }
	switch chart {
	case "games":
		sort.Slice(stats, func(i, j int) bool { return stats[i].Games > stats[j].Games })
		io.WriteString(w, drawBars("Games per Engine", "", getGames, getMaxGames, chartColors[0]))
	case "length":
		sort.Slice(stats, func(i, j int) bool { return stats[i].AvgPly > stats[j].AvgPly })
		getPly := func(s engineStats) float64 { return float64(s.AvgPly) }
	getMaxPly := func() float64 { m := 0.0; for _, s := range stats { if float64(s.AvgPly) > m { m = float64(s.AvgPly) } }; return m }
	io.WriteString(w, drawBars("Average Game Length (plies)", "", getPly, getMaxPly, chartColors[1]))
	case "timeout":
		totalTimeouts := 0; for _, s := range stats { totalTimeouts += s.Timeouts }
		if totalTimeouts == 0 {
			io.WriteString(w, `<p style="color:var(--muted);margin-top:2em">No timeouts recorded — all engines stay within their time budget.</p>`)
		} else if stats[0].TotalMoves > 0 {
		sort.Slice(stats, func(i, j int) bool { return float64(stats[i].Timeouts)/float64(max(stats[i].TotalMoves,1)) > float64(stats[j].Timeouts)/float64(max(stats[j].TotalMoves,1)) })
		getTO := func(s engineStats) float64 { return float64(s.Timeouts) * 100 / float64(max(s.TotalMoves,1)) }
		getMaxTO := func() float64 { m := 0.0; for _, s := range stats { v := getTO(s); if v > m { m = v } }; return m }
		io.WriteString(w, drawBars("Timeout Rate (%)", "%", getTO, getMaxTO, chartColors[2]))
		}
	case "unspent":
		sort.Slice(stats, func(i, j int) bool { return stats[i].UnspentPct > stats[j].UnspentPct })
		getUnspent := func(s engineStats) float64 { return s.UnspentPct }
	getMaxUnspent := func() float64 { m := 0.0; for _, s := range stats { if s.UnspentPct > m { m = s.UnspentPct } }; return m }
	io.WriteString(w, drawBars("Unspent Time (%)", "%", getUnspent, getMaxUnspent, chartColors[3]))
	}

	switch chart {
	case "games": io.WriteString(w, `<p style="color:var(--muted);margin-top:2em">Number of games completed by each engine across all time controls.</p>`)
	case "length": io.WriteString(w, `<p style="color:var(--muted);margin-top:2em">Average game length in plies. Higher means more moves per game.</p>`)
	case "timeout": io.WriteString(w, `<p style="color:var(--muted);margin-top:2em">Percentage of moves where the engine exceeded its time budget.</p>`)
	case "unspent": io.WriteString(w, `<p style="color:var(--muted);margin-top:2em">How much of the time budget goes unused. Higher % means the engine finishes early.</p>`)
	}
	}


