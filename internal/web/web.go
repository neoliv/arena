package web

import (
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/neoliv/arena/internal/coach"
	"github.com/neoliv/arena/internal/db"
)

var SharedCSS = sharedCSS

const sharedCSS = `<style>
:root{--bg:#fafafa;--fg:#222;--muted:#666;--border:#ddd;--hover:#f0f0f5;--th-bg:#f0f0f0;--link:#4caf50;--link-visited:#b8a070;--link-hover:#388e3c;--nav-hl:#1a5c3a;--bg2:#fff;--accent:var(--nav-hl);--win-bg:#dfd;--win-fg:#060;--loss-bg:#fdd;--loss-fg:#600;--draw-bg:#ffd;--draw-fg:#660;color-scheme:light}
@media(prefers-color-scheme:dark){:root{--bg:#1a1e1a;--fg:#e8e6e3;--muted:#a9a7a3;--border:#333;--hover:#253028;--th-bg:#222a24;--link:#7a7;--link-visited:#8a7a5a;--link-hover:#9b9;--nav-hl:#284;--bg2:#213025;--accent:var(--nav-hl);--win-bg:#1a3a1a;--win-fg:#7f7;--loss-bg:#3a1a1a;--loss-fg:#f77;--draw-bg:#3a3a1a;--draw-fg:#ee7;color-scheme:dark}}
body{font-family:system-ui,sans-serif;max-width:960px;margin:0 auto;padding:1em;color:var(--fg);background:var(--bg)}
h1{font-size:1.4em;margin:0 0 .5em}
nav{margin-bottom:1.5em;border-bottom:1px solid var(--border);padding-bottom:.5em}
nav a{display:inline-block;margin-right:.3em;text-decoration:none;color:#e8e6e3;font-size:1.1em;font-weight:600;padding:.35em .7em;border-radius:5px;border:1px solid var(--nav-hl);background:rgba(56,136,85,0.06);transition:all .15s}
nav a:visited,nav a:link{color:#e8e6e3}
nav a:hover{background:var(--nav-hl);color:#fff;border-color:var(--nav-hl)}
nav a.logout:hover{background:#c33;border-color:#c33;color:#fff}
nav a.active,.chart-tabs a.active,nav a.active:visited{background:var(--nav-hl);color:#fff !important}
	.chart-tab{transition:all .15s}.chart-tab:hover{background:var(--nav-hl)!important;color:#fff!important}.chart-tab-errors:hover{background:rgba(244,67,54,0.4)!important;border-color:rgba(244,67,54,0.8)!important;color:#fff!important}.chart-tab-errors{transition:all .15s}
table{border-collapse:collapse;width:100%;margin-bottom:2em}
th,td{text-align:left;padding:.4em .6em;border-bottom:1px solid var(--border)}
th{font-weight:600;background:var(--th-bg);cursor:pointer;user-select:none;position:relative;padding-right:18px}th:hover{background:var(--hover)}td{white-space:nowrap}.sort-ind{position:absolute;right:4px;top:50%;transform:translateY(-50%);font-size:1.2em;font-weight:900;color:var(--fg)}
tr:hover{background:var(--hover)}
a{color:var(--link)}a:link{color:var(--link)}a:visited{color:var(--link-visited)}
.badge{padding:.1em .4em;border-radius:3px;font-size:.85em}
.win{background:var(--win-bg);color:var(--win-fg)}
.loss{background:var(--loss-bg);color:var(--loss-fg)}
.draw{background:var(--draw-bg);color:var(--draw-fg)}
.bar{display:inline-block;height:12px;background:var(--link);border-radius:3px}
input,select{background:var(--bg);color:var(--fg);border:1px solid var(--nav-hl);background:rgba(56,136,85,0.06);padding:.2em .5em;border-radius:4px}
#filterBox{width:100%;max-width:300px;padding:.4em .6em;margin-bottom:1em;font-size:1em;border:1px solid var(--muted);border-radius:4px;outline:none;transition:border-color .2s}#filterBox:focus{border-color:var(--link)}
.stats-table{width:auto;min-width:400px}.stats-table td:first-child{width:140px;font-weight:600;color:var(--muted)}tr.critical td:last-child{color:#f44336;font-weight:600}tr.warning td:last-child{color:#ff9800;font-weight:600}
.stats-table{width:auto;min-width:400px}.stats-table td:first-child{width:140px;font-weight:600;color:var(--muted)}tr.critical td:last-child{color:#f44336;font-weight:600}tr.warning td:last-child{color:#ff9800;font-weight:600}
#board-container svg{max-width:320px;width:100%;height:auto}
	</style>`

