package web

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"os"
	"syscall"
	"runtime"
	"strings"
	"time"

	"github.com/neoliv/arena/internal/db"
	"github.com/neoliv/arena/internal/stats"
	"github.com/neoliv/arena/internal/version"
)

var SharedCSS = sharedCSS

const sharedCSS = `<style>
:root{--bg:#fafafa;--fg:#222;--muted:#666;--border:#ddd;--hover:#f0f0f5;--th-bg:#f0f0f0;--link:#385;--link-hover:#263;--nav-hl:#1a5c3a;--bg2:#fff;--accent:var(--nav-hl);--win-bg:#dfd;--win-fg:#060;--loss-bg:#fdd;--loss-fg:#600;--draw-bg:#ffd;--draw-fg:#660;color-scheme:light}
@media(prefers-color-scheme:dark){:root{--bg:#1a1a2e;--fg:#e8e6e3;--muted:#a9a7a3;--border:#333;--hover:#252540;--th-bg:#22223a;--link:#7a7;--link-visited:#4a4;--link-hover:#9b9;--nav-hl:#284;--bg2:#252540;--accent:var(--nav-hl);--win-bg:#1a3a1a;--win-fg:#7f7;--loss-bg:#3a1a1a;--loss-fg:#f77;--draw-bg:#3a3a1a;--draw-fg:#ee7;color-scheme:dark}}
body{font-family:system-ui,sans-serif;max-width:960px;margin:0 auto;padding:1em;color:var(--fg);background:var(--bg)}
h1{font-size:1.4em;margin:0 0 .5em}
nav{margin-bottom:1.5em;border-bottom:1px solid var(--border);padding-bottom:.5em}
nav a{display:inline-block;margin-right:.3em;text-decoration:none;color:#e8e6e3;font-size:1.1em;font-weight:600;padding:.35em .7em;border-radius:5px;border:1px solid var(--nav-hl);background:rgba(56,136,85,0.06);transition:all .15s}
nav a:hover{background:var(--nav-hl);color:#fff;border-color:var(--nav-hl)}
nav a.logout:hover{background:#c33;border-color:#c33;color:#fff}
nav a.active,.chart-tabs a.active{background:var(--nav-hl);color:#fff}
	.chart-tab{transition:all .15s}.chart-tab:hover{background:var(--nav-hl)!important;color:#fff!important}
table{border-collapse:collapse;width:100%;margin-bottom:2em}
th,td{text-align:left;padding:.4em .6em;border-bottom:1px solid var(--border)}
th{font-weight:600;background:var(--th-bg);cursor:pointer;user-select:none;position:relative;padding-right:18px}th:hover{background:var(--hover)}td{white-space:nowrap}.sort-ind{position:absolute;right:4px;top:50%;transform:translateY(-50%);font-size:1.2em;font-weight:900;color:var(--fg)}
tr:hover{background:var(--hover)}
a{color:var(--link)}a:visited{color:var(--link-visited)}
.badge{padding:.1em .4em;border-radius:3px;font-size:.85em}
.win{background:var(--win-bg);color:var(--win-fg)}
.loss{background:var(--loss-bg);color:var(--loss-fg)}
.draw{background:var(--draw-bg);color:var(--draw-fg)}
.bar{display:inline-block;height:12px;background:var(--link);border-radius:3px}
input,select{background:var(--bg);color:var(--fg);border:1px solid var(--nav-hl);background:rgba(56,136,85,0.06);padding:.2em .5em;border-radius:4px}
#filterBox{width:100%;max-width:300px;padding:.4em .6em;margin-bottom:1em;font-size:1em;border:1px solid var(--muted);border-radius:4px;outline:none;transition:border-color .2s}#filterBox:focus{border-color:var(--link)}
.stats-table{width:auto;min-width:400px}.stats-table td:first-child{width:140px;font-weight:600;color:var(--muted)}tr.critical td:last-child{color:#f44336;font-weight:600}tr.warning td:last-child{color:#ff9800;font-weight:600}
.stats-table{width:auto;min-width:400px}.stats-table td:first-child{width:140px;font-weight:600;color:var(--muted)}tr.critical td:last-child{color:#f44336;font-weight:600}tr.warning td:last-child{color:#ff9800;font-weight:600}
</style>`

const searchJS = `<scr` + `ipt>
function filter(){let q=document.getElementById('filterBox').value.toLowerCase().trim();if(!q){document.querySelectorAll('tr.filter-row').forEach(r=>r.style.display='');document.querySelectorAll('.filter-item').forEach(r=>r.style.display='');return}
let words=q.split(/\s+/);document.querySelectorAll('tr.filter-row').forEach(r=>{let t=r.textContent.toLowerCase();r.style.display=words.every(function(w){return t.includes(w)})?'':'none'})
document.querySelectorAll('.filter-item').forEach(r=>{let t=r.textContent.toLowerCase();r.style.display=words.every(function(w){return t.includes(w)})?'':'none'})}
var sc=-1,sa=!0;
function st(t,c,n){var b=t.querySelector("tbody")||t,r=Array.from(b.querySelectorAll("tr.filter-row"));
if(c===sc)sa=!sa;else{sa=!0;sc=c}
r.sort(function(a,b){var va=a.cells[c].textContent.trim(),vb=b.cells[c].textContent.trim();
if(n){va=parseFloat(va)||0;vb=parseFloat(vb)||0}
return sa?va>vb?1:va<vb?-1:0:va<vb?1:va>vb?-1:0});
r.forEach(function(r){b.appendChild(r)});
t.querySelectorAll("th").forEach(function(t,i){var s=t.querySelector(".sort-ind");if(!s){s=document.createElement("span");s.className="sort-ind";t.appendChild(s)}s.textContent=i===c?(sa?"\u25b2":"\u25bc"):""})}
<` + `/script>`

const filterBox = `<input type="search" id="filterBox" placeholder="Filter…" oninput="filter()" autofocus>`

const navHTML = `<nav>
<a href="/">Ranks</a> <a href="/charts">Charts</a>
<a href="/matches">Matches</a> <a href="/games">Games</a> <a href="/players">Players</a> <a href="/coaches">Coaches</a>
<a href="/health">Health</a> <a href="/admin">Admin</a>
<span style="float:right"><a class="logout" href="/logout">Disconnect</a></span>
</nav>`

const htmxScript = `<script src="https://unpkg.com/htmx.org@2.0.4" integrity="sha384-HGxOGrUEVMQQBW1EE4IqOmxPxVJzZSoS0rIYgJOlhNYG8YP4iWm4kq6FDoGsEdJj" crossorigin="anonymous"></script>`
const pageHead = `<!DOCTYPE html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Othello Arena</title>` + sharedCSS + htmxScript + `</head><body>`
const pageFoot = `</body></html>`

// chartColors are chalk/pastel hues visible on both light and dark backgrounds.
var chartColors = [8]string{"#ff6b8a","#6bd4ff","#ffe66b","#6bff8a","#ff8a6b","#c46bff","#6bffe6","#ffb86b"}

