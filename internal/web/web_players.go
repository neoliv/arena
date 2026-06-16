package web

import (
	"fmt"
	"io"
	"net/http"
)

func (h *Handler) handleEngine(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, pageHead+navHTML)
	fmt.Fprintf(w, "<h1>%s</h1>", htmlEscape(name))
	// Show manifest for the latest version
	var manifest string
	h.DB.QueryRow("SELECT COALESCE(engine_manifest,'') FROM engines WHERE name=? ORDER BY created_at DESC LIMIT 1", name).Scan(&manifest)
	if manifest != "" {
		fmt.Fprintf(w, "<pre style=\"background:var(--th-bg);padding:1em;border-radius:4px;font-size:.85em;max-height:400px;overflow:auto\">%s</pre>", htmlEscape(manifest))
	}
	var spark string
	eloRows, _ := h.DB.Query(`SELECT eh.rating_after FROM elo_history eh JOIN engines e2 ON eh.engine_id=e2.id WHERE e2.name=? ORDER BY eh.created_at`, name)
	if eloRows != nil { defer eloRows.Close(); var vals []float64; min, max := 4000.0, 0.0; for eloRows.Next() { var v float64; eloRows.Scan(&v); vals = append(vals, v); if v < min { min = v }; if v > max { max = v } }; if len(vals) > 0 && max > min { chars := "▁▂▃▄▅▆▇█"; for _, v := range vals { idx := int((v-min)/(max-min)*float64(len(chars)-1)); if idx < 0 { idx = 0 }; if idx >= len(chars) { idx = len(chars)-1 }; spark += string(chars[idx]) }; spark += fmt.Sprintf("  %.0f … %.0f", min, max) } }
	fmt.Fprintf(w, "<pre>%s</pre><h3>Recent Games</h3><table><tr><th>#</th><th>Opponent</th><th>Result</th><th>Score</th></tr>", spark)
	gameRows, _ := h.DB.Query(`SELECT g.id, CASE WHEN g.black_id IN (SELECT id FROM engines WHERE name=?) THEN ew.name ELSE eb.name END, CASE WHEN g.black_id IN (SELECT id FROM engines WHERE name=?) THEN g.result ELSE CASE g.result WHEN '1-0' THEN '0-1' WHEN '0-1' THEN '1-0' ELSE g.result END END, COALESCE(g.final_score,0) FROM games g JOIN engines eb ON g.black_id=eb.id JOIN engines ew ON g.white_id=ew.id WHERE eb.name=? OR ew.name=? ORDER BY g.id DESC LIMIT 30`, name, name, name, name)
	if gameRows != nil { defer gameRows.Close(); for gameRows.Next() { var id, s int; var opp, r string; if gameRows.Scan(&id, &opp, &r, &s) == nil { fmt.Fprintf(w, `<tr class="filter-row"><td><a href="/games/%d">%d</a></td><td>%s</td><td>%s</td><td>%+d</td></tr>`, id, id, opp, r, s) } } }
	io.WriteString(w, "</table>"+`</div>`+pageFoot)
}

func (h *Handler) handleVersions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, pageHead+navHTML+searchJS+`<h1>All Players</h1><p>An <strong>engine</strong> is a software build identified by content hash. A <strong>player</strong> is an engine with runtime arguments. Stats are tracked per time control. Click a column header to sort.</p>`+filterBox)
	type ver struct {
		Name, Version, Created, ChangelogShort, ChangelogFull, Budget, WR string
		Elo float64; Games, Wins, Losses, Draws int
	}
	rows, _ := h.DB.Query(`SELECT e.name, e.version, COALESCE(e.created, e.created_at), COALESCE(e.changelog_short,''), COALESCE(e.changelog_full,''), CASE WHEN e.version LIKE '%-%s' THEN SUBSTR(e.version, LENGTH(e.version)-2) ELSE '-' END as budget, COALESCE((SELECT rating_after FROM elo_history WHERE engine_id=e.id ORDER BY created_at DESC LIMIT 1), 1500.0), (SELECT COUNT(*) FROM games WHERE black_id=e.id OR white_id=e.id), (SELECT COUNT(*) FROM games WHERE (black_id=e.id AND result='1-0') OR (white_id=e.id AND result='0-1')), (SELECT COUNT(*) FROM games WHERE (black_id=e.id AND result='0-1') OR (white_id=e.id AND result='1-0')), (SELECT COUNT(*) FROM games WHERE (black_id=e.id OR white_id=e.id) AND result='1/2') FROM engines e ORDER BY COALESCE(e.created, e.created_at) DESC`)
	if rows == nil { io.WriteString(w, pageFoot); return }
	defer rows.Close()
	var versions []ver
	for rows.Next() {
		var v ver
		if rows.Scan(&v.Name, &v.Version, &v.Created, &v.ChangelogShort, &v.ChangelogFull, &v.Budget, &v.Elo, &v.Games, &v.Wins, &v.Losses, &v.Draws) != nil { continue }
		if v.Created == "" { v.Created = "unknown" }
		if v.Games > 0 { v.WR = fmt.Sprintf("%.1f%%", float64(v.Wins)/float64(v.Games)*100) } else { v.WR = "—" }
		versions = append(versions, v)
	}
	if len(versions) == 0 { io.WriteString(w, "<p>No players registered yet.</p>"+pageFoot); return }
	io.WriteString(w, `<table><tr><th onclick="st(this.parentElement.parentElement,0,false)" style="cursor:pointer">Engine</th><th onclick="st(this.parentElement.parentElement,1,false)" style="cursor:pointer">Version</th><th onclick="st(this.parentElement.parentElement,2,false)" style="cursor:pointer">Created</th><th onclick="st(this.parentElement.parentElement,3,false)" style="cursor:pointer">Budget</th><th onclick="st(this.parentElement.parentElement,4,true)" style="cursor:pointer">Elo</th><th onclick="st(this.parentElement.parentElement,5,true)" style="cursor:pointer">Games</th><th onclick="st(this.parentElement.parentElement,6,true)" style="cursor:pointer">W/L/D</th><th>Changes</th></tr>`)
	for i, v := range versions {
		shortID := fmt.Sprintf("cl-%d", i)
		fullID := fmt.Sprintf("fl-%d", i)
		changeCell := "—"
		if v.ChangelogShort != "" {
			changeCell = fmt.Sprintf(`<span id="%s">%s <a href="#" onclick="document.getElementById('%s').style.display='none';document.getElementById('%s').style.display='block';return false">[more]</a></span><span id="%s" style="display:none">%s <a href="#" onclick="document.getElementById('%s').style.display='block';document.getElementById('%s').style.display='none';return false">[less]</a></span>`, shortID, htmlEscape(v.ChangelogShort), shortID, fullID, fullID, htmlEscape(v.ChangelogFull), shortID, fullID)
		}
		fmt.Fprintf(w, `<tr class="filter-row"><td><a href="/engines/%s">%s</a></td><td>%s</td><td>%s</td><td>%s</td><td>%.0f</td><td>%d</td><td>%d/%d/%d</td><td>%s</td></tr>`, v.Name, v.Name, v.Version, v.Created, v.Budget, v.Elo, v.Games, v.Wins, v.Losses, v.Draws, changeCell)
	}
	io.WriteString(w, "</table>"+`</div>`+pageFoot)
}

