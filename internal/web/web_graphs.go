package web

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/neoliv/arena/internal/game"
	"time"
)

func (h *Handler) handleGraphs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tab := r.URL.Query().Get("tab")
	io.WriteString(w, pageHead+navHTML+searchJS+`<h1>Stats</h1>`+filterBox+`
		<nav class="chart-tabs" style="margin-bottom:1.5em">
		<a href="?tab=elo" class="chart-tab" style="display:inline-block;padding:.35em .7em;border-radius:5px;font-size:1.1em;font-weight:600;text-decoration:none;border:1px solid var(--nav-hl);color:`+func() string {
		if tab == "" || tab == "elo" {
			return "#fff"
		} else {
			return "var(--fg)"
		}
	}()+`;background:`+func() string {
		if tab == "" || tab == "elo" {
			return "var(--nav-hl)"
		} else {
			return "rgba(56,136,85,0.06)"
		}
	}()+`">Elo</a>
		<a href="?tab=errors" class="chart-tab" style="display:inline-block;padding:.35em .7em;border-radius:5px;font-size:1.1em;font-weight:600;text-decoration:none;`+func() string {
		var n int
		h.DB.QueryRow("SELECT COUNT(*) FROM games WHERE error_code != 0").Scan(&n)
		if n > 0 && tab != "errors" {
			return "border:1px solid rgba(244,67,54,0.6);color:var(--fg);background:rgba(244,67,54,0.2)"
		}
		if tab == "errors" {
			if n > 0 {
				return "border:1px solid rgba(244,67,54,0.8);color:#fff;background:rgba(244,67,54,0.5)"
			}
			return "border:1px solid var(--nav-hl);color:#fff;background:var(--nav-hl)"
		}
		return "border:1px solid var(--nav-hl);color:var(--fg);background:rgba(56,136,85,0.06)"
	}()+`">Errors</a>
		<a href="?tab=games" class="chart-tab" style="display:inline-block;padding:.35em .7em;border-radius:5px;font-size:1.1em;font-weight:600;text-decoration:none;border:1px solid var(--nav-hl);color:`+func() string {
		if tab == "games" {
			return "#fff"
		} else {
			return "var(--fg)"
		}
	}()+`;background:`+func() string {
		if tab == "games" {
			return "var(--nav-hl)"
		} else {
			return "rgba(56,136,85,0.06)"
		}
	}()+`">Played</a>
		<a href="?tab=unspent" class="chart-tab" style="display:inline-block;padding:.35em .7em;border-radius:5px;font-size:1.1em;font-weight:600;text-decoration:none;border:1px solid var(--nav-hl);color:`+func() string {
		if tab == "unspent" {
			return "#fff"
		} else {
			return "var(--fg)"
		}
	}()+`;background:`+func() string {
		if tab == "unspent" {
			return "var(--nav-hl)"
		} else {
			return "rgba(56,136,85,0.06)"
		}
	}()+`">Unspent</a>
		</nav>`)

	switch tab {
	case "games":
		h.renderStatsBars(w, r, "games")
	case "unspent":
		h.renderStatsBars(w, r, "unspent")
	case "errors":
		h.renderErrorChart(w, r)
	default:
		h.renderEloChart(w, r)
		h.renderRanksTable(w, r)
	}
	io.WriteString(w, pageFoot)
}

