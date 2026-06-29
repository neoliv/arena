package matchmaker

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/neoliv/arena/internal/coach"
	"github.com/neoliv/arena/internal/game"
)

var traceGameID atomic.Int64
func InitTrace(path string) error { return nil }

//
//go:embed openings_8ply.txt
var embeddedOpeningsBook string
var (openingsOnce sync.Once; openingsCache []string)

func loadOpenings() []string {
	openingsOnce.Do(func() {
		for _, line := range strings.Split(embeddedOpeningsBook, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") { continue }
			moves := parseMoveList(line)
			if len(moves) == 0 { continue }
			board := game.NewBoard()
			valid := true
			for i, mv := range moves {
				c := "b"; if i%2 == 1 { c = "w" }
				sq := game.SqFromString(mv)
				if sq < 0 { valid = false; break }
				player := board.Black(); if c == "w" { player = board.White() }
				if board.LegalMoves(player)&(1<<sq) == 0 { valid = false; break }
				board = board.ApplyMove(player, sq)
			}
			if valid { openingsCache = append(openingsCache, line) }
		}
		if len(openingsCache) == 0 { openingsCache = append(openingsCache, "") }
	})
	return openingsCache
}

type gameMove struct { Side, Move string; Nodes int64; Depth int; TimeMs float64; Score int }
type gameResult struct {
	Gid int64; Black, White, Result string; FinalScore int; OpeningLine string
	BlackTimeS, WhiteTimeS float64; BlackNodes, WhiteNodes int64
	Disconnect bool; ErrorCode int8; BlackDepth, WhiteDepth int; Moves []gameMove
}

func sendCmd(stream coach.Stream, cmd string, timeoutSec float64) (string, []string, error) {
	select {
	case stream.Out <- cmd:
	case <-time.After(time.Duration(timeoutSec) * time.Second):
		return "", nil, fmt.Errorf("write timeout: %s", cmd)
	}
	var comments []string
	for {
		select {
		case resp, ok := <-stream.In:
			if !ok { return "", comments, fmt.Errorf("stream closed") }
			s := strings.TrimSpace(resp)
			if s == "" { continue }
			if strings.HasPrefix(s, "#") { comments = append(comments, s); continue }
			if strings.HasPrefix(s, "=") || strings.HasPrefix(s, "?") { return s, comments, nil }
		case <-time.After(time.Duration(timeoutSec) * time.Second):
			return "", comments, fmt.Errorf("read timeout: %s", cmd)
		}
	}
}

func gtpLog(gid int64, side, dir, msg string) {
	if len(msg) > 200 { msg = msg[:200] + "..." }
	slog.Info("GTP", "gid", gid, "dir", dir, "side", side, "msg", msg)
}

func tracedSend(gid int64, side string, stream coach.Stream, cmd string, timeoutSec float64) (string, []string, error) {
	gtpLog(gid, side, ">>", cmd)
	resp, comments, err := sendCmd(stream, cmd, timeoutSec)
	if err != nil { gtpLog(gid, side, "<<", err.Error()) } else { gtpLog(gid, side, "<<", strings.TrimSpace(resp)) }
	return resp, comments, err
}

func parseStats(lines []string, fallbackMs float64) (nodes int64, depth int, score int, coachMs float64) {
	coachMs = fallbackMs
	for _, s := range lines {
		if strings.HasPrefix(s, "# time_ms ") {
			if fields := strings.Fields(s); len(fields) >= 2 { fmt.Sscanf(fields[1], "%f", &coachMs) }
			continue
		}
		if idx := strings.Index(s, "{"); idx >= 0 {
			var ns struct { Nodes int64 `json:"nodes"`; Depth int `json:"depth"`; Score int `json:"score"` }
			if json.Unmarshal([]byte(s[idx:]), &ns) == nil { nodes, depth, score = ns.Nodes, ns.Depth, ns.Score }
		}
	}
	return
}

func boardString(b game.Board) string {
	blk, wht := b.Black(), b.White()
	var buf [64]byte
	for sq := 0; sq < 64; sq++ {
		switch { case blk&(1<<sq) != 0: buf[sq] = 'B'; case wht&(1<<sq) != 0: buf[sq] = 'W'; default: buf[sq] = '.' }
	}
	return string(buf[:])
}

