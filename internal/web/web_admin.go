package web

import (
	"fmt"
	"io"
	"net/http"

	"github.com/neoliv/arena/internal/db"
)

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