// renderRanksTable renders the player rankings table — merged below the Elo chart.
func (h *Handler) renderRanksTable(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, `<h2>Player Rankings</h2><table><tr><th onclick="st(this.closest('table'),0,true)">#</th><th onclick="st(this.closest('table'),1,false)">Player</th><th onclick="st(this.closest('table'),2,true)">Elo</th><th onclick="st(this.closest('table'),3,true)">+/-</th><th onclick="st(this.closest('table'),4,true)">Games</th><th onclick="st(this.closest('table'),5,false)">W/L/D</th><th onclick="st(this.closest('table'),6,false)">Trend</th></tr>`)
	rows, err := h.DB.Query(`SELECT e.id, e.name, e.version, COALESCE(e.engine_id,''), COALESCE((SELECT rating_after FROM elo_history WHERE engine_id=e.id ORDER BY created_at DESC LIMIT 1), 1500.0), (SELECT COUNT(*) FROM games WHERE black_id=e.id OR white_id=e.id) as g, (SELECT COUNT(*) FROM games WHERE (black_id=e.id AND result='1-0') OR (white_id=e.id AND result='0-1')), (SELECT COUNT(*) FROM games WHERE (black_id=e.id AND result='0-1') OR (white_id=e.id AND result='1-0')), (SELECT COUNT(*) FROM games WHERE (black_id=e.id OR white_id=e.id) AND result='1/2') FROM engines e ORDER BY 5 DESC`)
	if err != nil || rows == nil {
		io.WriteString(w, "</table>")
		return
	}
	defer rows.Close()
	type eng struct {
		Name, Version, EngineID string
		ID                      int
		Elo                     float64
		Games, W, L, D          int
	}
	var engines []eng
	for rows.Next() {
		var e eng
		rows.Scan(&e.ID, &e.Name, &e.Version, &e.EngineID, &e.Elo, &e.Games, &e.W, &e.L, &e.D)
		engines = append(engines, e)
	}
	for i, e := range engines {
		ci := 400.0 / math.Sqrt(math.Max(float64(e.Games), 1))
		trend := "—"
		if e.Games >= 10 {
			var oldElo float64
			h.DB.QueryRow(`SELECT rating_before FROM elo_history WHERE engine_id=? ORDER BY created_at ASC LIMIT 1`, e.ID).Scan(&oldElo)
			if oldElo > 0 {
				if e.Elo > oldElo+10 {
					trend = "▲"
				} else if e.Elo < oldElo-10 {
					trend = "▼"
				} else {
					trend = "→"
				}
			}
		}
		var wr string
		if e.Games > 0 {
			wr = fmt.Sprintf("%d/%d/%d", e.W, e.L, e.D)
		} else {
			wr = "—"
		}
		fmt.Fprintf(w, `<tr class="filter-row"><td>%d</td><td><a href="/engines/%s">%s %s</a> <small style="color:var(--muted)">%s</small></td><td>%.0f</td><td>±%.0f</td><td>%d</td><td>%s</td><td>%s</td></tr>`,
			i+1, e.Name, e.Name, e.Version, e.EngineID[:min(8, len(e.EngineID))], e.Elo, ci, e.Games, wr, trend)
	}
	io.WriteString(w, "</table>")
}

// enginePrefix extracts the engine family prefix from a name.
// Examples: "nrsi-0.1.0-d10-e12" → "nrsi", "egar-7.8.1-d6-e12" → "egar"
func enginePrefix(name string) string {
	for i, c := range name {
		if c == '-' {
			return name[:i]
		}
	}
	return name
}

// prefixBaseHue returns a well-separated hue for an engine prefix.
// Prefixes are sorted alphabetically and assigned evenly-spaced hues
// around the color wheel so each engine family gets a distinct color.
// Within a family, variants differ in saturation/lightness.
func prefixBaseHue(prefix string) int {
	// Collect unique prefixes sorted alphabetically.
	seen := map[string]bool{}
	var prefixes []string
	for _, name := range allEngineNames {
		p := enginePrefix(name)
		if !seen[p] {
			seen[p] = true
			prefixes = append(prefixes, p)
		}
	}
	sort.Strings(prefixes)
	// Swap nrsi ↔ neur: nrsi gets teal (~180°), neur gets purple (~270°).
	swapIfBothPresent := func(a, b string) {
		ia, ib := -1, -1
		for i, p := range prefixes {
			if p == a {
				ia = i
			}
			if p == b {
				ib = i
			}
		}
		if ia >= 0 && ib >= 0 {
			prefixes[ia], prefixes[ib] = prefixes[ib], prefixes[ia]
		}
	}
	swapIfBothPresent("nrsi", "neur")
	for i, p := range prefixes {
		if p == prefix {
			// Evenly spaced around the hue wheel, starting at 0°.
			return i * 360 / len(prefixes)
		}
	}
	// Fallback: hash if not found (shouldn't happen).
	h := 0
	for _, c := range prefix {
		h = h*31 + int(c)
	}
	if h < 0 {
		h = -h
	}
	return h % 360
}

