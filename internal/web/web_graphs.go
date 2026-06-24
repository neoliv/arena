package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func (h *Handler) handleGraphs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tab := r.URL.Query().Get("tab")
	io.WriteString(w, pageHead+navHTML+searchJS+`<h1>Graphs</h1>`+filterBox+`
		<nav class="chart-tabs" style="margin-bottom:1.5em">
		<a href="?tab=elo" class="chart-tab" style="display:inline-block;padding:.35em .7em;border-radius:5px;font-size:1.1em;font-weight:600;text-decoration:none;border:1px solid var(--nav-hl);color:`+func()string{if tab==""||tab=="elo"{return"#fff"}else{return"var(--fg)"}}()+`;background:`+func()string{if tab==""||tab=="elo"{return"var(--nav-hl)"}else{return"rgba(56,136,85,0.06)"}}()+`">Elo</a>
		<a href="?tab=games" class="chart-tab" style="display:inline-block;padding:.35em .7em;border-radius:5px;font-size:1.1em;font-weight:600;text-decoration:none;border:1px solid var(--nav-hl);color:`+func()string{if tab=="games"{return"#fff"}else{return"var(--fg)"}}()+`;background:`+func()string{if tab=="games"{return"var(--nav-hl)"}else{return"rgba(56,136,85,0.06)"}}()+`">Games</a>
		<a href="?tab=timeout" class="chart-tab" style="display:inline-block;padding:.35em .7em;border-radius:5px;font-size:1.1em;font-weight:600;text-decoration:none;border:1px solid var(--nav-hl);color:`+func()string{if tab=="timeout"{return"#fff"}else{return"var(--fg)"}}()+`;background:`+func()string{if tab=="timeout"{return"var(--nav-hl)"}else{return"rgba(56,136,85,0.06)"}}()+`">Timeouts</a>
		<a href="?tab=unspent" class="chart-tab" style="display:inline-block;padding:.35em .7em;border-radius:5px;font-size:1.1em;font-weight:600;text-decoration:none;border:1px solid var(--nav-hl);color:`+func()string{if tab=="unspent"{return"#fff"}else{return"var(--fg)"}}()+`;background:`+func()string{if tab=="unspent"{return"var(--nav-hl)"}else{return"rgba(56,136,85,0.06)"}}()+`">Unspent</a>
		</div>`)

	switch tab {
	case "games":
		h.renderStatsBars(w, r, "games")
	case "timeout":
		h.renderStatsBars(w, r, "timeout")
	case "unspent":
		h.renderStatsBars(w, r, "unspent")
	default:
		h.renderEloChart(w, r)
	}
	io.WriteString(w, pageFoot)
}