type Handler struct {
	DB       *db.DB
	Token    string
	Sessions *SessionStore
	Limiter  *RateLimiter
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", h.handleLogin)
	mux.HandleFunc("POST /login", h.handleLogin)
	mux.HandleFunc("GET /logout", h.HandleLogout)
	mux.HandleFunc("GET /{$}", h.RequireLogin(h.handleRanks))
	// charts route handled below
	mux.HandleFunc("GET /graphs", h.RequireLogin(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/charts?tab="+r.URL.Query().Get("tab"), http.StatusMovedPermanently) }))
	mux.HandleFunc("GET /charts", h.RequireLogin(h.handleGraphs))
	mux.HandleFunc("GET /matches", h.RequireLogin(h.handleMatches))
	mux.HandleFunc("GET /matches/{id}", h.RequireLogin(h.handleMatch))
	mux.HandleFunc("GET /games", h.RequireLogin(h.handleGames))
		mux.HandleFunc("GET /games/{id}", h.RequireLogin(h.handleGameDetail))
	mux.HandleFunc("GET /engines/{name}", h.RequireLogin(h.handleEngine))
	mux.HandleFunc("GET /versions", h.RequireLogin(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/players", http.StatusMovedPermanently) }))
	mux.HandleFunc("GET /players", h.RequireLogin(h.handleVersions))
	mux.HandleFunc("GET /coaches", h.RequireLogin(h.handleCoaches))
	mux.HandleFunc("GET /health", h.RequireLogin(h.handleHealth))
	
	mux.HandleFunc("GET /admin", h.RequireLogin(h.handleAdmin))
	mux.HandleFunc("POST /admin", h.RequireLogin(h.handleAdminSave))
	mux.HandleFunc("GET /admin/suspend/{id}", h.RequireLogin(h.handleAdminSuspend))
	mux.HandleFunc("GET /admin/delete/{id}", h.RequireLogin(h.handleAdminDelete))
	mux.HandleFunc("POST /admin/new", h.RequireLogin(h.handleAdminNew))
	mux.HandleFunc("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(404)
		io.WriteString(w, pageHead+`<div style="text-align:center;margin-top:4em"><h1 style="font-size:3em;margin:0">404</h1><p style="font-size:1.2em;color:var(--muted)">This square is off the board.</p><p><a href="/" style="color:var(--link)">Return to the board</a></p></div>`+pageFoot)
	}))
}

func (h *Handler) handleRanks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, pageHead+navHTML+`<div hx-get="." hx-trigger="every 30s" hx-swap="outerHTML">`+searchJS+`<h1>Player Rankings</h1>`+filterBox+`<table><tr><th onclick="st(this.closest('table'),0,true)">#</th><th onclick="st(this.closest('table'),1,false)">Player</th><th onclick="st(this.closest('table'),2,true)">Elo</th><th onclick="st(this.closest('table'),3,true)">+/-</th><th onclick="st(this.closest('table'),4,true)">Games</th><th onclick="st(this.closest('table'),5,false)">W/L/D</th><th onclick="st(this.closest('table'),6,false)">Trend</th></tr>`)
	rows, err := h.DB.Query(`SELECT e.id, e.name, e.version, COALESCE(e.engine_id,''), COALESCE((SELECT rating_after FROM elo_history WHERE engine_id=e.id ORDER BY created_at DESC LIMIT 1), 1500.0), (SELECT COUNT(*) FROM games WHERE black_id=e.id OR white_id=e.id) as g, (SELECT COUNT(*) FROM games WHERE (black_id=e.id AND result='1-0') OR (white_id=e.id AND result='0-1')), (SELECT COUNT(*) FROM games WHERE (black_id=e.id AND result='0-1') OR (white_id=e.id AND result='1-0')), (SELECT COUNT(*) FROM games WHERE (black_id=e.id OR white_id=e.id) AND result='1/2') FROM engines e ORDER BY 4 DESC`)
	if err != nil || rows == nil { io.WriteString(w, "</table>"); return }
	defer rows.Close()
	type eng struct{ Name, Version, EngineID string; ID int; Elo float64; Games, W, L, D int }
	var engines []eng
	for rows.Next() {
		var e eng; rows.Scan(&e.ID, &e.Name, &e.Version, &e.EngineID, &e.Elo, &e.Games, &e.W, &e.L, &e.D)
		engines = append(engines, e)
	}
	for i, e := range engines {
		ci := 400.0 / math.Sqrt(math.Max(float64(e.Games), 1))
		trend := "—"
		if e.Games >= 10 {
			var oldElo float64
			h.DB.QueryRow(`SELECT rating_before FROM elo_history WHERE engine_id=? ORDER BY created_at ASC LIMIT 1`, e.ID).Scan(&oldElo)
			if oldElo > 0 { if e.Elo > oldElo+10 { trend = "↑" } else if e.Elo < oldElo-10 { trend = "↓" } else { trend = "→" } }
		}
		var wr string
		if e.Games > 0 { wr = fmt.Sprintf("%d/%d/%d", e.W, e.L, e.D) } else { wr = "—" }
		fmt.Fprintf(w, `<tr class="filter-row"><td>%d</td><td><a href="/engines/%s">%s %s</a> <small style="color:var(--muted)">%s</small></td><td>%.0f</td><td>±%.0f</td><td>%d</td><td>%s</td><td>%s</td></tr>`, i+1, e.Name, e.Name, e.Version, e.EngineID[:min(8,len(e.EngineID))], e.Elo, ci, e.Games, wr, trend)
	}
	io.WriteString(w, "</table>"+`</div>`+pageFoot)
}

func (h *Handler) handleGraphs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tab := r.URL.Query().Get("tab")
	io.WriteString(w, pageHead+navHTML+searchJS+`<h1>Graphs</h1>`+filterBox+`
		<nav class="chart-tabs" style="margin-bottom:1.5em">
		<a href="?tab=elo" class="chart-tab" style="display:inline-block;padding:.35em .7em;border-radius:5px;font-size:1.1em;font-weight:600;text-decoration:none;border:1px solid var(--nav-hl);color:`+func()string{if tab==""||tab=="elo"{return"#fff"}else{return"var(--fg)"}}()+`;background:`+func()string{if tab==""||tab=="elo"{return"var(--nav-hl)"}else{return"rgba(56,136,85,0.06)"}}()+`">Elo</a>
		<a href="?tab=speed" class="chart-tab" style="display:inline-block;padding:.35em .7em;border-radius:5px;font-size:1.1em;font-weight:600;text-decoration:none;border:1px solid var(--nav-hl);color:`+func()string{if tab=="speed"{return"#fff"}else{return"var(--fg)"}}()+`;background:`+func()string{if tab=="speed"{return"var(--nav-hl)"}else{return"rgba(56,136,85,0.06)"}}()+`">Speed</a>
		<a href="?tab=games" class="chart-tab" style="display:inline-block;padding:.35em .7em;border-radius:5px;font-size:1.1em;font-weight:600;text-decoration:none;border:1px solid var(--nav-hl);color:`+func()string{if tab=="games"{return"#fff"}else{return"var(--fg)"}}()+`;background:`+func()string{if tab=="games"{return"var(--nav-hl)"}else{return"rgba(56,136,85,0.06)"}}()+`">Games</a>
		<a href="?tab=timeout" class="chart-tab" style="display:inline-block;padding:.35em .7em;border-radius:5px;font-size:1.1em;font-weight:600;text-decoration:none;border:1px solid var(--nav-hl);color:`+func()string{if tab=="timeout"{return"#fff"}else{return"var(--fg)"}}()+`;background:`+func()string{if tab=="timeout"{return"var(--nav-hl)"}else{return"rgba(56,136,85,0.06)"}}()+`">Timeouts</a>
		<a href="?tab=performance" style="display:inline-block;border-radius:5px;font-size:1.1em;font-weight:600;text-decoration:none;border:1px solid var(--border);color:`+func()string{if tab=="performance"{return"#fff"}else{return"var(--fg)"}}()+`;background:`+func()string{if tab=="performance"{return"var(--nav-hl)"}else{return"rgba(56,136,85,0.06)"}}()+`">Performance</a>
		</div>`)

	switch tab {
	case "speed":
		h.renderSpeedGraph(w, r)
	case "games":
		h.renderStatsBars(w, r, "games")
	case "timeout":
		h.renderStatsBars(w, r, "timeout")
	case "performance":
		h.renderStatsBars(w, r, "length")
		io.WriteString(w, "<br>")
		h.renderStatsBars(w, r, "unspent")
	default:
		h.renderEloChart(w, r)
	}
	io.WriteString(w, pageFoot)
}

