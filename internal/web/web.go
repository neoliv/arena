package web

import (
	"io"
	"net/http"

	"github.com/neoliv/arena/internal/db"
)

var SharedCSS = sharedCSS

const sharedCSS = `<style>
:root{--bg:#fafafa;--fg:#222;--muted:#666;--border:#ddd;--hover:#f0f0f5;--th-bg:#f0f0f0;--link:#385;--link-hover:#263;--nav-hl:#1a5c3a;--bg2:#fff;--accent:var(--nav-hl);--win-bg:#dfd;--win-fg:#060;--loss-bg:#fdd;--loss-fg:#600;--draw-bg:#ffd;--draw-fg:#660;color-scheme:light}
@media(prefers-color-scheme:dark){:root{--bg:#1a1e1a;--fg:#e8e6e3;--muted:#a9a7a3;--border:#333;--hover:#253028;--th-bg:#222a24;--link:#7a7;--link-visited:#4a4;--link-hover:#9b9;--nav-hl:#284;--bg2:#213025;--accent:var(--nav-hl);--win-bg:#1a3a1a;--win-fg:#7f7;--loss-bg:#3a1a1a;--loss-fg:#f77;--draw-bg:#3a3a1a;--draw-fg:#ee7;color-scheme:dark}}
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
function filter(){let q=document.getElementById('filterBox').value.toLowerCase().trim();if(!q){document.querySelectorAll('tr.filter-row').forEach(r=>r.style.display='');document.querySelectorAll('.filter-item').forEach(r=>r.style.display='');updateCounts();return}
let words=q.split(/\s+/);document.querySelectorAll('tr.filter-row').forEach(r=>{let t=r.textContent.toLowerCase();r.style.display=words.every(function(w){return t.includes(w)})?'':'none'})
document.querySelectorAll('.filter-item').forEach(r=>{let t=r.textContent.toLowerCase();r.style.display=words.every(function(w){return t.includes(w)})?'':'none'});updateCounts()}
function updateCounts(){document.querySelectorAll('h2').forEach(function(h){let t=h.nextElementSibling;if(!t||t.tagName!=='TABLE')return;let n=0;t.querySelectorAll('tr.filter-row').forEach(function(r){if(r.style.display!=='none')n++});let s=h.querySelector('.section-count');let txt=' ('+n+')';if(s){s.textContent=txt}else{let el=document.createElement('span');el.className='section-count';el.style.fontWeight='normal';el.style.color='var(--muted)';el.textContent=txt;h.appendChild(el)}})}
setTimeout(updateCounts,10);var sc=-1,sa=!0;
function st(t,c,n){var b=t.querySelector("tbody")||t,r=Array.from(b.querySelectorAll("tr.filter-row"));
if(c===sc)sa=!sa;else{sa=!0;sc=c}
r.sort(function(a,b){var va=a.cells[c].textContent.trim(),vb=b.cells[c].textContent.trim();
if(n){va=parseFloat(va)||0;vb=parseFloat(vb)||0}
return sa?va>vb?1:va<vb?-1:0:va<vb?1:va>vb?-1:0});
r.forEach(function(r){b.appendChild(r)});
t.querySelectorAll("th").forEach(function(t,i){var s=t.querySelector(".sort-ind");if(!s){s=document.createElement("span");s.className="sort-ind";t.appendChild(s)}s.textContent=i===c?(sa?"\u25b2":"\u25bc"):""})}
<` + `/script>`

const filterBox = `<input type="search" id="filterBox" placeholder="Filter…" oninput="filter()" autofocus>`

var navHTML = `<nav>
<a href="/">Ranks</a> <a href="/charts">Charts</a>
<a href="/matches">Matches</a> <a href="/games">Games</a> <a href="/players">Players</a> <a href="/coaches">Coaches</a>
<a href="/health">Health</a> <a href="/admin">Admin</a>
<span style="float:right"><a class="logout" href="/logout">Disconnect</a></span>
</nav>`

func SetRollbackBanner() {
	navHTML = `<div style="background:#c44;color:#fff;text-align:center;padding:.4em;font-weight:600;margin-bottom:.5em">⚠ Database was restored from backup — recent games may be missing.</div>` + navHTML
}

const htmxScript = `<script src="https://unpkg.com/htmx.org@2.0.4" integrity="sha384-HGxOGrUEVMQQBW1EE4IqOmxPxVJzZSoS0rIYgJOlhNYG8YP4iWm4kq6FDoGsEdJj" crossorigin="anonymous"></script>`
const pageHead = `<!DOCTYPE html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Othello Arena</title>` + sharedCSS + htmxScript + `</head><body>`
const pageFoot = `</body></html>`

// htmxWrap wraps auto-refresh content. When the request came from HTMX
// (HX-Request header), it returns only the inner div — no page chrome.
// This prevents nested <html> documents on auto-refreshing pages.
func htmxWrap(r *http.Request, path string) (open, closing string) {
	open = `<div hx-get="` + path + `" hx-trigger="every 30s" hx-swap="outerHTML">` + searchJS + filterBox
	closing = `</div>`
	if r.Header.Get("HX-Request") != "true" {
		open = pageHead + navHTML + open
		closing = closing + pageFoot
	}
	return
}

// chartColors are chalk/pastel hues visible on both light and dark backgrounds.
var chartColors = [8]string{"#4caf50","#6bd4ff","#ffe66b","#6bff8a","#ff8a6b","#c46bff","#6bffe6","#ffb86b"}

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

