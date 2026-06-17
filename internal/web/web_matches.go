package web

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

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
	io.WriteString(w, "</table>"+`</div>`+pageFoot)

	var completedCount int
	h.DB.QueryRow("SELECT COUNT(*) FROM matches").Scan(&completedCount)
	fmt.Fprintf(w, `<h2>Completed %d</h2><table><tr><th>ID</th><th>Black</th><th>White</th><th>Score</th><th>Games</th><th>Date</th></tr>`, completedCount)
	rows, _ := h.DB.Query(`SELECT m.id, (SELECT name||' '||version FROM engines WHERE id=m.engine1_id), (SELECT name||' '||version FROM engines WHERE id=m.engine2_id), m.wins_1, m.wins_2, m.draws, m.total_games, COALESCE(m.created_at,'') FROM matches m ORDER BY m.id DESC LIMIT 100`)
	if rows != nil { defer rows.Close(); for rows.Next() { var id, w1, w2, d, t int; var e1, e2, created string; rows.Scan(&id, &e1, &e2, &w1, &w2, &d, &t, &created); fmt.Fprintf(w, `<tr class="filter-row"><td><a href="/matches/%d">%d</a></td><td>%s</td><td>%s</td><td>%d-%d-%d</td><td>%d</td><td>%s</td></tr>`, id, id, htmlEscape(e1), htmlEscape(e2), w1, w2, d, t, htmlEscape(created[:min(10,len(created))])) } }
	io.WriteString(w, "</table>"+`</div>`+pageFoot)
}

func (h *Handler) handleMatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, pageHead+navHTML)
	fmt.Fprintf(w, "<h1>Match #%s</h1><table><tr><th>#</th><th>Black</th><th>White</th><th>Result</th><th>Score</th><th>Opening</th></tr>", id)
	rows, _ := h.DB.Query(`SELECT g.game_number, (SELECT name||' '||version FROM engines WHERE id=g.black_id), (SELECT name||' '||version FROM engines WHERE id=g.white_id), g.result, COALESCE(g.final_score,0), COALESCE(g.opening_line,'') FROM games g WHERE g.match_id=? ORDER BY g.game_number`, id)
	if rows != nil { defer rows.Close(); for rows.Next() { var num, s int; var blk, wht, r, o string; rows.Scan(&num, &blk, &wht, &r, &s, &o); badge := ""; if r == "1-0" { badge = `<span class="badge win">W</span>` } else if r == "0-1" { badge = `<span class="badge loss">L</span>` } else { badge = `<span class="badge draw">D</span>` }; fmt.Fprintf(w, `<tr><td><a href="/games/%d">%d</a></td><td>%s</td><td>%s</td><td>%s %s</td><td>%+d</td><td>%s</td></tr>`, num, num, htmlEscape(blk), htmlEscape(wht), htmlEscape(r), badge, s, htmlEscape(o)) } }
	io.WriteString(w, "</table>"+`</div>`+pageFoot)
}