const searchJS = `<scr` + `ipt>
var filterMode='OR';
function toggleMode(btn){filterMode=filterMode==='OR'?'AND':'OR';btn.textContent=filterMode;btn.style.background=filterMode==='AND'?'var(--nav-hl)':'rgba(56,136,85,0.06)';btn.style.color=filterMode==='AND'?'#fff':'var(--fg)';filter()}
function filter(){let q=document.getElementById('filterBox').value.toLowerCase().trim();if(!q){document.querySelectorAll('tr.filter-row').forEach(r=>r.style.display='');document.querySelectorAll('.filter-item').forEach(r=>r.style.display='');updateCounts();return}
let words=q.split(/\s+/),inc=[],exc=[];words.forEach(function(w){if(w.startsWith('-')){exc.push(w.slice(1))}else{inc.push(w)}});let useAnd=filterMode==='AND';
function match(t){for(var i=0;i<exc.length;i++){if(t.includes(exc[i]))return false}if(inc.length===0)return true;if(useAnd){for(var i=0;i<inc.length;i++){if(!t.includes(inc[i]))return false}return true}else{for(var i=0;i<inc.length;i++){if(t.includes(inc[i]))return true}return false}}
document.querySelectorAll('tr.filter-row').forEach(r=>{let t=r.textContent.toLowerCase();r.style.display=match(t)?'':'none'})
document.querySelectorAll('.filter-item').forEach(r=>{let t=r.textContent.toLowerCase();r.style.display=match(t)?'':'none'});updateCounts()}
function updateCounts(){document.querySelectorAll('h2').forEach(function(h){let t=h.nextElementSibling;if(!t||t.tagName!=='TABLE')return;let n=0;t.querySelectorAll('tr.filter-row').forEach(function(r){if(r.style.display!=='none')n++});let s=h.querySelector('.section-count');let txt=' ('+n+')';if(s){s.textContent=txt}else{let el=document.createElement('span');el.className='section-count';el.style.fontWeight='normal';el.style.color='var(--muted)';el.textContent=txt;h.appendChild(el)}})}
setTimeout(updateCounts,10);var sc=-1,sa=!0;
function st(t,c,n){var b=t.querySelector("tbody")||t,r=Array.from(b.querySelectorAll("tr.filter-row"));
if(c===sc)sa=!sa;else{sa=!0;sc=c}
r.sort(function(a,b){var va=a.cells[c].textContent.trim(),vb=b.cells[c].textContent.trim();
if(n){va=parseFloat(va)||0;vb=parseFloat(vb)||0}
return sa?va>vb?1:va<vb?-1:0:va<vb?1:va>vb?-1:0});
r.forEach(function(r){b.appendChild(r)});
t.querySelectorAll("th").forEach(function(t,i){var s=t.querySelector(".sort-ind");if(!s){s=document.createElement("span");s.className="sort-ind";t.appendChild(s)}s.textContent=i===c?(sa?"▲":"▼"):""})}
<` + `/script>`

const filterBox = `<div style="display:flex;align-items:center;gap:.4em;margin-bottom:1em"><input type="search" id="filterBox" placeholder="Filter…" oninput="filter()" autofocus style="flex:1;max-width:320px"><button id="filterModeBtn" onclick="toggleMode(this)" title="AND: all words must match | OR: any word matches" style="padding:.2em .7em;border-radius:4px;border:1px solid var(--nav-hl);font-size:.85em;cursor:pointer;background:rgba(56,136,85,0.06);color:var(--fg);font-weight:600">OR</button><span title="- prefix excludes words, e.g. neur nrsi -d10" style="color:var(--muted);cursor:help;font-size:.85em;border-bottom:1px dotted var(--muted)">?</span></div>`