func playGames(ctx context.Context, black, white coach.Stream, numGames int, gameTimeSec float64, assignmentID int) []gameResult {
	openings := loadOpenings()
	var results []gameResult
	for i := 0; i < numGames; i++ {
		opening := openings[rand.Intn(len(openings))]
		if i%2 == 1 && i > 0 { opening = results[i-1].OpeningLine }
		var e1, e2 coach.Stream; bName, wName := "Black", "White"
		if i%2 == 1 { e1, e2 = white, black; bName, wName = wName, bName } else { e1, e2 = black, white }
		gr := playOneGame(ctx, e1, e2, opening, gameTimeSec, bName, wName, assignmentID, i)
		results = append(results, gr)
		if gr.Disconnect || (gr.ErrorCode != game.ErrNone && gr.ErrorCode != game.ErrResign) {
			slog.Warn("game error, stopping pair", "gid", gr.Gid, "assign", assignmentID, "game", i+1, "error_code", gr.ErrorCode)
			break
		}
	}
	return results
}

func playOneGame(ctx context.Context, black, white coach.Stream, opening string, gameTimeSec float64, bName, wName string, assignmentID, gameIdx int) gameResult {
	gid := traceGameID.Add(1)
	slog.Info("game start", "gid", gid, "black", bName, "white", wName)
	gr := gameResult{Gid: gid, Black: bName, White: wName, OpeningLine: opening}

	for _, s := range []coach.Stream{black, white} {
		if _, _, err := tracedSend(gid, "both", s, "boardsize 8", 10); err != nil { gr.Disconnect = true; return gr }
		if _, _, err := tracedSend(gid, "both", s, "clear_board", 10); err != nil { gr.Disconnect = true; return gr }
	}
	board := game.NewBoard()

	moves := parseMoveList(opening)
	for i, mv := range moves {
		color := "b"; if i%2 == 1 { color = "w" }
		sq := game.SqFromString(mv)
		player := board.Black(); if color == "w" { player = board.White() }
		if sq < 0 || board.LegalMoves(player)&(1<<sq) == 0 { gr.Disconnect = true; return gr }
		board = board.ApplyMove(player, sq)
		cmd := "play " + color + " " + mv
		for _, s := range []coach.Stream{black, white} {
			if resp, _, err := tracedSend(gid, "both", s, cmd, 10); err != nil || strings.HasPrefix(resp, "?") {
				gr.Disconnect = true; return gr
			}
		}
	}

	timeLimit := gameTimeSec * 1.05; pass, side := 0, "b"
	curPlayer, oppPlayer := board.Black(), board.White()
	if len(moves)%2 == 1 { side, curPlayer, oppPlayer = "w", oppPlayer, curPlayer }

	for {
		if (side == "b" && gr.BlackTimeS >= timeLimit) || (side == "w" && gr.WhiteTimeS >= timeLimit) {
			gr.ErrorCode = game.ErrTimeout
			if side == "b" { gr.Result = "0-1" } else { gr.Result = "1-0" }
			break
		}
		if board.IsOver() { break }

		legal := board.LegalMoves(curPlayer)
		current := black; if side == "w" { current = white }

		t0 := time.Now()
		resp, stats, err := tracedSend(gid, side, current, "genmove "+side, gameTimeSec)
		elapsed := time.Since(t0).Seconds()
		if side == "b" { gr.BlackTimeS += elapsed } else { gr.WhiteTimeS += elapsed }

		if err != nil {
			gr.ErrorCode = game.ErrCrash
			if strings.Contains(err.Error(), "stream closed") || strings.Contains(err.Error(), "write timeout") { gr.Disconnect = true }
			if side == "b" { gr.Result = "0-1" } else { gr.Result = "1-0" }
			break
		}

		mv := strings.TrimPrefix(resp, "= "); mv = strings.TrimPrefix(mv, "=")
		mv = strings.TrimSpace(mv)
		if fields := strings.Fields(mv); len(fields) > 0 { mv = fields[0] }
		mv = strings.ToUpper(mv)

		if mv == "" { gr.ErrorCode = game.ErrInvalidResponse; if side == "b" { gr.Result = "0-1" } else { gr.Result = "1-0" }; break }

		// Coach-induced timeout: "? timeout" → ErrTimeout (same as MM guard).
		if strings.Contains(resp, "timeout") {
			gr.ErrorCode = game.ErrTimeout
			if side == "b" { gr.Result = "0-1" } else { gr.Result = "1-0" }
			break
		}
		if mv == "RESIGN" || strings.HasPrefix(resp, "?") {
			gr.ErrorCode = game.ErrResign
			if side == "b" { gr.Result = "0-1" } else { gr.Result = "1-0" }
			break
		}

		if mv == "PASS" {
			if legal != 0 {
				slog.Error("illegal pass", "gid", gid, "side", side, "legal_count", game.Popcount(legal))
				gr.ErrorCode = game.ErrIllegalPass
				if side == "b" { gr.Result = "0-1" } else { gr.Result = "1-0" }
				break
			}
			opp := white; if side == "w" { opp = black }
			if resp, _, err := tracedSend(gid, "opp", opp, "play "+side+" pass", 10); err != nil {
				gr.ErrorCode = game.ErrCrash
				if side == "b" { gr.Result = "0-1" } else { gr.Result = "1-0" }
				break
			} else if strings.HasPrefix(resp, "?") {
				slog.Error("opponent rejected pass", "gid", gid, "resp", resp)
				gr.ErrorCode = game.ErrInvalidResponse
				if side == "b" { gr.Result = "0-1" } else { gr.Result = "1-0" }
				break
			}
			pass++; if pass == 2 { break }
			if side == "b" { side, curPlayer, oppPlayer = "w", board.White(), board.Black() } else { side, curPlayer, oppPlayer = "b", board.Black(), board.White() }
			continue
		}
		pass = 0
		if len(mv) != 2 || mv[0] < 'A' || mv[0] > 'H' || mv[1] < '1' || mv[1] > '8' {
			slog.Warn("invalid response format", "gid", gid, "side", side, "raw", resp)
			gr.ErrorCode = game.ErrInvalidResponse
			if side == "b" { gr.Result = "0-1" } else { gr.Result = "1-0" }
			break
		}
		sq := game.SqFromString(mv)

		if (legal>>sq)&1 == 0 {
			slog.Error("illegal move", "gid", gid, "side", side, "move", mv, "legal_count", game.Popcount(legal),
				"empties", 64-game.Popcount(board.Black()|board.White()),
				"black", fmt.Sprintf("%064b", board.Black()), "white", fmt.Sprintf("%064b", board.White()), "legal", fmt.Sprintf("%064b", legal))
			gr.ErrorCode = game.ErrIllegalMove
			if side == "b" { gr.Result = "0-1" } else { gr.Result = "1-0" }
			break
		}

		board = board.ApplyMove(curPlayer, sq)
		slog.Info("board", "gid", gid, "state", boardString(board))
		if board.IsOver() { break }

		opp := white; if side == "w" { opp = black }
			if resp, _, err := tracedSend(gid, "opp", opp, "play "+side+" "+mv, 10); err != nil {
				gr.ErrorCode = game.ErrCrash
				if side == "b" { gr.Result = "0-1" } else { gr.Result = "1-0" }
				break
			} else if strings.HasPrefix(resp, "?") {
				slog.Error("opponent rejected play", "gid", gid, "resp", resp)
				gr.ErrorCode = game.ErrInvalidResponse
				if side == "b" { gr.Result = "0-1" } else { gr.Result = "1-0" }
				break
			}

		nodes, depth, score, coachMs := parseStats(stats, elapsed*1000)
		gr.Moves = append(gr.Moves, gameMove{Side: side, Move: mv, Nodes: nodes, Depth: depth, TimeMs: coachMs, Score: score})

		if side == "b" { side, curPlayer, oppPlayer = "w", board.White(), board.Black() } else { side, curPlayer, oppPlayer = "b", board.Black(), board.White() }
		if len(gr.Moves) > 90 { break }
	}

	for _, m := range gr.Moves {
		if m.Side == "b" { gr.BlackNodes += m.Nodes; if m.Depth > gr.BlackDepth { gr.BlackDepth = m.Depth } } else { gr.WhiteNodes += m.Nodes; if m.Depth > gr.WhiteDepth { gr.WhiteDepth = m.Depth } }
	}

	bCount, wCount := game.Popcount(board.Black()), game.Popcount(board.White())
	if gr.Result == "" {
		if bCount > wCount { gr.Result = "1-0" } else if wCount > bCount { gr.Result = "0-1" } else { gr.Result = "1/2" }
	}
	if gr.ErrorCode == game.ErrTimeout || gr.ErrorCode == game.ErrResign {
		// Tournament forfeit rule: winner gets max(actual discs, 34), loser 30.
		if gr.Result == "1-0" {
			w := bCount; if w < 34 { w = 34 }
			gr.FinalScore = w - 30
		} else if gr.Result == "0-1" {
			w := wCount; if w < 34 { w = 34 }
			gr.FinalScore = w - 30
		}
	} else {
		// Natural ending: FFO convention (empties to winner). Board is full so empties=0.
		if bCount > wCount { gr.FinalScore = bCount - wCount } else if wCount > bCount { gr.FinalScore = wCount - bCount }
	}
	slog.Info("game result", "gid", gid, "assign", assignmentID, "game", gameIdx+1, "result", gr.Result, "score", gr.FinalScore, "moves", len(gr.Moves))
	return gr
}

func parseMoveList(line string) []string {
	line = strings.TrimSpace(line); if line == "" { return nil }
	var m []string
	for i := 0; i < len(line); i += 2 { if i+1 < len(line) { m = append(m, strings.ToUpper(line[i:i+2])) } }
	return m
}