func (h *Handler) renderEloChart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, searchJS+filterBox)
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

	// Build engine data for SVG polylines
	type ep struct{ x, y float64 }
	engineData := make([][]ep, len(engineNames))
	totalPts := 0
	for _, p := range points { for _, e := range engineNames { if p.Engine == e { totalPts++ } } }
	for _, p := range points {
		idx := engineIdx[p.Engine]
		_, _ = parseTime(p.Date)
		x := 0.0
		if totalPts > 1 {
			x = float64(len(engineData[idx])) / float64(max(totalPts/len(engineNames), 1)) * graphw
			if x > graphw { x = graphw }
		}
		y := graphh - (p.Elo-minElo)/(maxElo-minElo)*graphh
		engineData[idx] = append(engineData[idx], ep{x: x, y: y})
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
		fmt.Fprintf(w, `<polyline class="filter-item" fill="none" stroke="%s" stroke-width="2" points="%s"/>`, chartColors[i%8], strings.TrimSpace(pts))
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
	io.WriteString(w, `<form><select name="id" onchange="location=this.value"><option value="?tab=speed">Select engine…</option>`)
	rows, _ := h.DB.Query("SELECT id, name, version FROM engines ORDER BY name")
	if rows != nil { defer rows.Close(); for rows.Next() { var id int; var n, v string; rows.Scan(&id, &n, &v); fmt.Fprintf(w, `<option value="?tab=speed&id=%d">%s %s</option>`, id, n, v) } }
	io.WriteString(w, `</select></form>`)
	engID := r.URL.Query().Get("id")
	if engID == "" { io.WriteString(w, pageFoot); return }
	io.WriteString(w, `<table><tr><th>Ply</th><th>#</th><th>Depth</th><th>NPS</th><th>Timeouts</th><th></th></tr>`)
	sRows, _ := h.DB.Query(`SELECT ply, SUM(sample_count), CAST(SUM(total_depth) AS REAL)/MAX(1,SUM(sample_count)), CAST(SUM(total_nps) AS REAL)/MAX(1,SUM(sample_count)), SUM(timeouts) FROM speed_stats WHERE engine_id=? GROUP BY ply ORDER BY ply`, engID)
	if sRows != nil { defer sRows.Close(); for sRows.Next() { var ply, samples, timeouts int; var depth, nps float64; sRows.Scan(&ply, &samples, &depth, &nps, &timeouts); fmt.Fprintf(w, `<tr class="filter-row"><td>%d</td><td>%d</td><td>%.1f</td><td>%.0f</td><td>%d</td><td><span class="bar" style="width:%dpx"></span></td></tr>`, ply, samples, depth, nps, timeouts, int(nps/1000)) } }
	io.WriteString(w, "</table>")
}

func (h *Handler) handleMatches(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, pageHead+navHTML+`<div hx-get="." hx-trigger="every 30s" hx-swap="outerHTML">`+searchJS+`<h1>Matches</h1>`+filterBox)

	var inProgressCount int
	h.DB.QueryRow("SELECT COUNT(*) FROM match_assignments WHERE status='in_progress'").Scan(&inProgressCount)
	fmt.Fprintf(w, `<h2>In Progress %d</h2><table><tr><th onclick="st(this.closest('table'),0,true)">ID</th><th onclick="st(this.closest('table'),1,false)">Black</th><th onclick="st(this.closest('table'),2,false)">White</th><th onclick="st(this.closest('table'),3,false)">Time</th><th onclick="st(this.closest('table'),4,true)">Games</th><th onclick="st(this.closest('table'),5,false)">Started</th></tr>`, inProgressCount)
	aRows, _ := h.DB.Query(`SELECT a.id, (SELECT name||' '||version FROM engines WHERE id=a.engine1_id), (SELECT name||' '||version FROM engines WHERE id=a.engine2_id), COALESCE(a.time_control,'{}'), a.num_games, COALESCE(a.in_progress_at, a.created_at) FROM match_assignments a WHERE a.status='in_progress' ORDER BY a.id DESC LIMIT 20`)
	if aRows != nil { defer aRows.Close(); for aRows.Next() { var id, games int; var e1, e2, tc, started string; aRows.Scan(&id, &e1, &e2, &tc, &games, &started); tcDisplay := formatTimeControl(tc)
			if t, err := time.Parse(time.RFC3339, started); err == nil {
				elapsed := time.Since(t).Round(time.Second)
				tcDisplay = fmt.Sprintf("%s / %s", elapsed, tcDisplay)
			} else if t, err := time.Parse("2006-01-02 15:04:05", strings.TrimSpace(started)); err == nil {
				elapsed := time.Since(t).Round(time.Second)
				tcDisplay = fmt.Sprintf("%s / %s", elapsed, tcDisplay)
			}; startDisplay := started[:min(19, len(started))]; if t, err := time.Parse(time.RFC3339, started); err == nil { startDisplay = niceDuration(t) } else if t, err := time.Parse("2006-01-02 15:04:05", started[:19]); err == nil { startDisplay = niceDuration(t) }; fmt.Fprintf(w, `<tr class="filter-row"><td>%d</td><td>%s</td><td>%s</td><td>%s</td><td>%d</td><td>%s</td></tr>`, id, e1, e2, tcDisplay, games, startDisplay) } }
	io.WriteString(w, "</table>")

	var completedCount int
	h.DB.QueryRow("SELECT COUNT(*) FROM matches").Scan(&completedCount)
	fmt.Fprintf(w, `<h2>Completed %d</h2><table><tr><th>ID</th><th>Black</th><th>White</th><th>Score</th><th>Games</th><th>Date</th></tr>`, completedCount)
	rows, _ := h.DB.Query(`SELECT m.id, (SELECT name||' '||version FROM engines WHERE id=m.engine1_id), (SELECT name||' '||version FROM engines WHERE id=m.engine2_id), m.wins_1, m.wins_2, m.draws, m.total_games, COALESCE(m.created_at,'') FROM matches m ORDER BY m.id DESC LIMIT 100`)
	if rows != nil { defer rows.Close(); for rows.Next() { var id, w1, w2, d, t int; var e1, e2, created string; rows.Scan(&id, &e1, &e2, &w1, &w2, &d, &t, &created); fmt.Fprintf(w, `<tr class="filter-row"><td><a href="/matches/%d">%d</a></td><td>%s</td><td>%s</td><td>%d-%d-%d</td><td>%d</td><td>%s</td></tr>`, id, id, e1, e2, w1, w2, d, t, created[:min(10,len(created))]) } }
	io.WriteString(w, "</table>")
}

func (h *Handler) handleMatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, pageHead+navHTML)
	fmt.Fprintf(w, "<h1>Match #%s</h1><table><tr><th>#</th><th>Black</th><th>White</th><th>Result</th><th>Score</th><th>Opening</th></tr>", id)
	rows, _ := h.DB.Query(`SELECT g.game_number, (SELECT name||' '||version FROM engines WHERE id=g.black_id), (SELECT name||' '||version FROM engines WHERE id=g.white_id), g.result, COALESCE(g.final_score,0), COALESCE(g.opening_line,'') FROM games g WHERE g.match_id=? ORDER BY g.game_number`, id)
	if rows != nil { defer rows.Close(); for rows.Next() { var num, s int; var blk, wht, r, o string; rows.Scan(&num, &blk, &wht, &r, &s, &o); badge := ""; if r == "1-0" { badge = `<span class="badge win">W</span>` } else if r == "0-1" { badge = `<span class="badge loss">L</span>` } else { badge = `<span class="badge draw">D</span>` }; fmt.Fprintf(w, `<tr><td><a href="/games/%d">%d</a></td><td>%s</td><td>%s</td><td>%s %s</td><td>%+d</td><td>%s</td></tr>`, num, num, blk, wht, r, badge, s, o) } }
	io.WriteString(w, "</table>")
}

