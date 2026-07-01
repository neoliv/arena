package web

import (
	"fmt"
	"io"
	"sort"
	"time"
	"net/http"
)

func (h *Handler) handleEngine(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	var manifest string
	h.DB.QueryRow("SELECT COALESCE(engine_manifest,'') FROM engines WHERE name=? ORDER BY created_at DESC LIMIT 1", name).Scan(&manifest)

	io.WriteString(w, pageHead+navHTML+fmt.Sprintf(`<h1>%s</h1>`, htmlEscape(name)))
	io.WriteString(w, fmt.Sprintf(`<pre style="font-size:.75em;line-height:1.4;max-width:100%%;overflow-x:auto;padding:1em;background:var(--bg2);border:1px solid var(--border);border-radius:4px">%s</pre>`, htmlEscape(manifest)))

	eloRows, _ := h.DB.Query(`SELECT eh.rating_after FROM elo_history eh JOIN engines e2 ON eh.engine_id=e2.id WHERE e2.name=? ORDER BY eh.created_at`, name)
	if eloRows != nil {
		defer eloRows.Close()
	}
	gameRows, _ := h.DB.Query(`SELECT g.id, CASE WHEN g.black_id IN (SELECT id FROM engines WHERE name=?) THEN ew.name ELSE eb.name END, CASE WHEN g.black_id IN (SELECT id FROM engines WHERE name=?) THEN g.result ELSE CASE g.result WHEN '1-0' THEN '0-1' WHEN '0-1' THEN '1-0' ELSE g.result END END, COALESCE(g.final_score,0) FROM games g JOIN engines eb ON g.black_id=eb.id JOIN engines ew ON g.white_id=ew.id WHERE eb.name=? OR ew.name=? ORDER BY g.id DESC LIMIT 30`, name, name, name, name)
	if gameRows != nil {
		defer gameRows.Close()
	}
	io.WriteString(w, pageFoot)
}

