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

// engineColor returns a deterministic color for an engine name.
// Same name always gets the same color, across page loads and restarts.
// Uses a 16-color palette with a simple string hash to pick.
func engineColor(name string) string {
	h := 0
	for _, c := range name {
		h = h*31 + int(c)
	}
	if h < 0 {
		h = -h
	}
	return chartColors16[h%16]
}

// chartColors16 — 16 distinct chalk/pastel hues visible on light+dark bg.
var chartColors16 = [16]string{
	"#4caf50", "#6bd4ff", "#ffe66b", "#6bff8a",
	"#ff8a6b", "#c46bff", "#6bffe6", "#ffb86b",
	"#ff6b9d", "#6bb5ff", "#a0ff6b", "#ffd46b",
	"#6bffff", "#ff6bff", "#b0b0b0", "#ff9f43",
}

func (h *Handler) renderEloChart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	rows, err := h.DB.Query(`SELECT e.name, eh.match_id, eh.rating_after FROM elo_history eh JOIN engines e ON eh.engine_id=e.id WHERE eh.id IN (SELECT MAX(eh2.id) FROM elo_history eh2 GROUP BY eh2.engine_id, eh2.match_id) ORDER BY eh.match_id`)
	if err != nil || rows == nil {
		io.WriteString(w, "<p>No data yet.</p>"+pageFoot)
		return
	}
	defer rows.Close()
	type point struct {
		Engine  string
		MatchID int
		Elo     float64
	}
	var points []point
	for rows.Next() {
		var p point
		rows.Scan(&p.Engine, &p.MatchID, &p.Elo)
		points = append(points, p)
	}
	if len(points) == 0 {
		io.WriteString(w, "<p>No data yet.</p>"+pageFoot)
		return
	}
	engineIdx := map[string]int{}
	var engineNames []string
	for _, p := range points {
		if _, ok := engineIdx[p.Engine]; !ok {
			engineIdx[p.Engine] = len(engineNames)
			engineNames = append(engineNames, p.Engine)
		}
	}
	// Assign sequential match indices so gaps from deleted/disconnected
	// games don't create dead space on the X axis.
	matchSeq := make(map[int]int)
	seq := 0
	for _, p := range points {
		if _, ok := matchSeq[p.MatchID]; !ok {
			matchSeq[p.MatchID] = seq
			seq++
		}
	}

	minElo, maxElo := points[0].Elo, points[0].Elo
	minMatch, maxMatch := 0, seq-1
	for _, p := range points {
		if p.Elo < minElo {
			minElo = p.Elo
		}
		if p.Elo > maxElo {
			maxElo = p.Elo
		}
	}
	pad := (maxElo - minElo) * 0.1
	if pad < 50 {
		pad = 50
	}
	minElo -= pad
	maxElo += pad
	matchRange := float64(maxMatch - minMatch)
	if matchRange <= 0 {
		matchRange = 1
	}
	svgh := 360
	svgw := 700
	left := 70
	top := 10
	graphh := float64(svgh - 50)
	graphw := float64(svgw - 20)

	// X-axis: sequential match count (no gaps from deleted matches).
	// All engines share the same time axis.
	type ep struct{ x, y float64 }
	engineData := make([][]ep, len(engineNames))
	for _, p := range points {
		idx := engineIdx[p.Engine]
		x := float64(matchSeq[p.MatchID]-minMatch) / matchRange * graphw
		y := graphh - (p.Elo-minElo)/(maxElo-minElo)*graphh
		engineData[idx] = append(engineData[idx], ep{x: x, y: y})
	}

	// Hover-highlight JS: legend ↔ curve cross-highlighting
	io.WriteString(w, `<script>
(function(){
	var curves=document.querySelectorAll('.elo-curve');
	var labels=document.querySelectorAll('.elo-label');
	var legends=document.querySelectorAll('.elo-legend-item');
	function highlight(i){
		curves.forEach(function(c,j){
			if(i<0||j===i){c.style.strokeWidth='3.5';c.style.opacity='1'}
			else{c.style.strokeWidth='1';c.style.opacity='0.2'}
		});
		labels.forEach(function(l,j){
			if(i<0||j===i){l.style.opacity='1'}else{l.style.opacity='0.2'}
		});
		legends.forEach(function(l,j){
			if(i<0||j===i){l.style.opacity='1';l.style.fontWeight='600'}else{l.style.opacity='0.4';l.style.fontWeight='normal'}
		})
	}
	legends.forEach(function(el,i){
		el.addEventListener('mouseenter',function(){highlight(i)});
		el.addEventListener('mouseleave',function(){highlight(-1)})
	});
	curves.forEach(function(el,i){
		el.addEventListener('mouseenter',function(){highlight(i)});
		el.addEventListener('mouseleave',function(){highlight(-1)})
	})
})();
</`+`script>`)

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

	// Data lines with endpoint labels
	for i, data := range engineData {
		if len(data) < 2 {
			continue
		}
		pts := ""
		for _, pt := range data {
			pts += fmt.Sprintf("%.1f,%.1f ", pt.x, pt.y)
		}
		col := engineColor(engineNames[i])
		fmt.Fprintf(w, `<g class="filter-item"><title>%s</title><polyline class="elo-curve" fill="none" stroke="%s" stroke-width="2" points="%s" style="cursor:pointer"/></g>`,
			engineNames[i], col, strings.TrimSpace(pts))
		// Endpoint label: engine name at the last data point
		last := data[len(data)-1]
		fmt.Fprintf(w, `<text class="elo-label" x="%.1f" y="%.1f" dx="4" dy="-4" fill="%s" font-size="10" style="pointer-events:none">%s</text>`,
			last.x, last.y, col, engineNames[i])
	}
	io.WriteString(w, `</g></svg>`)

	// Legend with hover interaction
	io.WriteString(w, `<div style="margin-top:1em">`)
	for _, e := range engineNames {
		col := engineColor(e)
		fmt.Fprintf(w, `<span class="filter-item elo-legend-item" style="color:%s;margin-right:1.2em;white-space:nowrap;cursor:pointer;transition:all .15s">● %s</span>`, col, e)
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
	if err != nil {
		return 0, err
	}
	return float64(t.Unix()), nil
}