func (h *Handler) handleGames(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, pageHead+navHTML+searchJS+`<h1>Games</h1>`+filterBox)

	var inProgressCount int
	h.DB.QueryRow("SELECT COUNT(*) FROM match_assignments WHERE status='in_progress'").Scan(&inProgressCount)
	fmt.Fprintf(w, `<h2>In Progress %d</h2><table><tr><th onclick="st(this.closest('table'),0,true)">ID</th><th onclick="st(this.closest('table'),1,false)">Black</th><th onclick="st(this.closest('table'),2,false)">White</th><th onclick="st(this.closest('table'),3,false)">Time</th><th onclick="st(this.closest('table'),4,true)">Games</th><th onclick="st(this.closest('table'),5,false)">Started</th></tr>`, inProgressCount)
	iRows, _ := h.DB.Query(`SELECT a.id, (SELECT name||' '||version FROM engines WHERE id=a.engine1_id), (SELECT name||' '||version FROM engines WHERE id=a.engine2_id), COALESCE(a.time_control,'{}'), a.num_games, COALESCE(a.in_progress_at, a.created_at) FROM match_assignments a WHERE a.status='in_progress' ORDER BY a.id DESC LIMIT 20`)
	if iRows != nil {
		defer iRows.Close()
		for iRows.Next() {
			var id, games int; var e1, e2, tc, started string
			iRows.Scan(&id, &e1, &e2, &tc, &games, &started)
			tcDisplay := formatTimeControl(tc)
			if t, err := time.Parse(time.RFC3339, started); err == nil {
				elapsed := time.Since(t).Round(time.Second)
				tcDisplay = fmt.Sprintf("%s / %s", elapsed, tcDisplay)
			} else if t, err := time.Parse("2006-01-02 15:04:05", strings.TrimSpace(started)); err == nil {
				elapsed := time.Since(t).Round(time.Second)
				tcDisplay = fmt.Sprintf("%s / %s", elapsed, tcDisplay)
			}
			startedDisplay := started[:min(19, len(started))]
			if t, err := time.Parse(time.RFC3339, started); err == nil {
				startedDisplay = niceDuration(t)
			} else if t, err := time.Parse("2006-01-02 15:04:05", started[:19]); err == nil {
				startedDisplay = niceDuration(t)
			}
			fmt.Fprintf(w, `<tr class="filter-row"><td>%d</td><td>%s</td><td>%s</td><td>%s</td><td>%d</td><td>%s</td></tr>`, id, e1, e2, tcDisplay, games, startedDisplay)
		}
	}
	if iRows == nil { io.WriteString(w, `<tr><td colspan="6">None</td></tr>`) }
	io.WriteString(w, "</table>")

	var completedCount int
	h.DB.QueryRow("SELECT COUNT(*) FROM games").Scan(&completedCount)
	fmt.Fprintf(w, `<h2>Completed %d</h2><table><tr><th onclick="st(this.closest('table'),0,true)">ID</th><th onclick="st(this.closest('table'),1,false)">Black</th><th onclick="st(this.closest('table'),2,false)">White</th><th onclick="st(this.closest('table'),3,false)">Result</th><th onclick="st(this.closest('table'),4,true)">Score</th><th onclick="st(this.closest('table'),5,false)">Opening</th></tr>`, completedCount)
	gRows, _ := h.DB.Query(`SELECT g.id, (SELECT name||' '||version FROM engines WHERE id=g.black_id), (SELECT name||' '||version FROM engines WHERE id=g.white_id), g.result, COALESCE(g.final_score,0), COALESCE(g.opening_line,'') FROM games g ORDER BY g.id DESC LIMIT 100`)
	if gRows != nil { defer gRows.Close(); for gRows.Next() { var id, s int; var blk, wht, r, o string; gRows.Scan(&id, &blk, &wht, &r, &s, &o); fmt.Fprintf(w, `<tr class="filter-row"><td><a href="/games/%d">%d</a></td><td>%s</td><td>%s</td><td>%s</td><td>%+d</td><td>%s</td></tr>`, id, id, blk, wht, r, s, o) } }
	io.WriteString(w, "</table>")
}

