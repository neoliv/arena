package web

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

func (h *Handler) handleGames(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	open, closing := htmxWrap(r)
	io.WriteString(w, open+`<h1>Games</h1>`)

	fmt.Fprintf(w, `<h2>In Progress</h2><table><tr><th onclick="st(this.closest('table'),0,true)">ID</th><th onclick="st(this.closest('table'),1,false)">Black</th><th onclick="st(this.closest('table'),2,false)">White</th><th onclick="st(this.closest('table'),3,false)">Time</th><th onclick="st(this.closest('table'),4,true)">Games</th><th onclick="st(this.closest('table'),5,false)">Started</th></tr>`)
	iRows, _ := h.DB.Query(`SELECT a.id, (SELECT name FROM engines WHERE id=a.engine1_id), (SELECT name FROM engines WHERE id=a.engine2_id), COALESCE(a.time_control,'{}'), a.num_games, COALESCE(a.in_progress_at, a.created_at) FROM match_assignments a WHERE a.status='in_progress' ORDER BY a.id DESC LIMIT 20`)
	if iRows != nil {
		defer iRows.Close()
		for iRows.Next() {
			var id, games int; var e1, e2, tc string; var started int64
			iRows.Scan(&id, &e1, &e2, &tc, &games, &started)
			tcDisplay := formatTimeControl(tc)
			if started > 0 {
				t := time.Unix(started, 0)
				elapsed := time.Since(t).Round(time.Second)
				tcDisplay = fmt.Sprintf("%s / %s", elapsed, tcDisplay)
			}
			startedDisplay := ""
			if started > 0 {
				startedDisplay = niceDuration(time.Unix(started, 0))
			}
			fmt.Fprintf(w, `<tr class="filter-row"><td>%d</td><td>%s</td><td>%s</td><td>%s</td><td>%d</td><td>%s</td></tr>`, id, htmlEscape(e1), htmlEscape(e2), htmlEscape(tcDisplay), games, htmlEscape(startedDisplay))
		}
	}
	if iRows == nil { io.WriteString(w, `<tr><td colspan="6">None</td></tr>`) }
	io.WriteString(w, "</table>")

	fmt.Fprintf(w, `<h2>Completed</h2><table><tr><th onclick="st(this.closest('table'),0,true)">ID</th><th onclick="st(this.closest('table'),1,false)">Black</th><th onclick="st(this.closest('table'),2,false)">White</th><th onclick="st(this.closest('table'),3,false)">Result</th><th onclick="st(this.closest('table'),4,true)">Score</th><th onclick="st(this.closest('table'),5,false)">Age</th><th onclick="st(this.closest('table'),6,false)">Opening</th></tr>`)
	gRows, _ := h.DB.Query(`SELECT g.id, (SELECT name FROM engines WHERE id=g.black_id), (SELECT name FROM engines WHERE id=g.white_id), g.result, COALESCE(g.final_score,0), COALESCE(g.opening_line,''), COALESCE(g.created_at,0) FROM games g ORDER BY g.id DESC LIMIT 100`)
	if gRows != nil {
		defer gRows.Close()
		for gRows.Next() {
			var id, s int; var blk, wht, r, o string; var created int64
			gRows.Scan(&id, &blk, &wht, &r, &s, &o, &created)
			age := ""
			if created > 0 {
				age = niceDuration(time.Unix(created, 0))
			}
			fmt.Fprintf(w, `<tr class="filter-row"><td><a href="/games/%d">%d</a></td><td>%s</td><td>%s</td><td>%s</td><td>%+d</td><td>%s</td><td>%s</td></tr>`,
				id, id, htmlEscape(blk), htmlEscape(wht), htmlEscape(r), s, htmlEscape(age), htmlEscape(o))
		}
	}
	io.WriteString(w, "</table>"+closing)
}
