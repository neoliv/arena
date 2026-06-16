package matchmaker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/neoliv/arena/internal/coach"
)

type gameMove struct {
	Side      string
	Move      string
	Nodes     int64
	Depth     int
	TimeMs    float64 // wall-clock from arena genmove timing
	Branching int
}
type gameResult struct {
	Black        string
	White        string
	Result       string
	FinalScore   int
	OpeningLine  string
	BlackTimeS   float64
	WhiteTimeS   float64
	BlackNodes   int64
	WhiteNodes   int64
	BlackDepth   int
	WhiteDepth   int
	Moves        []gameMove
}

func wsSend(stream coach.Stream, cmd string) (string, error) {
	select {
	case stream.Out <- cmd:
	case <-time.After(5 * time.Second):
		return "", fmt.Errorf("write timeout: %s", cmd)
	}

	for {
		select {
		case resp, ok := <-stream.In:
			if !ok {
				return "", fmt.Errorf("stream closed")
			}
			if strings.TrimSpace(resp) != "" {
				return resp, nil
			}
		case <-time.After(5 * time.Second):
			return "", fmt.Errorf("read timeout: %s", cmd)
		}
	}
}

func playGames(ctx context.Context, black, white coach.Stream, numGames int, gameTimeSec float64) []gameResult {
	openings := defaultBook()
	var results []gameResult

	for i := 0; i < numGames; i++ {
		opening := openings[i%len(openings)]

		var e1, e2 coach.Stream
		bName, wName := "Black", "White"
		if i%2 == 1 {
			e1, e2 = white, black
			bName, wName = wName, bName
		} else {
			e1, e2 = black, white
		}

		gr := playOneGame(ctx, e1, e2, opening, gameTimeSec, bName, wName)
		results = append(results, gr)
	}
	return results
}