func (h *Handler) handleGameDetail(w http.ResponseWriter, r *http.Request) {
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

		bNPS := float64(blackNodes) / max(bTime, 0.001)
		wNPS := float64(whiteNodes) / max(wTime, 0.001)

		fmt.Fprintf(w, `<table class="stats-table">`)
		fmt.Fprintf(w, `<tr><td>Match</td><td><a href="/matches/%d">#%d</a> (game %d)</td></tr>`, mid, mid, gnum)
		fmt.Fprintf(w, `<tr><td>Black</td><td><a href="/engines/%s">%s %s</a> <small style="color:var(--muted)">(%.0f)</small></td></tr>`, bName, bName, bVer, bElo)
		fmt.Fprintf(w, `<tr><td>White</td><td><a href="/engines/%s">%s %s</a> <small style="color:var(--muted)">(%.0f)</small></td></tr>`, wName, wName, wVer, wElo)
		fmt.Fprintf(w, `<tr><td>Result</td><td>%s %s</td></tr>`, result, badge)
		fmt.Fprintf(w, `<tr><td>Score</td><td>%+d</td></tr>`, finalScore)
		if opening != "" { fmt.Fprintf(w, `<tr><td>Opening</td><td>%s</td></tr>`, opening) }
		fmt.Fprintf(w, `<tr><td>Black NPS</td><td>%.0f / depth %d</td></tr>`, bNPS, blackDepth)
		fmt.Fprintf(w, `<tr><td>White NPS</td><td>%.0f / depth %d</td></tr></table>`, wNPS, whiteDepth)

		// Per-move data
		mRows, _ := h.DB.Query("SELECT move_num, side, move, nodes, depth, time_ms, nps FROM game_moves WHERE game_id=? ORDER BY move_num", gid)
		if mRows != nil {
			defer mRows.Close()
			type moveRow struct{ num int; side, move string; nodes int; depth int; timeMs float64; nps int }
			var moves []moveRow
			maxTime, maxNodes, maxNPS := 0.0, 0.0, 0.0
			for mRows.Next() {
				var m moveRow
				mRows.Scan(&m.num, &m.side, &m.move, &m.nodes, &m.depth, &m.timeMs, &m.nps)
				moves = append(moves, m)
				if m.timeMs > maxTime { maxTime = m.timeMs }
				if float64(m.nodes) > maxNodes { maxNodes = float64(m.nodes) }
				if float64(m.nps) > maxNPS { maxNPS = float64(m.nps) }
			}

			// Move transcript at top
			if len(moves) > 0 {
				tab := r.URL.Query().Get("tab")
				chartH := 140
				chartW := fmt.Sprintf("%d", max(600, len(moves)*8))
				io.WriteString(w, `<table><tr><th>#</th><th>Side</th><th>Move</th><th>Time</th><th>Nodes</th><th>Depth</th><th>NPS</th></tr>`)
				for _, m := range moves {
					side := "Black"
					if m.side == "w" { side = "White" }
					fmt.Fprintf(w, `<tr class="filter-row"><td>%d</td><td>%s</td><td>%s</td><td>%.1fms</td><td>%d</td><td>%d</td><td>%d</td></tr>`,
						m.num, side, m.move, m.timeMs, m.nodes, m.depth, m.nps)
				}
				io.WriteString(w, "</table>")

				// Chart tabs
				io.WriteString(w, `<nav class="chart-tabs" style="margin-top:1em;margin-bottom:1em">`)
				for _, t := range []struct{ key, label string }{ {"time","Time"}, {"nodes","Nodes"}, {"nps","NpS"} } {
					sel := `style="display:inline-block;padding:.35em .7em;border-radius:5px;font-size:1.1em;font-weight:600;text-decoration:none;border:1px solid var(--nav-hl);color:#fff;background:var(--nav-hl)"`
					if tab != t.key { sel = `style="display:inline-block;padding:.35em .7em;border-radius:5px;font-size:1.1em;font-weight:600;text-decoration:none;border:1px solid var(--border);color:var(--fg);background:rgba(56,136,85,0.06)"` }
					fmt.Fprintf(w, `<a href="?tab=%s" %s>%s</a>`, t.key, sel, t.label)
				}
				io.WriteString(w, `</nav>`)

				// Render chart based on selected tab
				renderChart := func(metric string, maxVal float64, unit string, yLabel string) {
					if tab == "" { tab = "time" }
					if metric != tab { return }
					io.WriteString(w, fmt.Sprintf(`<div style="background:#1a3a1a;border:1px solid #2a4a2a;border-radius:6px;padding:12px 8px 24px 8px;position:relative;overflow-x:auto;width:%s">`, chartW))
					io.WriteString(w, fmt.Sprintf(`<svg width="%s" height="%d">`, chartW, chartH+30))
					// Y-axis labels
					for pct := 0; pct <= 100; pct += 25 {
						y := chartH - pct*chartH/100 + 4
						val := maxVal * float64(pct) / 100.0
						var label string
						if val >= 1000 { label = fmt.Sprintf("%.0fk", val/1000) } else { label = fmt.Sprintf("%.0f", val) }
						fmt.Fprintf(w, `<text x="0" y="%d" fill="#6a6" font-size="9">%s %s</text>`, y, label, unit)
						fmt.Fprintf(w, `<line x1="30" y1="%d" x2="100%%" y2="%d" stroke="#2a4a2a" stroke-width="0.5"/>`, chartH-pct*chartH/100, chartH-pct*chartH/100)
					}
					// X-axis label
					fmt.Fprintf(w, `<text x="50%%" y="%d" text-anchor="middle" fill="#6a6" font-size="10">%s</text>`, chartH+20, yLabel)
					// Bars with parity handling
					for i, m := range moves {
						var val float64
						switch metric {
						case "time": val = m.timeMs
						case "nodes": val = float64(m.nodes)
						case "nps": val = float64(m.nps)
						}
						h := 0
						if maxVal > 0 { h = int(val / maxVal * float64(chartH)) }
						if h < 1 { h = 1 }
						color := "#2c5a2c"
						if m.side == "w" { color = "#eee" }
						// Parity check: if previous move was same side, skip label
						showLabel := true
						if i > 0 && moves[i-1].side == m.side { showLabel = false }
						x := 32 + i*6
						fmt.Fprintf(w, `<rect x="%d" y="%d" width="5" height="%d" fill="%s" rx="1"><title>%s %s: %.0f%s %d nodes</title></rect>`, x, chartH-h, h, color, m.side, m.move, val, unit, m.nodes)
						if showLabel {
							fmt.Fprintf(w, `<text x="%d" y="%d" fill="%s" font-size="7" text-anchor="middle">%s</text>`, x+2, chartH+14, color, m.move)
						}
					}
					io.WriteString(w, `</svg></div>`)
				}
				renderChart("time", maxTime, "ms", "Time per move (ms)")
				renderChart("nodes", maxNodes, "kn", "Nodes explored")
				renderChart("nps", maxNPS, "kn/s", "Nodes per second")

				// Full detailed table at bottom
				io.WriteString(w, `<table style="margin-top:1.5em"><tr><th>#</th><th>Side</th><th>Move</th><th>Time</th><th>Nodes</th><th>Depth</th><th>NPS</th></tr>`)
				for _, m := range moves {
					side := "Black"
					if m.side == "w" { side = "White" }
					fmt.Fprintf(w, `<tr class="filter-row"><td>%d</td><td>%s</td><td>%s</td><td>%.1fms</td><td>%d</td><td>%d</td><td>%d</td></tr>`,
						m.num, side, m.move, m.timeMs, m.nodes, m.depth, m.nps)
				}
				io.WriteString(w, "</table>")
			}
		} else {
			io.WriteString(w, "<p style=\"color:var(--muted);font-style:italic\">No per-move data — engines may not support move stats.</p>")
		}
		io.WriteString(w, pageFoot)
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


func (h *Handler) handleEngine(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, pageHead+navHTML)
	fmt.Fprintf(w, "<h1>%s</h1>", htmlEscape(name))
	// Show manifest for the latest version
	var manifest string
	h.DB.QueryRow("SELECT COALESCE(engine_manifest,'') FROM engines WHERE name=? ORDER BY created_at DESC LIMIT 1", name).Scan(&manifest)
	if manifest != "" {
		fmt.Fprintf(w, "<pre style=\"background:var(--th-bg);padding:1em;border-radius:4px;font-size:.85em;max-height:400px;overflow:auto\">%s</pre>", htmlEscape(manifest))
	}
	var spark string
	eloRows, _ := h.DB.Query(`SELECT eh.rating_after FROM elo_history eh JOIN engines e2 ON eh.engine_id=e2.id WHERE e2.name=? ORDER BY eh.created_at`, name)
	if eloRows != nil { defer eloRows.Close(); var vals []float64; min, max := 4000.0, 0.0; for eloRows.Next() { var v float64; eloRows.Scan(&v); vals = append(vals, v); if v < min { min = v }; if v > max { max = v } }; if len(vals) > 0 && max > min { chars := "▁▂▃▄▅▆▇█"; for _, v := range vals { idx := int((v-min)/(max-min)*float64(len(chars)-1)); if idx < 0 { idx = 0 }; if idx >= len(chars) { idx = len(chars)-1 }; spark += string(chars[idx]) }; spark += fmt.Sprintf("  %.0f … %.0f", min, max) } }
	fmt.Fprintf(w, "<pre>%s</pre><h3>Recent Games</h3><table><tr><th>#</th><th>Opponent</th><th>Result</th><th>Score</th></tr>", spark)
	gameRows, _ := h.DB.Query(`SELECT g.id, CASE WHEN g.black_id IN (SELECT id FROM engines WHERE name=?) THEN ew.name ELSE eb.name END, CASE WHEN g.black_id IN (SELECT id FROM engines WHERE name=?) THEN g.result ELSE CASE g.result WHEN '1-0' THEN '0-1' WHEN '0-1' THEN '1-0' ELSE g.result END END, COALESCE(g.final_score,0) FROM games g JOIN engines eb ON g.black_id=eb.id JOIN engines ew ON g.white_id=ew.id WHERE eb.name=? OR ew.name=? ORDER BY g.id DESC LIMIT 30`, name, name, name, name)
	if gameRows != nil { defer gameRows.Close(); for gameRows.Next() { var id, s int; var opp, r string; if gameRows.Scan(&id, &opp, &r, &s) == nil { fmt.Fprintf(w, `<tr class="filter-row"><td><a href="/games/%d">%d</a></td><td>%s</td><td>%s</td><td>%+d</td></tr>`, id, id, opp, r, s) } } }
	io.WriteString(w, "</table>")
}

