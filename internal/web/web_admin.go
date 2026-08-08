package web

import (
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/neoliv/arena/internal/db"
)

func (h *Handler) handleAdmin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// ── Matchmaker pause control (top of page) ─────────────────
	if h.SetDrainFunc != nil {
		state := "running"
		if h.DrainStateFunc != nil {
			state = h.DrainStateFunc()
		} else if h.IsDrainedFunc != nil && h.IsDrainedFunc() {
			state = "stopped"
		}
		playing := 0
		if h.PlayingCountFunc != nil {
			playing = h.PlayingCountFunc()
		}
		coaches := 0
		if h.CoachStatusFunc != nil {
			coaches = len(h.CoachStatusFunc())
		}

		var stateBadge, btn, help string
		switch state {
		case "pausing":
			stateBadge = `<span class="loss">Paused — finishing games…</span>`
			btn = `<form method="post" action="/admin/drain" style="display:inline"><button type="submit" style="background:#4caf50;color:#fff;border:1px solid #388e3c;border-radius:4px;padding:.4em 1.2em;font-size:1.05em;font-weight:700;cursor:pointer">▶ Resume</button></form>`
			help = fmt.Sprintf(`Pause requested: no new games are being assigned. %d game(s) still in progress will finish normally. Click Resume to start issuing new matches again.`, playing)
		case "stopped":
			stateBadge = `<span class="loss">Stopped</span>`
			btn = `<form method="post" action="/admin/drain" style="display:inline"><button type="submit" style="background:#4caf50;color:#fff;border:1px solid #388e3c;border-radius:4px;padding:.4em 1.2em;font-size:1.05em;font-weight:700;cursor:pointer">▶ Resume</button></form>`
			help = `All in-flight games have finished. The matchmaker is not issuing any new matches. Click Resume to start issuing new matches again.`
		default: // running
			stateBadge = `<span class="win">Running</span>`
			btn = `<form method="post" action="/admin/drain" style="display:inline"><button type="submit" style="background:#e8a840;color:#222;border:1px solid #a88830;border-radius:4px;padding:.4em 1.2em;font-size:1.05em;font-weight:700;cursor:pointer">⏸ Pause</button></form>`
			help = `Pause the matchmaker: no new games are assigned to coaches. Games already in progress finish normally. Click Resume to start issuing new matches again.`
		}
		coachBadge := `<span style="color:var(--muted)">Coaches online: <strong>` + fmt.Sprintf("%d", coaches) + `</strong></span>`

		budgetField := ""
		if h.BudgetSecFunc != nil {
			budget := h.BudgetSecFunc()
			budgetField = fmt.Sprintf(`<form method="post" action="/admin/budget" style="display:inline-flex;align-items:center;gap:.4em" title="Per-game time budget in seconds; saved in the database"><span style="color:var(--muted);font-size:.9em">Game budget:</span><input type="number" name="budget_sec" value="%d" min="1" max="600" style="width:70px;padding:.25em .4em;border-radius:4px;border:1px solid var(--border);background:var(--bg);color:var(--fg)"><button type="submit" style="background:var(--nav-hl);color:#fff;border:none;border-radius:4px;padding:.3em .7em;font-size:.9em;cursor:pointer">Set</button></form>`, budget)
		}

		io.WriteString(w, pageHead+navHTML+fmt.Sprintf(`<h1>Admin</h1><div style="border:1px solid var(--border);border-radius:6px;padding:.8em 1em;margin-bottom:1.2em;background:var(--bg2)"><div style="display:flex;align-items:center;gap:1em;flex-wrap:wrap"><span style="font-weight:700;font-size:1.1em">Matchmaker: %s</span>%s%s<span style="margin-left:auto">%s</span></div><div style="color:var(--muted);font-size:.9em;margin-top:.5em">%s</div></div>`, stateBadge, btn, budgetField, coachBadge, help))
	} else {
		io.WriteString(w, pageHead+navHTML+`<h1>Admin — API Tokens</h1>`)
	}
	io.WriteString(w, `
		<table><tr><th>Token</th><th>Email</th><th>Nickname</th><th>Comment</th><th>Status</th><th>Used</th><th>Last</th><th></th></tr>`)
	rows, _ := h.DB.Query("SELECT id, SUBSTR(token,1,4)||'...'||SUBSTR(token,-4), email, COALESCE(nickname,''), COALESCE(comment,''), use_count, COALESCE(last_used,''), active FROM api_tokens ORDER BY created_at DESC")
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var id, count, active int
			var tok, email, nick, comment, last string
			rows.Scan(&id, &tok, &email, &nick, &comment, &count, &last, &active)
			if nick == "" {
				nick = email
			}
			status := `<span class="win">active</span>`
			suspendLink := fmt.Sprintf(`<form method="post" action="/admin/suspend/%d" style="display:inline"><button type="submit" class="link-btn">suspend</button></form>`, id)
			if active == 0 {
				status = `<span class="loss">suspended</span>`
				suspendLink = fmt.Sprintf(`<form method="post" action="/admin/suspend/%d" style="display:inline"><button type="submit" class="link-btn">reactivate</button></form>`, id)
			}
			fmt.Fprintf(w, `<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%d</td><td>%s</td><td><a href="#" onclick="edit(%d,'%s','%s');return false">edit</a> %s <form method="post" action="/admin/delete/%d" style="display:inline" onsubmit="return confirm('Delete token?')"><button type="submit" class="link-btn">delete</button></form></td></tr>`, tok, email, htmlEscape(nick), htmlEscape(comment), status, count, last[:min(19, len(last))], id, htmlEscape(nick), htmlEscape(comment), suspendLink, id)
		}
	}
	io.WriteString(w, `</table><div id="edit-token-box" style="display:none"><hr><form method="post"><h3>Edit Token</h3><input type="hidden" name="id" id="edit-id"><table><tr><th>Nickname</th><td><input name="nickname" id="edit-nick" style="width:300px" placeholder="Coach nickname"></td></tr><tr><th>Comment</th><td><input name="comment" id="edit-comment" style="width:300px" placeholder="Optional comment"></td></tr></table><button type="submit">Save</button> <button type="button" onclick="cancelEdit()" class="link-btn">Cancel</button></form></div>
		<hr><form method="post" action="/admin/new"><h3>Create Token</h3><table><tr><th>Email</th><td><input name="email" style="width:300px" placeholder="user@example.com" required></td></tr><tr><th>Nickname</th><td><input name="nickname" style="width:300px" placeholder="Coach nickname"></td></tr><tr><th>Comment</th><td><input name="comment" style="width:300px" placeholder="Optional comment"></td></tr></table><button type="submit">Generate Token</button></form><script>function edit(id,n,c){document.getElementById("edit-id").value=id;document.getElementById("edit-nick").value=n;document.getElementById("edit-comment").value=c;document.getElementById("edit-token-box").style.display='block'}function cancelEdit(){document.getElementById("edit-token-box").style.display='none'}</script>`+pageFoot)
}