func playOneGame(ctx context.Context, black, white coach.Stream, opening string, gameTimeSec float64, bName, wName string) gameResult {
	gr := gameResult{Black: bName, White: wName, OpeningLine: opening}

	for _, s := range []coach.Stream{black, white} {
		if _, err := wsSend(s, "boardsize 8"); err != nil {
			slog.Error("init failed", "cmd", "boardsize 8", "err", err)
			gr.Result = "0-1"
			return gr
		}
		if _, err := wsSend(s, "clear_board"); err != nil {
			slog.Error("init failed", "cmd", "clear_board", "err", err)
			gr.Result = "0-1"
			return gr
		}
	}

	board := newBoard()

	moves := parseMoveList(opening)
	for i, mv := range moves {
		color := "b"
		if i%2 == 1 {
			color = "w"
		}
		cmd := "play " + color + " " + mv
		for _, s := range []coach.Stream{black, white} {
			resp, err := wsSend(s, cmd)
			if err != nil {
				slog.Error("opening play failed", "move", mv, "err", err)
				gr.Result = "0-1"
				return gr
			}
			if strings.HasPrefix(resp, "?") {
				slog.Warn("opening move rejected", "move", mv, "color", color, "response", resp)
				gr.Result = "0-1"
				return gr
			}
		}
		sq := sqFromString(mv)
		if sq >= 0 {
			player := board.black
			if color == "w" {
				player = board.white
			}
			board = board.applyMove(player, sq)
		}
	}

	consecutivePasses := 0
	timeLimit := gameTimeSec * 1.05

	sideToMove := "b"
	curPlayer := board.black
	oppPlayer := board.white
	if len(moves)%2 == 1 {
		sideToMove = "w"
		curPlayer, oppPlayer = oppPlayer, curPlayer
	}

	for {
		if gr.BlackTimeS >= timeLimit {
			gr.Result = "0-1"
			break
		}
		if gr.WhiteTimeS >= timeLimit {
			gr.Result = "1-0"
			break
		}

		if board.isOver() {
			break
		}

		legal := board.legalMoves(curPlayer)
		if legal == 0 {
			consecutivePasses++
			if consecutivePasses >= 2 {
				break
			}
			sideToMove, curPlayer, oppPlayer = flipSide(sideToMove, curPlayer, oppPlayer, board)
			continue
		}
		consecutivePasses = 0

		current := black
		if sideToMove == "w" {
			current = white
		}

		t0 := time.Now()
		resp, err := wsSend(current, "genmove "+sideToMove)
		elapsed := time.Since(t0).Seconds()
		if sideToMove == "b" {
			gr.BlackTimeS += elapsed
		} else {
			gr.WhiteTimeS += elapsed
		}

		if err != nil {
			slog.Error("genmove failed", "side", sideToMove, "err", err)
			if sideToMove == "b" {
				gr.Result = "0-1"
			} else {
				gr.Result = "1-0"
			}
			break
		}

		resp = strings.TrimSpace(strings.TrimPrefix(resp, "= "))
		parts := strings.Fields(resp)
		if len(parts) == 0 {
			slog.Warn("empty genmove response", "side", sideToMove)
			break
		}
		mv := strings.ToUpper(parts[0])

		if mv == "RESIGN" {
			if sideToMove == "b" {
				gr.Result = "0-1"
			} else {
				gr.Result = "1-0"
			}
			break
		}

		if mv == "PASS" {
			consecutivePasses++
			if consecutivePasses >= 2 {
				break
			}
			sideToMove, curPlayer, oppPlayer = flipSide(sideToMove, curPlayer, oppPlayer, board)
			continue
		}

		if len(mv) != 2 || mv[0] < 'A' || mv[0] > 'H' || mv[1] < '1' || mv[1] > '8' {
			slog.Warn("invalid genmove response", "side", sideToMove, "raw", resp)
			if sideToMove == "b" {
				gr.Result = "0-1"
			} else {
				gr.Result = "1-0"
			}
			break
		}
		sq := sqFromString(mv)
		if sq < 0 || (legal>>sq)&1 == 0 {
			slog.Warn("illegal move from engine", "side", sideToMove, "move", mv)
			if sideToMove == "b" {
				gr.Result = "0-1"
			} else {
				gr.Result = "1-0"
			}
			break
		}

		board = board.applyMove(curPlayer, sq)
		other := black
		if sideToMove == "w" {
			other = white
		}
		playResp, _ := wsSend(other, "play "+sideToMove+" "+mv)
		if strings.HasPrefix(playResp, "?") {
			slog.Warn("play rejected, ending game", "move", mv, "response", playResp)
			if sideToMove == "b" {
				gr.Result = "1-0"
			} else {
				gr.Result = "0-1"
			}
			break
		}

		// Read optional stats emitted by the engine after genmove.
		// Only nodes, depth, timeout, and branching are accepted.
		// Time is measured by the arena (wall-clock above).
		var nodes int64
		var depth int
		var timeout bool
		var branching int
		statsResp, _ := wsSend(current, "stats")
		statsResp = strings.TrimPrefix(strings.TrimSpace(statsResp), "= ")
		fmt.Sscanf(statsResp, "nodes %d depth %d timeout %t branching %d", &nodes, &depth, &timeout, &branching)

		elapsedMs := elapsed * 1000
		gr.Moves = append(gr.Moves, gameMove{
			Side: sideToMove, Move: mv,
			Nodes: nodes, Depth: depth, TimeMs: elapsedMs, Branching: branching,
		})

		sideToMove, curPlayer, oppPlayer = flipSide(sideToMove, curPlayer, oppPlayer, board)

		if len(gr.Moves) > 90 {
			break
		}
	}

	if gr.Result == "" {
		bCount := popcount(board.black)
		wCount := popcount(board.white)
		if bCount > wCount {
			gr.Result = "1-0"
			gr.FinalScore = bCount - wCount
		} else if wCount > bCount {
			gr.Result = "0-1"
			gr.FinalScore = wCount - bCount
		} else {
			gr.Result = "1/2"
		}
	}

	return gr
}

func flipSide(sideToMove string, curPlayer, oppPlayer uint64, board othelloBoard) (string, uint64, uint64) {
	if sideToMove == "b" {
		return "w", board.white, board.black
	}
	return "b", board.black, board.white
}

func popcount(x uint64) int {
	c := 0
	for x != 0 {
		x &= x - 1
		c++
	}
	return c
}

func parseMoveList(line string) []string {
	if line == "" {
		return nil
	}
	line = strings.TrimSpace(line)
	var m []string
	for i := 0; i < len(line); i += 2 {
		if i+1 < len(line) {
			m = append(m, strings.ToUpper(line[i:i+2]))
		}
	}
	return m
}

func defaultBook() []string {
	return []string{""}
}
