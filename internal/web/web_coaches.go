package web

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

func (h *Handler) handleCoaches(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	open, closing := htmxWrap(r)
	io.WriteString(w, open)
	io.WriteString(w, `<h1>Coach Resources</h1>`)

	// Read from in-memory MatchMaker state (not DB — coaches table is transient).
	if h.CoachStatusFunc == nil {
		io.WriteString(w, "<p>No coaches online.</p>"+closing)
		return
	}
	coachStatuses := h.CoachStatusFunc()
	if len(coachStatuses) == 0 {
		io.WriteString(w, "<p>No coaches online.</p>"+closing)
		return
	}

	// Fetch observed stats from coach resource reports (in-memory window).
	observedByCoach := make(map[string]struct{ cpuSum, rssSum float64; count int })
	if h.ResourceStore != nil {
		for _, s := range h.ResourceStore.GetAll(120 * time.Second) {
			obs := observedByCoach[s.CoachID]
			obs.cpuSum += s.Interval.CPUPct.Avg * 100 // convert fraction→pct
			obs.rssSum += s.Interval.RSSMb.Avg
			obs.count++
			observedByCoach[s.CoachID] = obs
		}
	}

	totalCores, totalCoresUsed := 0, 0
	for _, c := range coachStatuses {
		totalCores += c.CoresTotal
		totalCoresUsed += c.CoresUsed
	}

	bar := func(used, total int) string {
		if total == 0 { return "0" }
		pct := used * 100 / total
		color := "#4caf50"; if pct > 80 { color = "#f44336" } else if pct > 60 { color = "#ff9800" }
		return fmt.Sprintf(`<div style="background:var(--border);border-radius:4px;height:16px;width:200px"><div style="background:%s;height:100%%;width:%d%%;border-radius:4px"></div></div><small>%d/%d (%d%%)</small>`, color, pct, used, total, pct)
	}
	io.WriteString(w, `<h2>Total</h2><table><tr><th>CPU</th><td>`+bar(totalCoresUsed, totalCores)+`</td></tr></table>`)
	io.WriteString(w, `<h2>Per Coach</h2><table><tr><th>Coach</th><th>CPU (declared)</th><th>CPU (observed)</th><th>Last Seen</th></tr>`)
	for _, c := range coachStatuses {
		lastSeen := niceDuration(c.LastSeen)
		obsCPU := "-"
		if o, ok := observedByCoach[c.ID]; ok && o.count > 0 {
			obsCPU = fmt.Sprintf("%.0f%%", o.cpuSum)
		}
		fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>",
			c.ID,
			bar(c.CoresUsed, c.CoresTotal),
			obsCPU,
			lastSeen)
	}
	io.WriteString(w, "</table>"+`</div>`+closing)
}