var navHTML = `<nav>
	<a href="/stats">Stats</a>
	<a href="/games">Games</a> <a href="/players">Players</a> <a href="/coaches">Coaches</a>
	<a href="/health">Health</a> <a href="/admin">Admin</a>
	<span style="float:right"><a class="logout" href="/logout">Disconnect</a></span>
	</nav>`

var bannerOnce sync.Once
func SetRollbackBanner() {
	bannerOnce.Do(func() {
		navHTML = `<div style="background:#c44;color:#fff;text-align:center;padding:.4em;font-weight:600;margin-bottom:.5em">⚠ Database was restored from backup — recent games may be missing.</div>` + navHTML
	})
}

const htmxScript = `<script src="https://cdn.jsdelivr.net/npm/htmx.org@2.0.4"></script>`
const pageHead = `<!DOCTYPE html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Othello Arena</title>` + sharedCSS + htmxScript + searchJS + `</head><body>`
const pageFoot = `</body></html>`

// htmxWrap wraps auto-refresh content. When the request came from HTMX
// (HX-Request header), it returns only the inner div — no page chrome.
// This prevents nested <html> documents on auto-refreshing pages.
func htmxWrap(r *http.Request) (open, closing string) {
	path := r.URL.Path
	if r.URL.RawQuery != "" { path += "?" + r.URL.RawQuery }
	open = `<div hx-get="` + path + `" hx-trigger="every 30s" hx-swap="outerHTML">` + filterBox
	closing = `</div>`
	if r.Header.Get("HX-Request") != "true" {
		open = pageHead + navHTML + open
		closing = closing + pageFoot
	}
	return
}

// chartColors are chalk/pastel hues visible on both light and dark backgrounds.
var chartColors = [8]string{"#4caf50","#6bd4ff","#ffe66b","#6bff8a","#ff8a6b","#c46bff","#6bffe6","#ffb86b"}

// EngineStatus is a point-in-time snapshot of a registered engine.
type EngineStatus struct {
	Name              string
	Version           string
	CoachID           string
	Available         bool
	UnavailableReason string
}

// CoachStatus is a point-in-time snapshot of a coach (in-memory, not DB).
type CoachStatus struct {
	ID         string
	SessionID  string
	CoresTotal int
	CoresUsed  int
	MemUsed    int
	LastSeen   time.Time
}

// AssignmentStatus is a point-in-time snapshot of an in-progress match.
type AssignmentStatus struct {
	ID           int64
	BlackEngine  string
	WhiteEngine  string
	TimeControl  string
	NumGames     int
	InProgressAt time.Time
}

type Handler struct {
	DB                   *db.DB
	Token                string
	Sessions             *SessionStore
	Limiter              *RateLimiter
	EngineStatusFunc     func() []EngineStatus
	CoachStatusFunc      func() []CoachStatus
	ActiveAssignmentsFunc func() []AssignmentStatus
	ResourceStore        *coach.PlayerResourceStore
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", h.handleLogin)
	mux.HandleFunc("POST /login", h.handleLogin)
	mux.HandleFunc("GET /logout", h.HandleLogout)
	mux.HandleFunc("GET /{$}", h.RequireLogin(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/stats", http.StatusMovedPermanently) }))
	// charts route handled below
	mux.HandleFunc("GET /graphs", h.RequireLogin(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/stats?tab="+r.URL.Query().Get("tab"), http.StatusMovedPermanently) }))
	mux.HandleFunc("GET /stats", h.RequireLogin(h.handleGraphs))
	mux.HandleFunc("GET /games", h.RequireLogin(h.handleGames))
		mux.HandleFunc("GET /games/{id}", h.RequireLogin(h.handleGameDetail))
	mux.HandleFunc("GET /engines/{name}", h.RequireLogin(h.handleEngine))
	mux.HandleFunc("GET /versions", h.RequireLogin(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/players", http.StatusMovedPermanently) }))
	mux.HandleFunc("GET /ranks", h.RequireLogin(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/stats?tab=elo", http.StatusMovedPermanently) }))
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
