package web

import (
	"fmt"
	"io"
	"math"
	"net/http"
)

func (h *Handler) handleRanks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, pageHead+navHTML+`<div hx-get="." hx-trigger="every 30s" hx-swap="outerHTML">`+searchJS+`<h1>Player Rankings</h1>`+filterBox+`<table><tr><th onclick="st(this.closest('table'),0,true)">#</th><th onclick="st(this.closest('table'),1,false)">Player</th><th onclick="st(this.closest('table'),2,true)">Elo</th><th onclick="st(this.closest('table'),3,true)">+/-</th><th onclick="st(this.closest('table'),4,true)">Games</th><th onclick="st(this.closest('table'),5,false)">W/L/D</th><th onclick="st(this.closest('table'),6,false)">Trend</th></tr>`)
	rows, err := h.DB.Query(`SELECT e.id, e.name, e.version, COALESCE(e.engine_id,''), COALESCE((SELECT rating_after FROM elo_history WHERE engine_id=e.id ORDER BY created_at DESC LIMIT 1), 1500.0), (SELECT COUNT(*) FROM games WHERE black_id=e.id OR white_id=e.id) as g, (SELECT COUNT(*) FROM games WHERE (black_id=e.id AND result='1-0') OR (white_id=e.id AND result='0-1')), (SELECT COUNT(*) FROM games WHERE (black_id=e.id AND result='0-1') OR (white_id=e.id AND result='1-0')), (SELECT COUNT(*) FROM games WHERE (black_id=e.id OR white_id=e.id) AND result='1/2') FROM engines e ORDER BY 5 DESC`)
	if err != nil || rows == nil { io.WriteString(w, "</table>"); return }
	defer rows.Close()
	type eng struct{ Name, Version, EngineID string; ID int; Elo float64; Games, W, L, D int }
	var engines []eng
	for rows.Next() {
		var e eng; rows.Scan(&e.ID, &e.Name, &e.Version, &e.EngineID, &e.Elo, &e.Games, &e.W, &e.L, &e.D)
		engines = append(engines, e)
	}
	for i, e := range engines {
		ci := 400.0 / math.Sqrt(math.Max(float64(e.Games), 1))
		trend := "—"
		if e.Games >= 10 {
			var oldElo float64
			h.DB.QueryRow(`SELECT rating_before FROM elo_history WHERE engine_id=? ORDER BY created_at ASC LIMIT 1`, e.ID).Scan(&oldElo)
			if oldElo > 0 { if e.Elo > oldElo+10 { trend = "▲" } else if e.Elo < oldElo-10 { trend = "▼" } else { trend = "→" } }
		}
		var wr string
		if e.Games > 0 { wr = fmt.Sprintf("%d/%d/%d", e.W, e.L, e.D) } else { wr = "—" }
		fmt.Fprintf(w, `<tr class="filter-row"><td>%d</td><td><a href="/engines/%s">%s %s</a> <small style="color:var(--muted)">%s</small></td><td>%.0f</td><td>±%.0f</td><td>%d</td><td>%s</td><td>%s</td></tr>`, i+1, e.Name, e.Name, e.Version, e.EngineID[:min(8,len(e.EngineID))], e.Elo, ci, e.Games, wr, trend)
	}
	io.WriteString(w, "</table>"+`</div>`+pageFoot)
}

