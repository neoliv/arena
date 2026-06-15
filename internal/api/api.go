// Package api implements the arena REST API handlers.
package api

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/neoliv/arena/internal/db"
	"github.com/neoliv/arena/internal/elo"
)

type Server struct {
	DB            *db.DB
	Token         string // master token (from env, may be empty)
	ValidateToken func(string) bool
}

func (s *Server) checkAuth(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	t := strings.TrimPrefix(auth, "Bearer ")
	if t == "" { return false }
	if s.ValidateToken != nil && s.ValidateToken(t) { return true }
	if s.Token != "" && t == s.Token { return true }
	return false
}

func (s *Server) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Token == "" && s.ValidateToken == nil {
			next(w, r)
			return
		}
		if !s.checkAuth(r) {
			jsonError(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}


// ── Engines ────────────────────────────────────────────────────────────────

type engineReq struct {
	Name, Version, GitCommit, GitRepo, Protocol, SubmittedBy string
}

func (s *Server) HandleRegisterEngine(w http.ResponseWriter, r *http.Request) {
	var req engineReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Version == "" {
		jsonError(w, "name and version required", http.StatusBadRequest)
		return
	}
	if req.Protocol == "" {
		req.Protocol = "gtp"
	}
	_, err := s.DB.Exec(`INSERT OR IGNORE INTO engines (name,version,git_commit,git_repo,protocol,submitted_by)
		VALUES (?,?,?,?,?,?)`, req.Name, req.Version, req.GitCommit, req.GitRepo, req.Protocol, req.SubmittedBy)
	if err != nil {
		slog.Error("register engine", "err", err)
		jsonError(w, "db error", http.StatusInternalServerError)
		return
	}
	// Get the ID (existing or new)
	var id int
	s.DB.QueryRow(`SELECT id FROM engines WHERE name=? AND version=?`, req.Name, req.Version).Scan(&id)
	jsonOK(w, map[string]any{"id": id, "status": "registered"})
}

func (s *Server) HandleListEngines(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(`SELECT e.id, e.name, e.version, e.git_commit, e.submitted_by,
		COALESCE((SELECT rating_after FROM elo_history WHERE engine_id=e.id ORDER BY created_at DESC LIMIT 1), 1500.0),
		COALESCE((SELECT games FROM elo_history WHERE engine_id=e.id ORDER BY created_at DESC LIMIT 1), 0)
		FROM engines e ORDER BY 6 DESC`)
	if err != nil {
		jsonError(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type out struct {
		ID int `json:"id"`; Name, Version, GitCommit, SubmittedBy string
		Elo float64 `json:"elo"`; Games int `json:"games"`
	}
	var list []out
	for rows.Next() {
		var o out
		rows.Scan(&o.ID, &o.Name, &o.Version, &o.GitCommit, &o.SubmittedBy, &o.Elo, &o.Games)
		list = append(list, o)
	}
	jsonOK(w, list)
}

func (s *Server) HandleGetEngine(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	var e struct {
		ID int `json:"id"`; Name, Version, GitCommit, GitRepo, Protocol, SubmittedBy, CreatedAt string
	}
	err := s.DB.QueryRow(`SELECT id,name,version,COALESCE(git_commit,''),COALESCE(git_repo,''),
		protocol,COALESCE(submitted_by,''),created_at FROM engines WHERE id=?`, id).
		Scan(&e.ID, &e.Name, &e.Version, &e.GitCommit, &e.GitRepo, &e.Protocol, &e.SubmittedBy, &e.CreatedAt)
	if err == sql.ErrNoRows {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	jsonOK(w, e)
}

// ── Matches ────────────────────────────────────────────────────────────────

type gameResult struct {
	Black, White, Result, OpeningLine, PGN string
	FinalScore                             int
	BlackTimeS, WhiteTimeS                 float64
	BlackNodes, WhiteNodes                 int64
	BlackDepth, WhiteDepth                 int
}

type matchReq struct {
	Engine1, Engine2 engineRef
	TimeControl      map[string]any
	OpeningBook      string
	RunnerID         string
	Games            []gameResult
}

type engineRef struct{ Name, Version string }

func (s *Server) HandleSubmitMatch(w http.ResponseWriter, r *http.Request) {
	var req matchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if len(req.Games) == 0 {
		jsonError(w, "no games", http.StatusBadRequest)
		return
	}
	e1ID, _ := s.resolveEngine(req.Engine1.Name, req.Engine1.Version)
	e2ID, _ := s.resolveEngine(req.Engine2.Name, req.Engine2.Version)

	tc, _ := json.Marshal(req.TimeControl)
	res, err := s.DB.Exec(`INSERT INTO matches (engine1_id,engine2_id,time_control,opening_book,runner_id,total_games)
		VALUES (?,?,?,?,?,?)`, e1ID, e2ID, tc, req.OpeningBook, req.RunnerID, len(req.Games))
	if err != nil {
		jsonError(w, "create match: "+err.Error(), http.StatusInternalServerError)
		return
	}
	matchID, _ := res.LastInsertId()

	wins1, wins2, draws := 0, 0, 0
	for i, g := range req.Games {
		blkID, _ := s.resolveEngine(g.Black, "")
		whtID, _ := s.resolveEngine(g.White, "")
		if blkID == 0 { blkID = int(e1ID) }
		if whtID == 0 { whtID = int(e2ID) }
		s.DB.Exec(`INSERT INTO games (match_id,game_number,black_id,white_id,result,final_score,opening_line,pgn,
			black_time_s,white_time_s,black_nodes,white_nodes,black_depth,white_depth)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			matchID, i+1, blkID, whtID, g.Result, g.FinalScore, g.OpeningLine, g.PGN,
			g.BlackTimeS, g.WhiteTimeS, g.BlackNodes, g.WhiteNodes, g.BlackDepth, g.WhiteDepth)
		switch g.Result {
		case "1-0":
			if blkID == int(e1ID) { wins1++ } else { wins2++ }
		case "0-1":
			if whtID == int(e1ID) { wins1++ } else { wins2++ }
		case "1/2":
			draws++
		}
	}
	s.DB.Exec(`UPDATE matches SET wins_1=?, wins_2=?, draws=? WHERE id=?`, wins1, wins2, draws, matchID)
	s.RecomputeElo(int(e1ID))
	s.RecomputeElo(int(e2ID))

	jsonOK(w, map[string]any{"match_id": matchID, "games": len(req.Games),
		"wins_1": wins1, "wins_2": wins2, "draws": draws, "status": "recorded"})
}

func (s *Server) HandleListMatches(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, _ := strconv.Atoi(l); n > 0 && n <= 200 { limit = n }
	}
	rows, _ := s.DB.Query(`SELECT m.id, e1.name||' '||e1.version, e2.name||' '||e2.version,
		m.total_games, m.wins_1, m.wins_2, m.draws, m.created_at
		FROM matches m JOIN engines e1 ON m.engine1_id=e1.id JOIN engines e2 ON m.engine2_id=e2.id
		ORDER BY m.created_at DESC LIMIT ?`, limit)
	defer rows.Close()
	type out struct {
		ID, TotalGames, Wins1, Wins2, Draws int
		E1, E2, CreatedAt                   string
	}
	var list []out
	for rows.Next() {
		var o out
		rows.Scan(&o.ID, &o.E1, &o.E2, &o.TotalGames, &o.Wins1, &o.Wins2, &o.Draws, &o.CreatedAt)
		list = append(list, o)
	}
	jsonOK(w, list)
}

func (s *Server) HandleGetMatch(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	var m struct {
		ID, TotalGames, Wins1, Wins2, Draws int
		E1, E2, CreatedAt                   string
	}
	err := s.DB.QueryRow(`SELECT m.id, e1.name||' '||e1.version, e2.name||' '||e2.version,
		m.total_games, m.wins_1, m.wins_2, m.draws, m.created_at
		FROM matches m JOIN engines e1 ON m.engine1_id=e1.id JOIN engines e2 ON m.engine2_id=e2.id
		WHERE m.id=?`, id).Scan(&m.ID, &m.E1, &m.E2, &m.TotalGames, &m.Wins1, &m.Wins2, &m.Draws, &m.CreatedAt)
	if err != nil {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	rows, _ := s.DB.Query(`SELECT g.game_number, eb.name||' '||eb.version, ew.name||' '||ew.version,
		g.result, g.final_score, COALESCE(g.opening_line,''), g.pgn
		FROM games g JOIN engines eb ON g.black_id=eb.id JOIN engines ew ON g.white_id=ew.id
		WHERE g.match_id=? ORDER BY g.game_number`, id)
	defer rows.Close()
	type gOut struct {
		Number, FinalScore int; Black, White, Result, OpeningLine, PGN string
	}
	var games []gOut
	for rows.Next() {
		var g gOut
		rows.Scan(&g.Number, &g.Black, &g.White, &g.Result, &g.FinalScore, &g.OpeningLine, &g.PGN)
		games = append(games, g)
	}
	jsonOK(w, map[string]any{"match": m, "games": games})
}

// ── Elo ────────────────────────────────────────────────────────────────────

func (s *Server) HandleGetElo(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(`SELECT e.id, e.name, e.version,
		COALESCE((SELECT rating_after FROM elo_history WHERE engine_id=e.id ORDER BY created_at DESC LIMIT 1), 1500.0),
		COALESCE((SELECT COUNT(*) FROM elo_history WHERE engine_id=e.id), 0),
		COALESCE((SELECT COUNT(*) FROM elo_history WHERE engine_id=e.id AND wins>0), 0),
		COALESCE((SELECT COUNT(*) FROM elo_history WHERE engine_id=e.id AND losses>0), 0),
		COALESCE((SELECT COUNT(*) FROM elo_history WHERE engine_id=e.id AND draws>0), 0)
		FROM engines e ORDER BY 4 DESC`)
	if err != nil {
		jsonError(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var ratings []elo.Rating
	for rows.Next() {
		var r elo.Rating
		rows.Scan(&r.EngineID, &r.EngineName, &r.Version, &r.Elo, &r.Games, &r.Wins, &r.Losses, &r.Draws)
		ratings = append(ratings, r)
	}
	jsonOK(w, ratings)
}

func (s *Server) HandleGetEngineElo(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	rows, _ := s.DB.Query(`SELECT id,engine_id,opponent_id,match_id,rating_before,rating_after,games,wins,losses,draws
		FROM elo_history WHERE engine_id=? ORDER BY created_at`, id)
	defer rows.Close()
	var h []elo.HistoryEntry
	for rows.Next() {
		var e elo.HistoryEntry
		rows.Scan(&e.ID, &e.EngineID, &e.OpponentID, &e.MatchID, &e.RatingBefore, &e.RatingAfter, &e.Games, &e.Wins, &e.Losses, &e.Draws)
		h = append(h, e)
	}
	jsonOK(w, h)
}

// ── Version history ────────────────────────────────────────────────────────

func (s *Server) HandleVersionHistory(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rows, _ := s.DB.Query(`SELECT id, version, COALESCE(git_commit,''), created_at
		FROM engines WHERE name=? ORDER BY created_at`, name)
	defer rows.Close()
	type ev struct{ ID int; Version, GitCommit, CreatedAt string }
	var vers []ev
	for rows.Next() {
		var v ev; rows.Scan(&v.ID, &v.Version, &v.GitCommit, &v.CreatedAt); vers = append(vers, v)
	}
	type oppOut struct{ Name, Version string; Games, Wins, Losses int; WinRate float64 }
	type vp struct {
		Version, GitCommit, Date string; Elo float64
		Games, Wins, Losses, Draws int; Opponents []oppOut
	}
	var result []vp
	for _, v := range vers {
		p := vp{Version: v.Version, GitCommit: v.GitCommit, Date: v.CreatedAt, Elo: 1500}
		s.DB.QueryRow(`SELECT COALESCE((SELECT rating_after FROM elo_history WHERE engine_id=? ORDER BY created_at DESC LIMIT 1),1500.0)`, v.ID).Scan(&p.Elo)
		s.DB.QueryRow(`SELECT COUNT(*) FROM games WHERE black_id=? OR white_id=?`, v.ID, v.ID).Scan(&p.Games)
		s.DB.QueryRow(`SELECT COUNT(*) FROM games WHERE (black_id=? AND result='1-0') OR (white_id=? AND result='0-1')`, v.ID, v.ID).Scan(&p.Wins)
		s.DB.QueryRow(`SELECT COUNT(*) FROM games WHERE (black_id=? AND result='0-1') OR (white_id=? AND result='1-0')`, v.ID, v.ID).Scan(&p.Losses)
		s.DB.QueryRow(`SELECT COUNT(*) FROM games WHERE (black_id=? OR white_id=?) AND result='1/2'`, v.ID, v.ID).Scan(&p.Draws)
		orows, _ := s.DB.Query(`SELECT opp.name, opp.version,
			COUNT(*) AS g,
			SUM(CASE WHEN (g.black_id=? AND g.result='1-0') OR (g.white_id=? AND g.result='0-1') THEN 1 ELSE 0 END) AS w,
			SUM(CASE WHEN (g.black_id=? AND g.result='0-1') OR (g.white_id=? AND g.result='1-0') THEN 1 ELSE 0 END) AS l
			FROM games g
			JOIN engines opp ON opp.id = CASE WHEN g.black_id=? THEN g.white_id ELSE g.black_id END
			WHERE g.black_id=? OR g.white_id=?
			GROUP BY opp.name, opp.version ORDER BY g DESC`,
			v.ID, v.ID, v.ID, v.ID, v.ID, v.ID, v.ID)
		for orows.Next() {
			var o oppOut; orows.Scan(&o.Name, &o.Version, &o.Games, &o.Wins, &o.Losses)
			if o.Games > 0 { o.WinRate = float64(o.Wins) / float64(o.Games) * 100 }
			p.Opponents = append(p.Opponents, o)
		}
		orows.Close()
		result = append(result, p)
	}
	jsonOK(w, result)
}

// ── Games ──────────────────────────────────────────────────────────────────

func (s *Server) HandleListGames(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, _ := strconv.Atoi(l); n > 0 && n <= 200 { limit = n }
	}
	black := r.URL.Query().Get("black")
	white := r.URL.Query().Get("white")
	res := r.URL.Query().Get("result")
	query := `SELECT g.id, eb.name||' '||eb.version, ew.name||' '||ew.version,
		g.result, g.final_score, COALESCE(g.opening_line,''), g.game_number, g.created_at
		FROM games g JOIN engines eb ON g.black_id=eb.id JOIN engines ew ON g.white_id=ew.id WHERE 1=1`
	var args []any
	if black != "" { query += ` AND eb.name=?`; args = append(args, black) }
	if white != "" { query += ` AND ew.name=?`; args = append(args, white) }
	if res != ""   { query += ` AND g.result=?`; args = append(args, res) }
	query += ` ORDER BY g.created_at DESC LIMIT ?`; args = append(args, limit)
	rows, _ := s.DB.Query(query, args...)
	defer rows.Close()
	type out struct{ ID, FinalScore, GameNumber int; Black, White, Result, OpeningLine, CreatedAt string }
	var list []out
	for rows.Next() {
		var o out
		rows.Scan(&o.ID, &o.Black, &o.White, &o.Result, &o.FinalScore, &o.OpeningLine, &o.GameNumber, &o.CreatedAt)
		list = append(list, o)
	}
	jsonOK(w, list)
}

// ── Bisection ──────────────────────────────────────────────────────────────

func (s *Server) HandleCreateBisect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EngineName             string    `json:"engine_name"`
		GoodCommit  string `json:"good_commit"`
	BadCommit   string `json:"bad_commit"`
		RefEngine              engineRef `json:"ref_engine"`
		GamesPerStep           int       `json:"games_per_step"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.GamesPerStep == 0 { req.GamesPerStep = 100 }
	refID, _ := s.resolveEngine(req.RefEngine.Name, req.RefEngine.Version)
	bres, _ := s.DB.Exec(`INSERT INTO bisections (engine_name,good_commit,bad_commit,ref_engine_id,games_per_step,status,current_good,current_bad)
		VALUES (?,?,?,?,?,'pending',?,?)`, req.EngineName, req.GoodCommit, req.BadCommit, refID, req.GamesPerStep, req.GoodCommit, req.BadCommit)
	id, _ := bres.LastInsertId()
	jsonOK(w, map[string]any{"id": id, "status": "created"})
}

func (s *Server) HandleGetBisect(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	var b struct {
		ID, GamesPerStep                                                  int
		EngineName, GoodCommit, BadCommit, RefEngine, Status, CreatedAt string
		CurrentGood, CurrentBad                                          string
	}
	s.DB.QueryRow(`SELECT b.id,b.engine_name,b.good_commit,b.bad_commit,e.name||' '||e.version,
		b.games_per_step,b.status,COALESCE(b.current_good,''),COALESCE(b.current_bad,''),b.created_at
		FROM bisections b JOIN engines e ON b.ref_engine_id=e.id WHERE b.id=?`, id).
		Scan(&b.ID, &b.EngineName, &b.GoodCommit, &b.BadCommit, &b.RefEngine,
			&b.GamesPerStep, &b.Status, &b.CurrentGood, &b.CurrentBad, &b.CreatedAt)
	rows, _ := s.DB.Query(`SELECT commit_hash,elo_result,verdict,games_played,created_at FROM bisect_steps WHERE bisection_id=? ORDER BY id`, id)
	defer rows.Close()
	type step struct{ Commit, Verdict, CreatedAt string; EloResult *float64; GamesPlayed int }
	var steps []step
	for rows.Next() {
		var s2 step; rows.Scan(&s2.Commit, &s2.EloResult, &s2.Verdict, &s2.GamesPlayed, &s2.CreatedAt)
		steps = append(steps, s2)
	}
	jsonOK(w, map[string]any{"bisection": b, "steps": steps})
}

func (s *Server) HandleBisectNext(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	var good, bad, status string
	s.DB.QueryRow(`SELECT current_good,current_bad,status FROM bisections WHERE id=?`, id).Scan(&good, &bad, &status)
	if status == "done" {
		jsonOK(w, map[string]string{"status": "done", "message": "bisection complete: " + bad})
		return
	}
	s.DB.Exec(`UPDATE bisections SET status='running' WHERE id=?`, id)
	jsonOK(w, map[string]string{"status": "in_progress", "good_commit": good, "bad_commit": bad})
}

func (s *Server) HandleBisectResult(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	var req struct{ Commit string; EloResult float64; Verdict string; GamesPlayed int }
	json.NewDecoder(r.Body).Decode(&req)
	s.DB.Exec(`INSERT INTO bisect_steps (bisection_id,commit_hash,elo_result,verdict,games_played) VALUES (?,?,?,?,?)`,
		id, req.Commit, req.EloResult, req.Verdict, req.GamesPlayed)
	if req.Verdict == "good" {
		s.DB.Exec(`UPDATE bisections SET current_good=? WHERE id=?`, req.Commit, id)
	} else if req.Verdict == "bad" {
		s.DB.Exec(`UPDATE bisections SET current_bad=? WHERE id=?`, req.Commit, id)
	} else if req.Verdict == "found" {
		s.DB.Exec(`UPDATE bisections SET status='done', finished_at=datetime('now') WHERE id=?`, id)
	}
	jsonOK(w, map[string]string{"status": "recorded"})
}

// ── Elo recomputation ──────────────────────────────────────────────────────

func (s *Server) RecomputeElo(engineID int) {
	rows, _ := s.DB.Query(`SELECT g.id,g.black_id,g.white_id,g.result,g.match_id,eb.name,ew.name
		FROM games g JOIN engines eb ON g.black_id=eb.id JOIN engines ew ON g.white_id=ew.id
		WHERE g.black_id=? OR g.white_id=? ORDER BY g.created_at, g.id`, engineID, engineID)
	defer rows.Close()
	type gr struct{ ID, BlackID, WhiteID, MatchID int; Result, BlackName, WhiteName string }
	var games []gr
	for rows.Next() { var g gr; rows.Scan(&g.ID, &g.BlackID, &g.WhiteID, &g.Result, &g.MatchID, &g.BlackName, &g.WhiteName); games = append(games, g) }
	if len(games) == 0 { return }

	s.DB.Exec(`DELETE FROM elo_history WHERE engine_id=?`, engineID)
	ratings := map[int]float64{engineID: 1500}
	gp := map[int]int{engineID: 0}
	for _, g := range games {
		oppID := g.BlackID
		if engineID == g.BlackID { oppID = g.WhiteID }
		if _, ok := ratings[oppID]; !ok { ratings[oppID] = 1500; gp[oppID] = 0 }
		rA, rB := ratings[engineID], ratings[oppID]
		var sA float64
		if engineID == g.BlackID {
			switch g.Result { case "1-0": sA = 1; case "0-1": sA = 0; default: sA = 0.5 }
		} else {
			switch g.Result { case "0-1": sA = 1; case "1-0": sA = 0; default: sA = 0.5 }
		}
		nA, nB := elo.Update(rA, rB, sA, gp[engineID])
		var w, l, d int
		switch sA { case 1: w = 1; case 0: l = 1; default: d = 1 }
		s.DB.Exec(`INSERT INTO elo_history (engine_id,opponent_id,match_id,rating_before,rating_after,games,wins,losses,draws)
			VALUES (?,?,?,?,?,1,?,?,?)`, engineID, oppID, g.MatchID, rA, nA, w, l, d)
		ratings[engineID] = nA; ratings[oppID] = nB
		gp[engineID]++; gp[oppID]++
	}
}

// ── Helpers ────────────────────────────────────────────────────────────────

func (s *Server) resolveEngine(name, version string) (int, error) {
	if version == "" {
		var id int
		err := s.DB.QueryRow(`SELECT id FROM engines WHERE name=? ORDER BY created_at DESC LIMIT 1`, name).Scan(&id)
		return id, err
	}
	var id int
	err := s.DB.QueryRow(`SELECT id FROM engines WHERE name=? AND version=?`, name, version).Scan(&id)
	if err == sql.ErrNoRows {
		r, err2 := s.DB.Exec(`INSERT OR IGNORE INTO engines (name,version) VALUES (?,?)`, name, version)
		if err2 != nil { return 0, err2 }
		i, _ := r.LastInsertId()
		if i > 0 { id = int(i) } else {
			s.DB.QueryRow(`SELECT id FROM engines WHERE name=? AND version=?`, name, version).Scan(&id)
		}
	}
	return id, err
}

// ── Speed stats ────────────────────────────────────────────────────────────

func (s *Server) HandleSubmitSpeed(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Engine1 engineRef `json:"engine1"`
		Engine2 engineRef `json:"engine2"`
		MatchID int       `json:"match_id"`
		Moves   []struct {
			Ply        int     `json:"ply"`
			Color      string  `json:"color"`
			Nodes      int64   `json:"nodes"`
			TimeS      float64 `json:"time_s"`
			Depth      int     `json:"depth"`
			Timeout    bool    `json:"timeout"`
			Score      int     `json:"score"`
			NPS        int64   `json:"nps"`
			Branching  int     `json:"branching"`
			Empties    int     `json:"empties"`
			EngineName string  `json:"engine_name"`
		} `json:"moves"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid", 400)
		return
	}
	e1ID, _ := s.resolveEngine(req.Engine1.Name, req.Engine1.Version)
	e2ID, _ := s.resolveEngine(req.Engine2.Name, req.Engine2.Version)

	for _, m := range req.Moves {
		eID := e1ID
		if m.EngineName == req.Engine2.Name {
			eID = e2ID
		}
		if eID == 0 { continue }
		timeoutInt := 0; if m.Timeout { timeoutInt = 1 }
		s.DB.Exec(`INSERT INTO speed_stats (engine_id, match_id, ply, total_nodes, total_time_s, total_depth, timeouts, total_nps, total_branch, total_empties, sample_count)
			VALUES (?,?,?,?,?,?,?,?,?,?,1)`, eID, req.MatchID, m.Ply, m.Nodes, m.TimeS, m.Depth, timeoutInt, m.NPS, m.Branching, m.Empties)
	}
	jsonOK(w, map[string]string{"status": "ok"})
}

func (s *Server) HandleGetSpeed(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rows, _ := s.DB.Query(`SELECT ply, SUM(total_nodes), SUM(total_time_s), SUM(sample_count),
		SUM(total_depth), SUM(timeouts), SUM(total_nps)
		FROM speed_stats WHERE engine_id=? GROUP BY ply ORDER BY ply`, id)
	defer rows.Close()
	type pt struct {
		Ply int `json:"ply"`; Nodes int64 `json:"nodes"`; TimeS float64 `json:"time_s"`; Samples int `json:"samples"`
		AvgDepth float64 `json:"avg_depth"`; Timeouts int `json:"timeouts"`; AvgNPS float64 `json:"nps"`
	}
	var pts []pt
	for rows.Next() {
		var p pt; var depthSum, timeouts int; var npsSum int64
		rows.Scan(&p.Ply, &p.Nodes, &p.TimeS, &p.Samples, &depthSum, &timeouts, &npsSum)
		if p.TimeS > 0 { p.AvgNPS = float64(p.Nodes) / p.TimeS }
		if p.Samples > 0 { p.AvgDepth = float64(depthSum) / float64(p.Samples) }
		p.Timeouts = timeouts
		pts = append(pts, p)
	}
	jsonOK(w, pts)
}

func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	// All API endpoints require authentication.
	// When no token is configured (Token="" and ValidateToken=nil), auth is open for local dev.
	mux.HandleFunc("POST /api/engines", s.requireToken(s.HandleRegisterEngine))
	mux.HandleFunc("GET /api/engines", s.requireToken(s.HandleListEngines))
	mux.HandleFunc("GET /api/engines/{id}", s.requireToken(s.HandleGetEngine))
	mux.HandleFunc("POST /api/matches", s.requireToken(s.HandleSubmitMatch))
	mux.HandleFunc("GET /api/matches", s.requireToken(s.HandleListMatches))
	mux.HandleFunc("GET /api/matches/{id}", s.requireToken(s.HandleGetMatch))
	mux.HandleFunc("GET /api/elo", s.requireToken(s.HandleGetElo))
	mux.HandleFunc("GET /api/elo/{id}", s.requireToken(s.HandleGetEngineElo))
	mux.HandleFunc("GET /api/versions/{name}", s.requireToken(s.HandleVersionHistory))
	mux.HandleFunc("GET /api/games", s.requireToken(s.HandleListGames))
	mux.HandleFunc("POST /api/speed", s.requireToken(s.HandleSubmitSpeed))
	mux.HandleFunc("GET /api/speed/{id}", s.requireToken(s.HandleGetSpeed))
	mux.HandleFunc("POST /api/bisect", s.requireToken(s.HandleCreateBisect))
	mux.HandleFunc("GET /api/bisect/{id}", s.requireToken(s.HandleGetBisect))
	mux.HandleFunc("GET /api/bisect/{id}/next", s.requireToken(s.HandleBisectNext))
	mux.HandleFunc("POST /api/bisect/{id}/result", s.requireToken(s.HandleBisectResult))
}
