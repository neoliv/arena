package web

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func (h *Handler) handleGames(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	open, closing := htmxWrap(r, ".")
	io.WriteString(w, open+`<h1>Games</h1>`)

	fmt.Fprintf(w, `<h2>In Progress</h2><table><tr><th onclick="st(this.closest('table'),0,true)">ID</th><th onclick="st(this.closest('table'),1,false)">Black</th><th onclick="st(this.closest('table'),2,false)">White</th><th onclick="st(this.closest('table'),3,false)">Time</th><th onclick="st(this.closest('table'),4,true)">Games</th><th onclick="st(this.closest('table'),5,false)">Started</th></tr>`)
	iRows, _ := h.DB.Query(`SELECT a.id, (SELECT name FROM engines WHERE id=a.engine1_id), (SELECT name FROM engines WHERE id=a.engine2_id), COALESCE(a.time_control,'{}'), a.num_games, COALESCE(a.in_progress_at, a.created_at) FROM match_assignments a WHERE a.status='in_progress' ORDER BY a.id DESC LIMIT 20`)
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
			fmt.Fprintf(w, `<tr class="filter-row"><td>%d</td><td>%s</td><td>%s</td><td>%s</td><td>%d</td><td>%s</td></tr>`, id, htmlEscape(e1), htmlEscape(e2), htmlEscape(tcDisplay), games, htmlEscape(startedDisplay))
		}
	}
	if iRows == nil { io.WriteString(w, `<tr><td colspan="6">None</td></tr>`) }
	io.WriteString(w, "</table>")

	fmt.Fprintf(w, `<h2>Completed</h2><table><tr><th onclick="st(this.closest('table'),0,true)">ID</th><th onclick="st(this.closest('table'),1,false)">Black</th><th onclick="st(this.closest('table'),2,false)">White</th><th onclick="st(this.closest('table'),3,false)">Result</th><th onclick="st(this.closest('table'),4,true)">Score</th><th onclick="st(this.closest('table'),5,false)">Opening</th></tr>`)
	gRows, _ := h.DB.Query(`SELECT g.id, (SELECT name FROM engines WHERE id=g.black_id), (SELECT name FROM engines WHERE id=g.white_id), g.result, COALESCE(g.final_score,0), COALESCE(g.opening_line,'') FROM games g ORDER BY g.id DESC LIMIT 100`)
	if gRows != nil { defer gRows.Close(); for gRows.Next() { var id, s int; var blk, wht, r, o string; gRows.Scan(&id, &blk, &wht, &r, &s, &o); fmt.Fprintf(w, `<tr class="filter-row"><td><a href="/games/%d">%d</a></td><td>%s</td><td>%s</td><td>%s</td><td>%+d</td><td>%s</td></tr>`, id, id, htmlEscape(blk), htmlEscape(wht), htmlEscape(r), s, htmlEscape(o)) } }
	io.WriteString(w, "</table>"+closing)
}