// engineColor returns a deterministic color for an engine name.
// Engines sharing the same prefix (same family) get colors in the same hue,
// making it easy to spot strength variants of the same engine.
// Within a family, variants differ in saturation/lightness.
func engineColor(name string) string {
	prefix := enginePrefix(name)
	baseHue := prefixBaseHue(prefix)

	// Count siblings: how many engines share this prefix.
	siblings := 0
	for _, e := range allEngineNames {
		if enginePrefix(e) == prefix {
			siblings++
		}
	}

	// Hash full name for deterministic variant within the family.
	fh := 0
	for _, c := range name {
		fh = fh*31 + int(c)
	}
	if fh < 0 {
		fh = -fh
	}

	if siblings <= 1 {
		sat := 30 + fh%20
		light := 58 + fh%17
		return fmt.Sprintf("hsl(%d,%d%%,%d%%)", baseHue, sat, light)
	}

	// Find this engine's index among its siblings.
	sibIdx := 0
	for _, e := range allEngineNames {
		if e == name {
			break
		}
		if enginePrefix(e) == prefix {
			sibIdx++
		}
	}

	// Spread variants: earlier → more saturated+darker, later → more pastel+lighter.
	fraction := float64(sibIdx) / float64(siblings-1)
	sat := 25 + int(fraction*25)
	light := 70 - int(fraction*12)
	sat += (fh % 7) - 3
	light += ((fh / 7) % 5) - 2
	if sat < 20 {
		sat = 20
	}
	if sat > 60 {
		sat = 60
	}
	if light < 50 {
		light = 50
	}
	if light > 78 {
		light = 78
	}

	return fmt.Sprintf("hsl(%d,%d%%,%d%%)", baseHue, sat, light)
}