func (h *Handler) renderEloChart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	rows, err := h.DB.Query(`SELECT e.name||' '||e.version, eh.created_at, eh.rating_after FROM elo_history eh JOIN engines e ON eh.engine_id=e.id ORDER BY eh.created_at`)
	if err != nil || rows == nil { io.WriteString(w, "<p>No data yet.</p>"+pageFoot); return }
	defer rows.Close()
	type point struct{ Engine, Date string; Elo float64 }
	var points []point
	for rows.Next() { var p point; rows.Scan(&p.Engine, &p.Date, &p.Elo); points = append(points, p) }
	if len(points) == 0 { io.WriteString(w, "<p>No data yet.</p>"+pageFoot); return }
	engineIdx := map[string]int{}
	var engineNames []string
	for _, p := range points { if _, ok := engineIdx[p.Engine]; !ok { engineIdx[p.Engine] = len(engineNames); engineNames = append(engineNames, p.Engine) } }
	minElo, maxElo := points[0].Elo, points[0].Elo
	for _, p := range points {
		if p.Elo < minElo { minElo = p.Elo }
		if p.Elo > maxElo { maxElo = p.Elo }
	}
	pad := (maxElo - minElo) * 0.1; if pad < 50 { pad = 50 }
	minElo -= pad; maxElo += pad
	svgh := 360; svgw := 700; left := 70; top := 10; graphh := float64(svgh - 50); graphw := float64(svgw - 20)

	// Build engine data for SVG polylines, sequential per engine.
	// Scale each engine's points so the last lands exactly at graphw,
	// avoiding vertical artefact from clamp at the right edge.
	type ep struct{ x, y float64 }
	engineData := make([][]ep, len(engineNames))
	for _, p := range points {
		idx := engineIdx[p.Engine]
		y := graphh - (p.Elo-minElo)/(maxElo-minElo)*graphh
		engineData[idx] = append(engineData[idx], ep{x: 0, y: y}) // x filled in below
	}
	for i, data := range engineData {
		n := len(data)
		if n == 0 { continue }
		for j := 0; j < n; j++ {
			if n == 1 {
				engineData[i][j].x = graphw / 2 // center single point
			} else {
				engineData[i][j].x = float64(j) / float64(n-1) * graphw
			}
		}
	}

	fmt.Fprintf(w, `<svg viewBox="0 0 %d %d" style="width:100%%;max-width:860px"><g transform="translate(%d,%d)">`, svgw+left+20, svgh+20, left, top)
	// Grid lines & axis
	gridSteps := 5
	for i := 0; i <= gridSteps; i++ {
		val := minElo + (maxElo-minElo)*float64(i)/float64(gridSteps)
		y := graphh - (val-minElo)/(maxElo-minElo)*graphh
		fmt.Fprintf(w, `<line x1="0" y1="%.0f" x2="%.0f" y2="%.0f" stroke="var(--border)" stroke-width="0.5"/>`, y, graphw, y)
		fmt.Fprintf(w, `<text x="-6" y="%.0f" text-anchor="end" fill="var(--muted)" font-size="10">%.0f</text>`, y+4, val)
	}
	fmt.Fprintf(w, `<text x="%.0f" y="%.0f" text-anchor="middle" fill="var(--fg)" font-size="12">Elo</text>`, float64(graphw)/2, float64(-25))
	fmt.Fprintf(w, `<text x="%.0f" y="%.0f" text-anchor="middle" fill="var(--muted)" font-size="10">Time →</text>`, graphw/2, graphh+30)
	// Data lines
	for i, data := range engineData {
		if len(data) < 2 { continue }
		pts := ""
		for _, pt := range data { pts += fmt.Sprintf("%.1f,%.1f ", pt.x, pt.y) }
		fmt.Fprintf(w, `<g class="filter-item"><title>%s</title><polyline fill="none" stroke="%s" stroke-width="2" points="%s"/></g>`, engineNames[i], chartColors[i%8], strings.TrimSpace(pts))
	}
	io.WriteString(w, `</g></svg>`)
	// Legend with filter support
	io.WriteString(w, `<div style="margin-top:1em">`)
	for i, e := range engineNames {
		fmt.Fprintf(w, `<span class="filter-item" style="color:%s;margin-right:1.2em;white-space:nowrap">● %s</span>`, chartColors[i%8], e)
	}
	io.WriteString(w, `</div>`)
}

func formatTimeControl(tc string) string {
		var v struct {
			Seconds float64 `json:"seconds"`
			Label   string  `json:"label"`
		}
		if err := json.Unmarshal([]byte(tc), &v); err == nil && v.Seconds > 0 {
			if v.Label != "" {
				return v.Label
			}
			return fmt.Sprintf("%.0fs", v.Seconds)
		}
		return tc
	}

func parseTime(s string) (float64, error) {
	t, err := time.Parse("2006-01-02T15:04:05Z", strings.TrimSpace(s))
	if err != nil { return 0, err }
	return float64(t.Unix()), nil
}

func (h *Handler) handleSpeed(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/charts?tab=speed&"+r.URL.RawQuery, http.StatusMovedPermanently)
	return
}

func (h *Handler) renderSpeedGraph(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, searchJS+filterBox)
	io.WriteString(w, `<table><tr><th>Ply</th><th>Engine</th><th>#</th><th>Depth</th><th>NPS</th><th>Timeouts</th></tr>`)
	sRows, _ := h.DB.Query(`SELECT ss.ply, e.name, SUM(ss.sample_count), CAST(SUM(ss.total_depth) AS REAL)/MAX(1,SUM(ss.sample_count)), CAST(SUM(ss.total_nps) AS REAL)/MAX(1,SUM(ss.sample_count)), SUM(ss.timeouts) FROM speed_stats ss JOIN engines e ON e.id=ss.engine_id GROUP BY ss.ply, e.name ORDER BY ss.ply, e.name`)
	if sRows != nil { defer sRows.Close(); for sRows.Next() { var ply, samples, timeouts int; var name string; var depth, nps float64; sRows.Scan(&ply, &name, &samples, &depth, &nps, &timeouts); fmt.Fprintf(w, `<tr class="filter-row"><td>%d</td><td>%s</td><td>%d</td><td>%.1f</td><td>%.0f</td><td>%d</td></tr>`, ply, name, samples, depth, nps, timeouts) } }
	io.WriteString(w, "</table>"+`</div>`+pageFoot)
}

