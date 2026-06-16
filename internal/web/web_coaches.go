package web

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

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
	io.WriteString(w, "</table>"+`</div>`+pageFoot)
}