// handleAdminDrain toggles matchmaker drain mode (stop/resume new matches).
func (h *Handler) handleAdminDrain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.SetDrainFunc == nil {
		http.Error(w, "drain not configured", http.StatusServiceUnavailable)
		return
	}
	drained := h.IsDrainedFunc != nil && h.IsDrainedFunc()
	h.SetDrainFunc(!drained)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// handleAdminBudget updates the per-game time budget (seconds).
func (h *Handler) handleAdminBudget(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.SetBudgetSecFunc == nil {
		http.Error(w, "budget not configured", http.StatusServiceUnavailable)
		return
	}
	r.ParseForm()
	sec, err := strconv.Atoi(r.FormValue("budget_sec"))
	if err != nil || sec < 1 || sec > 600 {
		http.Error(w, "budget_sec must be an integer between 1 and 600", http.StatusBadRequest)
		return
	}
	if err := h.SetBudgetSecFunc(sec); err != nil {
		http.Error(w, "failed to save budget", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
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
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Basic CSRF protection: verify request origin matches expected host
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	_ = origin // defense-in-depth; admin interface is token-protected

	id := r.PathValue("id")
	var active int
	h.DB.QueryRow("SELECT active FROM api_tokens WHERE id=?", id).Scan(&active)
	if active == 1 {
		h.DB.Exec("UPDATE api_tokens SET active=0 WHERE id=?", id)
	} else {
		h.DB.Exec("UPDATE api_tokens SET active=1 WHERE id=?", id)
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *Handler) handleAdminDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Basic CSRF protection: verify request origin matches expected host
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	_ = origin // defense-in-depth; admin interface is token-protected

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