func (h *Handler) handleVersions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, pageHead+navHTML+searchJS+`<h1>All Players</h1><p>An <strong>engine</strong> is a software build identified by content hash. A <strong>player</strong> is an engine with runtime arguments. Stats are tracked per time control. Click a column header to sort.</p>`+filterBox)
	type ver struct {
		Name, Version, Created, ChangelogShort, ChangelogFull, Budget, WR string
		Elo float64; Games, Wins, Losses, Draws int
	}
	rows, _ := h.DB.Query(`SELECT e.name, e.version, COALESCE(e.created, e.created_at), COALESCE(e.changelog_short,''), COALESCE(e.changelog_full,''), CASE WHEN e.version LIKE '%-%s' THEN SUBSTR(e.version, LENGTH(e.version)-2) ELSE '-' END as budget, COALESCE((SELECT rating_after FROM elo_history WHERE engine_id=e.id ORDER BY created_at DESC LIMIT 1), 1500.0), (SELECT COUNT(*) FROM games WHERE black_id=e.id OR white_id=e.id), (SELECT COUNT(*) FROM games WHERE (black_id=e.id AND result='1-0') OR (white_id=e.id AND result='0-1')), (SELECT COUNT(*) FROM games WHERE (black_id=e.id AND result='0-1') OR (white_id=e.id AND result='1-0')), (SELECT COUNT(*) FROM games WHERE (black_id=e.id OR white_id=e.id) AND result='1/2') FROM engines e ORDER BY COALESCE(e.created, e.created_at) DESC`)
	if rows == nil { io.WriteString(w, pageFoot); return }
	defer rows.Close()
	var versions []ver
	for rows.Next() {
		var v ver
		if rows.Scan(&v.Name, &v.Version, &v.Created, &v.ChangelogShort, &v.ChangelogFull, &v.Budget, &v.Elo, &v.Games, &v.Wins, &v.Losses, &v.Draws) != nil { continue }
		if v.Created == "" { v.Created = "unknown" }
		if v.Games > 0 { v.WR = fmt.Sprintf("%.1f%%", float64(v.Wins)/float64(v.Games)*100) } else { v.WR = "—" }
		versions = append(versions, v)
	}
	if len(versions) == 0 { io.WriteString(w, "<p>No players registered yet.</p>"+pageFoot); return }
	io.WriteString(w, `<table><tr><th onclick="st(this.parentElement.parentElement,0,false)" style="cursor:pointer">Engine</th><th onclick="st(this.parentElement.parentElement,1,false)" style="cursor:pointer">Version</th><th onclick="st(this.parentElement.parentElement,2,false)" style="cursor:pointer">Created</th><th onclick="st(this.parentElement.parentElement,3,false)" style="cursor:pointer">Budget</th><th onclick="st(this.parentElement.parentElement,4,true)" style="cursor:pointer">Elo</th><th onclick="st(this.parentElement.parentElement,5,true)" style="cursor:pointer">Games</th><th onclick="st(this.parentElement.parentElement,6,true)" style="cursor:pointer">W/L/D</th><th>Changes</th></tr>`)
	for i, v := range versions {
		shortID := fmt.Sprintf("cl-%d", i)
		fullID := fmt.Sprintf("fl-%d", i)
		changeCell := "—"
		if v.ChangelogShort != "" {
			changeCell = fmt.Sprintf(`<span id="%s">%s <a href="#" onclick="document.getElementById('%s').style.display='none';document.getElementById('%s').style.display='block';return false">[more]</a></span><span id="%s" style="display:none">%s <a href="#" onclick="document.getElementById('%s').style.display='block';document.getElementById('%s').style.display='none';return false">[less]</a></span>`, shortID, htmlEscape(v.ChangelogShort), shortID, fullID, fullID, htmlEscape(v.ChangelogFull), shortID, fullID)
		}
		fmt.Fprintf(w, `<tr class="filter-row"><td><a href="/engines/%s">%s</a></td><td>%s</td><td>%s</td><td>%s</td><td>%.0f</td><td>%d</td><td>%d/%d/%d</td><td>%s</td></tr>`, v.Name, v.Name, v.Version, v.Created, v.Budget, v.Elo, v.Games, v.Wins, v.Losses, v.Draws, changeCell)
	}
	io.WriteString(w, "</table>")
}

func (h *Handler) handleCoaches(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, pageHead+navHTML+`<h1>Coach Resources</h1>`)
	rows, err := h.DB.Query(`SELECT c.coach_id, c.label, COALESCE(c.version,''), c.cores_total, c.memory_mb_total,
		COALESCE((SELECT SUM(ca.cores_per_instance * ca.instances_running) FROM coach_ais ca WHERE ca.coach_id=c.id), 0),
		COALESCE((SELECT SUM(ca.memory_mb_per_instance * ca.instances_running) FROM coach_ais ca WHERE ca.coach_id=c.id), 0),
		c.last_seen
		FROM coaches c WHERE c.last_seen >= datetime('now','-90 seconds')
		ORDER BY c.coach_id`)
	if err != nil || rows == nil { io.WriteString(w, "<p>No coaches online.</p>"+pageFoot); return }
	defer rows.Close()
	type coachInfo struct{ ID, Label, Version string; CoresTotal, MemTotal, CoresUsed, MemUsed int; LastSeen string }
	var coaches []coachInfo
	totalCores, totalMem, usedCores, usedMem := 0, 0, 0, 0
	for rows.Next() {
		var c coachInfo
		rows.Scan(&c.ID, &c.Label, &c.Version, &c.CoresTotal, &c.MemTotal, &c.CoresUsed, &c.MemUsed, &c.LastSeen)
		coaches = append(coaches, c)
		totalCores += c.CoresTotal; totalMem += c.MemTotal
		usedCores += c.CoresUsed; usedMem += c.MemUsed
	}
	if len(coaches) == 0 { io.WriteString(w, "<p>No coaches online.</p>"+pageFoot); return }

	bar := func(used, total int) string {
		if total == 0 { return "0" }
		pct := used * 100 / total
		color := "#4caf50"; if pct > 80 { color = "#f44336" } else if pct > 60 { color = "#ff9800" }
		return fmt.Sprintf(`<div style="background:var(--border);border-radius:4px;height:16px;width:200px"><div style="background:%s;height:100%%;width:%d%%;border-radius:4px"></div></div><small>%d/%d (%d%%)</small>`, color, pct, used, total, pct)
	}
	ramBar := func(used, total int) string {
			if total == 0 { return "0" }
			pct := used * 100 / total
			color := "#4caf50"; if pct > 80 { color = "#f44336" } else if pct > 60 { color = "#ff9800" }
			return fmt.Sprintf(`<div style="background:var(--border);border-radius:4px;height:16px;width:200px"><div style="background:%s;height:100%%;width:%d%%;border-radius:4px"></div></div><small>%d%% (%s / %s)</small>`, color, pct, pct, niceSize(int64(used*1024*1024)), niceSize(int64(total*1024*1024)))
		}
		io.WriteString(w, `<h2>Total</h2><table><tr><th>CPU</th><td>`+bar(usedCores, totalCores)+`</td></tr><tr><th>RAM</th><td>`+ramBar(usedMem, totalMem)+`</td></tr></table>`)
	io.WriteString(w, `<h2>Per Coach</h2><table><tr><th>Coach</th><th>Version</th><th>Label</th><th>CPU (used/total)</th><th>RAM (used/total)</th><th>Last Seen</th></tr>`)
	for _, c := range coaches {
		lastSeen := c.LastSeen
		if t, err := time.Parse(time.RFC3339, c.LastSeen); err == nil {
			lastSeen = niceDuration(t)
		}
		fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>",
			c.ID, c.Version, c.Label,
			bar(c.CoresUsed, c.CoresTotal),
			ramBar(c.MemUsed, c.MemTotal),
			lastSeen)
		}
	io.WriteString(w, "</table>")
}