// allEngineNames is populated before rendering to enable sibling-aware coloring.
var allEngineNames []string

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
	// Populate global list for sibling-aware engineColor.
	allEngineNames = engineNames
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
	right := 130 // room for endpoint labels
	top := 10
	graphh := float64(svgh - 50)
	graphw := float64(svgw)

	// X-axis: sequential match count (no gaps from deleted matches).
	// All engines share the same time axis.
	type ep struct {
		x, y float64
		seq  int
	}
	engineData := make([][]ep, len(engineNames))
	maxSeq := 0
	for _, p := range points {
		idx := engineIdx[p.Engine]
		seq := matchSeq[p.MatchID]
		x := float64(seq-minMatch) / matchRange * graphw
		y := graphh - (p.Elo-minElo)/(maxElo-minElo)*graphh
		engineData[idx] = append(engineData[idx], ep{x: x, y: y, seq: seq})
		if seq > maxSeq {
			maxSeq = seq
		}
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

	fmt.Fprintf(w, `<svg viewBox="0 0 %d %d" style="width:100%%;max-width:960px"><g transform="translate(%d,%d)">`, svgw+left+right, svgh+20, left, top)
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

	// Data lines with endpoint labels — stagger vertically to avoid overlap.
	// Retired engines (last game >100 matches ago) keep their curve but
	// get no right-side label — they still appear in the legend below.
	//
	// First pass: draw all curves.
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
	}
	// Second pass: labels only for active (non-retired) engines, staggered.
	type labelInfo struct {
		idx  int
		y    float64
		col  string
		name string
	}
	var activeLabels []labelInfo
	for i, data := range engineData {
		if len(data) < 2 {
			continue
		}
		if data[len(data)-1].seq < maxSeq-100 {
			continue
		} // retired
		activeLabels = append(activeLabels, labelInfo{idx: i, y: data[len(data)-1].y,
			col: engineColor(engineNames[i]), name: engineNames[i]})
	}
	sort.Slice(activeLabels, func(a, b int) bool { return activeLabels[a].y < activeLabels[b].y })
	const labelH = 13.0
	for i := 1; i < len(activeLabels); i++ {
		if activeLabels[i].y < activeLabels[i-1].y+labelH {
			activeLabels[i].y = activeLabels[i-1].y + labelH
		}
	}
	for i := range activeLabels {
		if activeLabels[i].y < float64(top) {
			activeLabels[i].y = float64(top)
		}
		if activeLabels[i].y > graphh {
			activeLabels[i].y = graphh
		}
	}
	for _, lb := range activeLabels {
		data := engineData[lb.idx]
		last := data[len(data)-1]
		fmt.Fprintf(w, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="0.8" stroke-dasharray="2,2"/>`,
			last.x, last.y, graphw, lb.y, lb.col)
		fmt.Fprintf(w, `<text class="elo-label" x="%.1f" y="%.1f" dx="4" dy="3" fill="%s" font-size="10" text-anchor="start">%s</text>`,
			graphw, lb.y, lb.col, lb.name)
	}
	io.WriteString(w, `</g></svg>`)

	// Legend with hover interaction — flex-wrap keeps names within chart width.
	io.WriteString(w, `<div style="margin-top:1em;display:flex;flex-wrap:wrap;max-width:960px">`)
	for _, e := range engineNames {
		col := engineColor(e)
		fmt.Fprintf(w, `<span class="filter-item elo-legend-item" style="color:%s;margin-right:1.2em;margin-bottom:.3em;white-space:nowrap;cursor:pointer;transition:all .15s">● %s</span>`, col, e)
	}
	io.WriteString(w, `</div>`)
}

func (h *Handler) renderErrorChart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Infra errors: disconnect=1 but no coach verdict → infrastructure.
	var infraCount int
	h.DB.QueryRow(`SELECT COUNT(*) FROM games WHERE disconnect=1 AND error_code=0`).Scan(&infraCount)

	// Attribute errors to the LOSING engine only (the one who committed the fault).
	// result "1-0" = Black wins → White lost (white_id at fault).
	// result "0-1" = White wins → Black lost (black_id at fault).
	rows, err := h.DB.Query(`SELECT e.name, g.error_code, COUNT(*) as cnt,
			(SELECT COUNT(*) FROM games WHERE black_id=e.id OR white_id=e.id) as total_games
			FROM games g JOIN engines e ON (
				(g.result='1-0' AND e.id=g.white_id) OR
				(g.result='0-1' AND e.id=g.black_id)
			)
			WHERE g.error_code != 0
			GROUP BY e.name, g.error_code ORDER BY e.name, cnt DESC`)
	if err != nil || rows == nil {
		io.WriteString(w, "<p>No error data.</p>")
		return
	}
	defer rows.Close()
	type errBar struct {
		engine       string
		ecode        int8
		count, games int
		recentIDs    []int
	}
	var bars []errBar
	for rows.Next() {
		var b errBar
		rows.Scan(&b.engine, &b.ecode, &b.count, &b.games)
		bars = append(bars, b)
	}

	// Fetch up to 5 most recent game IDs for each (engine, error_code) pair.
	recentRows, recentErr := h.DB.Query(`SELECT e.name, g.error_code, g.id
		FROM games g JOIN engines e ON (
			(g.result='1-0' AND e.id=g.white_id) OR
			(g.result='0-1' AND e.id=g.black_id)
		)
		WHERE g.error_code != 0 ORDER BY e.name, g.error_code, g.created_at DESC`)
	if recentErr == nil && recentRows != nil {
		defer recentRows.Close()
		recentMap := map[string][]int{} // key: "engine|ecode"
		for recentRows.Next() {
			var engName string
			var ecode int8
			var gid int
			recentRows.Scan(&engName, &ecode, &gid)
			key := engName + "|" + strconv.Itoa(int(ecode))
			if len(recentMap[key]) < 5 {
				recentMap[key] = append(recentMap[key], gid)
			}
		}
		for i := range bars {
			key := bars[i].engine + "|" + strconv.Itoa(int(bars[i].ecode))
			bars[i].recentIDs = recentMap[key]
		}
	}

	if len(bars) == 0 {
		io.WriteString(w, "<p>No errors recorded — all games clean.</p>")
		return
	}

	errorColors := map[int8]string{
		game.ErrIllegalMove:     "#e05555",
		game.ErrTimeout:         "#e8a840",
		game.ErrCrash:           "#b055c0",
		game.ErrResign:          "#78909c",
		game.ErrInvalidResponse: "#a08870",
	}

	// Group by engine, preserving order
	type engGroup struct {
		name  string
		games int
		bars  []errBar
	}
	var groups []engGroup
	seen := map[string]int{}
	for _, b := range bars {
		if idx, ok := seen[b.engine]; ok {
			groups[idx].bars = append(groups[idx].bars, b)
		} else {
			seen[b.engine] = len(groups)
			groups = append(groups, engGroup{name: b.engine, games: b.games, bars: []errBar{b}})
		}
	}

	io.WriteString(w, `<h2>Errors</h2>`)
	if infraCount > 0 {
		fmt.Fprintf(w, `<p style="color:var(--muted);margin-bottom:1em;font-size:1em;font-weight:600">%d infrastructure failures (no engine blame) — coach connection lost or network issue.</p>`, infraCount)
	}
	io.WriteString(w, `<div style="max-width:680px">`)
	barMax := 520.0 // px at 100%
	for _, g := range groups {
		fmt.Fprintf(w, `<div style="margin-bottom:1.2em" class="filter-item"><strong><a href="/engines/%s">%s</a></strong> <span style="color:var(--muted);font-size:.85em">%d games</span>`,
			htmlEscape(g.name), htmlEscape(g.name), g.games)
		for _, b := range g.bars {
			pct := float64(b.count) / float64(b.games) * 100
			bw := pct / 100 * barMax
			if bw < 2 {
				bw = 2
			}
			col := errorColors[b.ecode]
			if col == "" {
				col = "#888"
			}
			idLinks := ""
			if len(b.recentIDs) > 0 {
				parts := make([]string, len(b.recentIDs))
				for i, gid := range b.recentIDs {
					parts[i] = `<a href="/games/` + strconv.Itoa(gid) + `" style="font-size:.85em">[` + strconv.Itoa(gid) + `]</a>`
				}
				idLinks = " " + strings.Join(parts, " ")
			}
			fmt.Fprintf(w, `<div style="display:flex;align-items:center;gap:6px;margin:2px 0 2px 8px">`+
				`<div style="width:%.0fpx;height:14px;background:%s;border-radius:3px;min-width:2px;flex-shrink:0"></div>`+
				`<span style="font-size:1em;font-weight:600">%s%% %d/%d %s%s</span></div>`,
				bw, col, fmtPct(pct), b.count, b.games, game.ErrorLabel[b.ecode], idLinks)
		}
		io.WriteString(w, `</div>`)
	}
	io.WriteString(w, `</div>`)
}

// fmtPct formats a percentage with at most 2 significant digits and no trailing zeros.
// 98.9 → "99", 2.19 → "2.2", 2.0 → "2", 0.6 → "0.6".
func fmtPct(pct float64) string {
	if pct >= 10 {
		return fmt.Sprintf("%.0f", math.Round(pct))
	}
	if pct < 0.1 {
		return "<0.1"
	}
	s := fmt.Sprintf("%.1f", pct)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	return s
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
