package web

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (h *Handler) handleGames(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	open, closing := htmxWrap(r)
	io.WriteString(w, open+`<h1>Games</h1>`)

	fmt.Fprintf(w, `<h2>In Progress</h2><table><tr><th onclick="st(this.closest('table'),0,true)">ID</th><th onclick="st(this.closest('table'),1,false)">Black</th><th onclick="st(this.closest('table'),2,false)">White</th><th onclick="st(this.closest('table'),3,false)">Time</th><th onclick="st(this.closest('table'),4,true)">Games</th><th onclick="st(this.closest('table'),5,false)">Started</th></tr>`)
	// In-memory only: match_assignments table is transient — read from MM.
	if h.ActiveAssignmentsFunc != nil {
		for _, a := range h.ActiveAssignmentsFunc() {
			tcDisplay := formatTimeControl(a.TimeControl)
			elapsed := time.Since(a.InProgressAt).Round(time.Second)
			tcDisplay = fmt.Sprintf("%s / %s", elapsed, tcDisplay)
			startedDisplay := niceDuration(a.InProgressAt)
			fmt.Fprintf(w, `<tr class="filter-row"><td>%d</td><td>%s</td><td>%s</td><td>%s</td><td>%d</td><td>%s</td></tr>`,
				a.ID, htmlEscape(engineDisplayName(a.BlackEngine)), htmlEscape(engineDisplayName(a.WhiteEngine)), htmlEscape(tcDisplay), a.NumGames, htmlEscape(startedDisplay))
		}
	}
	if h.ActiveAssignmentsFunc == nil || len(h.ActiveAssignmentsFunc()) == 0 {
		io.WriteString(w, `<tr><td colspan="6">None</td></tr>`)
	}
	io.WriteString(w, "</table>")

	fmt.Fprintf(w, `<h2>Completed</h2><table><tr><th onclick="st(this.closest('table'),0,true)">ID</th><th onclick="st(this.closest('table'),1,false)">Black</th><th onclick="st(this.closest('table'),2,false)">White</th><th onclick="st(this.closest('table'),3,false)">Result</th><th onclick="st(this.closest('table'),4,true)">Score</th><th onclick="st(this.closest('table'),5,false)">Age</th><th onclick="st(this.closest('table'),6,false)">Opening</th></tr>`)
	gRows, _ := h.DB.Query(`SELECT g.id, (SELECT name FROM engines WHERE id=g.black_id), (SELECT name FROM engines WHERE id=g.white_id), g.result, COALESCE(g.final_score,0), COALESCE(g.opening_line,''), COALESCE(g.created_at, m.created_at, unixepoch()) FROM games g LEFT JOIN matches m ON m.id=g.match_id ORDER BY g.id DESC LIMIT 100`)
	if gRows != nil {
		defer gRows.Close()
		for gRows.Next() {
			var id, s int
			var blk, wht, r, o string
			var createdAny interface{}
			gRows.Scan(&id, &blk, &wht, &r, &s, &o, &createdAny)
			age := parseAge(createdAny)
			fmt.Fprintf(w, `<tr class="filter-row"><td><a href="/games/%d">%d</a></td><td>%s</td><td>%s</td><td>%s</td><td>%+d</td><td>%s</td><td>%s</td></tr>`,
				id, id, htmlEscape(blk), htmlEscape(wht), htmlEscape(r), s, htmlEscape(age), htmlEscape(o))
		}
	}
	io.WriteString(w, "</table>"+closing)
}

// parseAge converts a created_at value (int64, float64, or text datetime)
// into a human-readable duration string.
func parseAge(v interface{}) string {
	switch val := v.(type) {
	case int64:
		if val > 0 {
			return niceDuration(time.Unix(val, 0))
		}
	case float64:
		if val > 0 {
			return niceDuration(time.Unix(int64(val), 0))
		}
	case string:
		if val == "" {
			return "-"
		}
		// Try Unix timestamp string first (new format from migration)
		if unix, err := strconv.ParseInt(val, 10, 64); err == nil && unix > 0 {
			return niceDuration(time.Unix(unix, 0))
		}
		// Fall back to ISO datetime string (old format)
		if t, err := time.Parse("2006-01-02 15:04:05", val); err == nil {
			return niceDuration(t)
		}
	}
	return "-"
}

// engineDisplayName strips the ":version" suffix from an engine key.
// Keys are "name:version" — the name already contains the version info.
func engineDisplayName(key string) string {
	if idx := strings.LastIndex(key, ":"); idx > 0 {
		return key[:idx]
	}
	return key
}