func (h *Handler) handleVersions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	open, closing := htmxWrap(r)
	io.WriteString(w, open)
	io.WriteString(w, `<h1>All Players</h1><p>An <strong>engine</strong> is a software build identified by content hash. A <strong>player</strong> is an engine with runtime arguments. Stats are tracked per time control. Click a column header to sort.</p>`+filterBox)

	// ── Resource Usage (real CPU/RAM from coaches, updated every ~20s) ────
	if h.ResourceStore != nil {
		stats := h.ResourceStore.GetAll(120 * time.Second)
		if len(stats) > 0 {
			// Sort by CPU avg descending
			sort.Slice(stats, func(i, j int) bool {
				return stats[i].Interval.CPUPct.Avg > stats[j].Interval.CPUPct.Avg
			})
			io.WriteString(w, `<h2>Resource Usage <span style="font-weight:normal;color:var(--muted);font-size:.85em">(real CPU / RAM, 20s window, refreshes every 30s)</span></h2>`)
			io.WriteString(w, `<table><tr><th>Player</th><th>CPU (20s | Session)</th><th>RAM (20s | Session)</th><th>Inst</th></tr>`)
			for _, s := range stats {
				engLink := `<a href="/engines/` + htmlEscape(s.Name) + `">` + htmlEscape(s.Name) + `</a>`
				cpuBar := resourceBar(s.Interval.CPUPct.Avg*100, s.Cumulative.CPUPct.Avg*100, true, 100)
				ramBar := resourceBar(s.Interval.RSSMb.Avg, s.Cumulative.RSSMb.Avg, false, s.MemoryMB)
				fmt.Fprintf(w, `<tr class="filter-row"><td>%s <small style="color:var(--muted)">%s</small></td><td>%s</td><td>%s</td><td>%d</td></tr>`,
					engLink, htmlEscape(s.Version), cpuBar, ramBar, s.Instances)
			}
			io.WriteString(w, `</table>`)
		}
	}

	// ── Registered engines (in-memory, live from coaches) ────
	if h.EngineStatusFunc != nil {
		statuses := h.EngineStatusFunc()
		fmt.Fprintf(w, `<h2>Currently Registered <span style="font-weight:normal;color:var(--muted);font-size:.85em">(%d)</span></h2>`, len(statuses))
		if len(statuses) > 0 {
			io.WriteString(w, `<table><tr><th>Engine</th><th>Version</th><th>Coach</th><th>Status</th></tr>`)
			for _, s := range statuses {
				badge := `<span style="color:#4caf50">● active</span>`
				if !s.Available {
					reason := s.UnavailableReason
					if reason == "" {
						reason = "unavailable"
					}
					badge = fmt.Sprintf(`<span style="color:#f44336;font-weight:600">● %s</span>`, htmlEscape(reason))
				}
				fmt.Fprintf(w, `<tr class="filter-row"><td><a href="/engines/%s">%s</a></td><td>%s</td><td>%s</td><td>%s</td></tr>`,
					htmlEscape(s.Name), htmlEscape(s.Name), htmlEscape(s.Version), htmlEscape(s.CoachID), badge)
			}
			io.WriteString(w, `</table>`)
		} else {
			io.WriteString(w, `<p style="color:var(--muted)">No coach-connected players — <code>coach-update.sh</code> may be needed.</p>`)
		}
	}
}

	type ver struct {
		Name, Version, Created, ChangelogShort, ChangelogFull, Budget, WR string
		Elo                                                              float64
		Games, Wins, Losses, Draws                                       int
	}
	rows, _ := h.DB.Query(`SELECT e.name, e.version, COALESCE(e.created, e.created_at), COALESCE(e.changelog_short,''), COALESCE(e.changelog_full,''), CASE WHEN e.version LIKE '%-%s' THEN SUBSTR(e.version, LENGTH(e.version)-2) ELSE '-' END as budget, COALESCE((SELECT rating_after FROM elo_history WHERE engine_id=e.id ORDER BY created_at DESC LIMIT 1), 1500.0), (SELECT COUNT(*) FROM games WHERE black_id=e.id OR white_id=e.id), (SELECT COUNT(*) FROM games WHERE (black_id=e.id AND result='1-0') OR (white_id=e.id AND result='0-1')), (SELECT COUNT(*) FROM games WHERE (black_id=e.id AND result='0-1') OR (white_id=e.id AND result='1-0')), (SELECT COUNT(*) FROM games WHERE (black_id=e.id OR white_id=e.id) AND result='1/2') FROM engines e ORDER BY COALESCE(e.created, e.created_at) DESC`)
	if rows == nil {
		io.WriteString(w, pageFoot)
		return
	}
	defer rows.Close()
	var versions []ver
	for rows.Next() {
		var v ver
		if rows.Scan(&v.Name, &v.Version, &v.Created, &v.ChangelogShort, &v.ChangelogFull, &v.Budget, &v.Elo, &v.Games, &v.Wins, &v.Losses, &v.Draws) != nil {
			continue
		}
		if v.Created == "" {
			v.Created = "unknown"
		}
		if v.Games > 0 {
			v.WR = fmt.Sprintf("%.1f%%", float64(v.Wins)/float64(v.Games)*100)
		} else {
			v.WR = "—"
		}
		versions = append(versions, v)
	}
	if len(versions) == 0 {
		io.WriteString(w, "<p>No players in database yet.</p>"+pageFoot)
		return
	}
	io.WriteString(w, `<h2>All Players (DB)</h2><table><tr><th onclick="st(this.parentElement.parentElement,0,false)" style="cursor:pointer">Engine</th><th onclick="st(this.parentElement.parentElement,1,false)" style="cursor:pointer">Version</th><th onclick="st(this.parentElement.parentElement,2,false)" style="cursor:pointer">Created</th><th onclick="st(this.parentElement.parentElement,3,false)" style="cursor:pointer">Budget</th><th onclick="st(this.parentElement.parentElement,4,true)" style="cursor:pointer">Elo</th><th onclick="st(this.parentElement.parentElement,5,true)" style="cursor:pointer">Games</th><th onclick="st(this.parentElement.parentElement,6,true)" style="cursor:pointer">W/L/D</th><th>Changes</th></tr>`)
	for i, v := range versions {
		shortID := fmt.Sprintf("cl-%d", i)
		fullID := fmt.Sprintf("fl-%d", i)
		changeCell := "—"
		if v.ChangelogShort != "" {
			changeCell = fmt.Sprintf(`<span id="%s">%s <a href="#" onclick="document.getElementById('%s').style.display='none';document.getElementById('%s').style.display='block';return false">[more]</a></span><span id="%s" style="display:none">%s <a href="#" onclick="document.getElementById('%s').style.display='block';document.getElementById('%s').style.display='none';return false">[less]</a></span>`, shortID, htmlEscape(v.ChangelogShort), shortID, fullID, fullID, htmlEscape(v.ChangelogFull), shortID, fullID)
		}
		fmt.Fprintf(w, `<tr class="filter-row"><td><a href="/engines/%s">%s</a></td><td>%s</td><td>%s</td><td>%s</td><td>%.0f</td><td>%d</td><td>%d/%d/%d</td><td>%s</td></tr>`, v.Name, v.Name, v.Version, v.Created, v.Budget, v.Elo, v.Games, v.Wins, v.Losses, v.Draws, changeCell)
	}
	io.WriteString(w, "</table>"+`</div>`+closing)
}

// resourceBar renders two stacked horizontal bars: interval (20s) on top,
// cumulative (since coach start) below in a lighter shade.
// For RAM, maxRef is the declared memory_mb from the player config.
func resourceBar(interval, cumulative float64, isCPU bool, maxRef int) string {
	max := 100.0
	if !isCPU {
		if maxRef > 0 { max = float64(maxRef) } else { max = 200.0 }
	}
	barColor := func(v float64) string {
		pct := v / max * 100.0
		if pct > 80 { return "#f44336" } else if pct > 60 { return "#ff9800" }
		return "#4caf50"
	}
	barWidth := func(v float64) string {
		w := v / max * 150
		if w > 150 { w = 150 }
		if w < 1 { w = 1 }
		return fmt.Sprintf("%.0f", w)
	}
	suffix := "%"
	if !isCPU { suffix = " MB" }
	ci, cc := barColor(interval), barColor(cumulative)
	wi, wc := barWidth(interval), barWidth(cumulative)
	return fmt.Sprintf(`<div style="width:160px;line-height:1.2">`+
		`<div style="background:var(--border);border-radius:2px;height:8px;margin-bottom:1px">`+
		`<div style="background:%s;height:100%%;width:%spx;border-radius:2px" title="20s: %.1f%s | %.0f"></div></div>`+
		`<div style="background:var(--border);border-radius:2px;height:6px;opacity:0.6">`+
		`<div style="background:%s;height:100%%;width:%spx;border-radius:2px" title="session: %.1f%s | %.0f"></div></div>`+
		`<small style="font-size:.75em;color:var(--muted)">%.0f | %.0f%s</small></div>`,
		ci, wi, interval, suffix, max,
		cc, wc, cumulative, suffix, max,
		interval, cumulative, suffix)
}