var arenaStart = time.Now()

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, pageHead+navHTML+`<h1>Health</h1>`)

	// ── Arena Service ──
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	memPct := 0
	if m.Sys > 0 {
		var totalMem uint64
		if d, err := os.ReadFile("/proc/meminfo"); err == nil {
			for _, line := range strings.Split(string(d), "\n") {
				if strings.HasPrefix(line, "MemTotal:") {
					var kb uint64; fmt.Sscanf(strings.Fields(line)[1], "%d", &kb)
					totalMem = kb * 1024
					break
				}
			}
		}
		if totalMem > 0 { memPct = int(float64(m.Sys) / float64(totalMem) * 100) }
	}
	arenaDur := niceDuration(arenaStart)
	startStr := arenaStart.Format("2006-01-02 15:04")
	io.WriteString(w, `<h2>Arena Service</h2><table class="stats-table">`)
	kv := func(k, v, cls string) { fmt.Fprintf(w, `<tr class="%s"><td>%s</td><td>%s</td></tr>`, cls, k, v) }
	kv("Version", version.Version, "")
	kv("Online", arenaDur+" ("+startStr+")", "")
	kv("Goroutines", fmt.Sprintf("%d / %d cores", runtime.NumGoroutine(), runtime.NumCPU()), "")
	reqRate := stats.Global.ReqPerSec()
	inRate, outRate := stats.Global.ByteRate()
	kv("Requests", fmt.Sprintf("%.1f req/s (last minute)", reqRate), "")
	kv("Bandwidth", fmt.Sprintf("↓ %s/s  ↑ %s/s (last minute)", niceSize(int64(inRate)), niceSize(int64(outRate))), "")
	kv("Memory", fmt.Sprintf("%d%% (%s / %s)", memPct, niceSize(int64(m.Sys)), niceSize(int64(totalMem()))), memClass(memPct))

	// Disk stats
	dbPath := os.Getenv("ARENA_DB"); if dbPath == "" { dbPath = "/opt/arena/arena.db" }
	dbSize := fileSize(dbPath)
	backupDir := filepath.Join(filepath.Dir(dbPath), "backup")
	backupSize, backupCount := dirSize(backupDir)
	lastBackup := lastBackupFile(backupDir)
	totalPartition := totalDisk(filepath.Dir(dbPath))
	totalArena := dbSize + backupSize
	arenaDiskPct := 0
	if totalPartition > 0 { arenaDiskPct = int(float64(totalArena) / float64(totalPartition) * 100) }
	arenaDiskClass := ""; if arenaDiskPct > 90 { arenaDiskClass = "critical" } else if arenaDiskPct > 80 { arenaDiskClass = "warning" }
	kv("Disk", fmt.Sprintf("%d%% (%s / %s)", arenaDiskPct, niceSize(totalArena), niceSize(int64(totalPartition))), arenaDiskClass)
	kv("  database", fmt.Sprintf("%s (%s)", niceSize(dbSize), dbPath), "")
	kv("  backups", fmt.Sprintf("%s (%d file", niceSize(backupSize), backupCount)+func() string { if backupCount != 1 { return "s)" } else { return ")" } }(), "")
	kv("Last backup", lastBackup, "")
	io.WriteString(w, "</table>")

	// ── System ──
	sysDur, sysStart := sysUptime()
	sysCPU := cpuPercent()
	cpuClass := ""; if sysCPU > 80 { cpuClass = "critical" } else if sysCPU > 50 { cpuClass = "warning" }
	sysMemPct, memUsed, memTotal := sysMemInfo()
	memClass2 := ""; if sysMemPct > 90 { memClass2 = "critical" } else if sysMemPct > 75 { memClass2 = "warning" }
	diskPct, diskUsed, diskTotal := sysDiskInfo()
	diskClass := ""; if diskPct > 90 { diskClass = "critical" } else if diskPct > 80 { diskClass = "warning" }

	io.WriteString(w, `<h2>System</h2><table class="stats-table">`)
	osName := readProcFile("/etc/os-release", "PRETTY_NAME")
	kernel := readProcFile("/proc/version", "")
	if i := strings.Index(kernel, " ("); i >= 0 { kernel = "Linux kernel " + kernel[:i] }
	kv("OS", osName+" ("+kernel+")", "")
	kv("Online", sysDur+" ("+sysStart+")", "")
	kv("CPU", fmt.Sprintf("%d%% (%d cores)", sysCPU, runtime.NumCPU()), cpuClass)
	kv("Memory", fmt.Sprintf("%d%% (%s / %s)", sysMemPct, niceSize(memUsed), niceSize(memTotal)), memClass2)
	kv("Disk", fmt.Sprintf("%d%% (%s / %s)", diskPct, niceSize(diskUsed), niceSize(diskTotal)), diskClass)
	io.WriteString(w, "</table>")
}

func fileSize(path string) int64 {
	s, err := os.Stat(path)
	if err != nil { return 0 }
	return s.Size()
}

func dirSize(path string) (int64, int) {
	var total int64
	var count int
	entries, err := os.ReadDir(path)
	if err != nil { return 0, 0 }
	for _, e := range entries {
		if !e.IsDir() {
			if info, err := e.Info(); err == nil { total += info.Size(); count++ }
		}
	}
	return total, count
}

func lastBackupFile(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 { return "none" }
	var latest os.FileInfo
	for _, e := range entries {
		if !e.IsDir() {
			if info, err := e.Info(); err == nil {
				if latest == nil || info.ModTime().After(latest.ModTime()) { latest = info }
			}
		}
	}
	if latest == nil { return "none" }
	t := latest.ModTime()
	d := time.Since(t)
	if d < 0 { d = -d }
	return fmt.Sprintf("%s ago (%s)", niceDuration(t), t.Format("2006-01-02 15:04"))
}

func arenaDiskInfo(path string) (int, int64, int64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err == nil {
		total := int64(stat.Blocks) * int64(stat.Bsize)
		avail := int64(stat.Bavail) * int64(stat.Bsize)
		used := total - avail
		if total > 0 { return int(float64(used) / float64(total) * 100), used, total }
	}
	return 0, 0, 0
}

func totalDisk(path string) uint64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err == nil {
		return uint64(stat.Blocks) * uint64(stat.Bsize)
	}
	return 0
}


func niceDuration(t time.Time) string {
	d := time.Since(t)
	if d < 0 { d = -d }
	switch {
	case d < time.Minute: return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour: return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour: return fmt.Sprintf("%dh", int(d.Hours()))
	default: return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func niceSize(n int64) string {
	suf := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(n)
	i := 0
	for i < len(suf)-1 && f >= 995 { f /= 1024; i++ }
	if f < 10 { return fmt.Sprintf("%.1f %s", f, suf[i]) }
	return fmt.Sprintf("%.0f %s", f, suf[i])
}

func sysUptime() (string, string) {
	d, _ := os.ReadFile("/proc/uptime")
	var secs float64
	fmt.Sscanf(string(d), "%f", &secs)
	start := time.Now().Add(-time.Duration(secs) * time.Second)
	return niceDuration(start), start.Format("2006-01-02 15:04")
}

func sysMemInfo() (pct int, used, total int64) {
	d, _ := os.ReadFile("/proc/meminfo")
	var totalKB, availKB int64
	for _, line := range strings.Split(string(d), "\n") {
		var kb int64
		if strings.HasPrefix(line, "MemTotal:") { fmt.Sscanf(strings.Fields(line)[1], "%d", &kb); totalKB = kb }
		if strings.HasPrefix(line, "MemAvailable:") { fmt.Sscanf(strings.Fields(line)[1], "%d", &kb); availKB = kb }
	}
	total = totalKB * 1024
	used = (totalKB - availKB) * 1024
	if total > 0 { pct = int(float64(used) / float64(total) * 100) }
	return
}

func sysDiskInfo() (pct int, used, total int64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err == nil {
		total = int64(stat.Blocks) * int64(stat.Bsize)
		avail := int64(stat.Bavail) * int64(stat.Bsize)
		used = total - avail
		if total > 0 { pct = int(float64(used) / float64(total) * 100) }
	}
	return
}

func cpuPercent() int {
	d, _ := os.ReadFile("/proc/stat")
	var user, nice, system, idle uint64
	fmt.Sscanf(string(d), "cpu %d %d %d %d", &user, &nice, &system, &idle)
	total := user + nice + system + idle
	if total > 0 { return int(100 - (float64(idle)/float64(total))*100) }
	return 0
}

func totalMem() uint64 {
	d, _ := os.ReadFile("/proc/meminfo")
	for _, line := range strings.Split(string(d), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			var kb uint64
			fmt.Sscanf(strings.Fields(line)[1], "%d", &kb)
			return kb * 1024
		}
	}
	return 0
}

func memClass(pct int) string {
	if pct > 80 { return "critical" }
	if pct > 50 { return "warning" }
	return ""
}

func readProcFile(path, key string) string {
	d, _ := os.ReadFile(path)
	if key == "" { return strings.TrimSpace(string(d)) }
	for _, line := range strings.Split(string(d), "\n") {
		if strings.HasPrefix(line, key+"=") {
			return strings.Trim(strings.TrimPrefix(line, key+"="), "\"")
		}
	}
	return ""
}


func (h *Handler) handleAdmin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, pageHead+navHTML+`<h1>Admin — API Tokens</h1>
		<table><tr><th>Token</th><th>Email</th><th>Nickname</th><th>Comment</th><th>Status</th><th>Used</th><th>Last</th><th></th></tr>`)
	rows, _ := h.DB.Query("SELECT id, SUBSTR(token,1,4)||'...'||SUBSTR(token,-4), email, COALESCE(nickname,''), COALESCE(comment,''), use_count, COALESCE(last_used,''), active FROM api_tokens ORDER BY created_at DESC")
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var id, count, active int; var tok, email, nick, comment, last string
			rows.Scan(&id, &tok, &email, &nick, &comment, &count, &last, &active)
			if nick == "" { nick = email }
			status := `<span class="win">active</span>`
			suspendLink := fmt.Sprintf(`<a href="/admin/suspend/%d">suspend</a>`, id)
			if active == 0 { status = `<span class="loss">suspended</span>`; suspendLink = fmt.Sprintf(`<a href="/admin/suspend/%d">reactivate</a>`, id) }
			fmt.Fprintf(w, `<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%d</td><td>%s</td><td><a href="#" onclick="edit(%d,'%s','%s');return false">edit</a> %s <a href="/admin/delete/%d" onclick="return confirm('"'"'Delete token?'"'"')">delete</a></td></tr>`, tok, email, htmlEscape(nick), htmlEscape(comment), status, count, last[:min(19,len(last))], id, htmlEscape(nick), htmlEscape(comment), suspendLink, id)
		}
	}
	io.WriteString(w, `</table><hr><form method="post"><h3>Edit Token</h3><input type="hidden" name="id" id="edit-id"><table><tr><th>Nickname</th><td><input name="nickname" id="edit-nick" style="width:300px" placeholder="Coach nickname"></td></tr><tr><th>Comment</th><td><input name="comment" id="edit-comment" style="width:300px" placeholder="Optional comment"></td></tr></table><button type="submit">Save</button></form>
		<hr><form method="post" action="/admin/new"><h3>Create Token</h3><table><tr><th>Email</th><td><input name="email" style="width:300px" placeholder="user@example.com" required></td></tr><tr><th>Nickname</th><td><input name="nickname" style="width:300px" placeholder="Coach nickname"></td></tr><tr><th>Comment</th><td><input name="comment" style="width:300px" placeholder="Optional comment"></td></tr></table><button type="submit">Generate Token</button></form><script>function edit(id,n,c){document.getElementById("edit-id").value=id;document.getElementById("edit-nick").value=n;document.getElementById("edit-comment").value=c}</script>`+pageFoot)
}

func (h *Handler) handleAdminSave(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	id := r.FormValue("id")
	nick := r.FormValue("nickname")
	comment := r.FormValue("comment")
	if id != "" {
		h.DB.Exec("UPDATE api_tokens SET nickname=?, comment=? WHERE id=?", nick, comment, id)
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}


func (h *Handler) handleAdminSuspend(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var active int
	h.DB.QueryRow("SELECT active FROM api_tokens WHERE id=?", id).Scan(&active)
	if active == 1 { h.DB.Exec("UPDATE api_tokens SET active=0 WHERE id=?", id) } else { h.DB.Exec("UPDATE api_tokens SET active=1 WHERE id=?", id) }
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *Handler) handleAdminDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	h.DB.Exec("DELETE FROM api_tokens WHERE id=?", id)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *Handler) handleAdminNew(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	email := r.FormValue("email")
	nickname := r.FormValue("nickname")
	comment := r.FormValue("comment")
	if email != "" {
		token := db.GenerateToken()
		h.DB.Exec("INSERT INTO api_tokens (token, email, nickname, comment) VALUES (?,?,?,?)", token, email, nickname, comment)
		// Show the token once
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, pageHead+navHTML+fmt.Sprintf(`<h1>New Token</h1><p>Email: %s</p><p>Nickname: %s</p><p style="font-family:monospace;background:var(--th-bg);padding:1em;border-radius:4px">%s</p><p style="color:var(--muted)">Copy this token now — it won'"'"'t be shown again.</p><p><a href="/admin">Back to Admin</a></p>`, email, nickname, token)+pageFoot)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}


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

	// SVG bar chart helper
	maxW := 600; barH := 20; gap := 4
	drawBars := func(title, unit string, getVal func(engineStats) float64, getMax func() float64, color string) string {
		var svg strings.Builder
		maxVal := getMax()
		if maxVal == 0 { maxVal = 1 }
		height := len(stats)*(barH+gap) + 10
		fmt.Fprintf(&svg, `<h3>%s</h3><svg viewBox="0 0 %d %d" style="width:100%%;max-width:%dpx">`, title, maxW+160, height, maxW+160)
		for i, s := range stats {
			val := getVal(s)
			w := int(val / maxVal * float64(maxW))
			if w < 2 { w = 2 }
			y := i*(barH+gap)
			lbl := s.Name
			if len(lbl) > 18 { lbl = lbl[:17]+"…" }
			fmt.Fprintf(&svg, `<g class="filter-item"><text x="0" y="%d" fill="var(--fg)" font-size="11">%s</text>`, y+12, lbl)
			fmt.Fprintf(&svg, `<rect x="155" y="%d" width="%d" height="%d" fill="%s" rx="2"/>`, y, w, barH, color)
			fmt.Fprintf(&svg, `<text x="%d" y="%d" fill="var(--muted)" font-size="10">%s</text></g>`, 160+w, y+12, fmt.Sprintf("%.0f%s", val, unit))
		}
		svg.WriteString(`</svg>`)
		return svg.String()
	}

	getGames := func(s engineStats) float64 { return float64(s.Games) }
	getMaxGames := func() float64 { m := 0.0; for _, s := range stats { if float64(s.Games) > m { m = float64(s.Games) } }; return m }
	switch chart {
	case "games":
		io.WriteString(w, drawBars("Games per Engine", "", getGames, getMaxGames, chartColors[0]))
	case "length":
		getPly := func(s engineStats) float64 { return float64(s.AvgPly) }
	getMaxPly := func() float64 { m := 0.0; for _, s := range stats { if float64(s.AvgPly) > m { m = float64(s.AvgPly) } }; return m }
	io.WriteString(w, drawBars("Average Game Length (plies)", "", getPly, getMaxPly, chartColors[1]))
	case "timeout":
		if len(stats) > 0 && stats[0].TotalMoves > 0 {
		getTO := func(s engineStats) float64 { return float64(s.Timeouts) * 100 / float64(max(s.TotalMoves,1)) }
		getMaxTO := func() float64 { m := 0.0; for _, s := range stats { v := getTO(s); if v > m { m = v } }; return m }
		io.WriteString(w, drawBars("Timeout Rate (%)", "%", getTO, getMaxTO, chartColors[2]))
		}
	case "unspent":
		getUnspent := func(s engineStats) float64 { return s.UnspentPct }
	getMaxUnspent := func() float64 { m := 0.0; for _, s := range stats { if s.UnspentPct > m { m = s.UnspentPct } }; return m }
	io.WriteString(w, drawBars("Unspent Time (%)", "%", getUnspent, getMaxUnspent, chartColors[3]))
	}

	io.WriteString(w, `<p style="color:var(--muted);margin-top:2em">Unspent time = how much of the allocated time budget goes unused, averaged across all games and time controls. Higher % means the engine finishes early (fast).</p>`)
	}


func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&#34;")
	s = strings.ReplaceAll(s, "'", "&#39;")
	s = strings.ReplaceAll(s, "\n", "<br>")
	return s
}
