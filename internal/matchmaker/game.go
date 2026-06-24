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
	"time"

	"github.com/neoliv/arena/internal/coach"
)

// ── Embedded opening book ──────────────────────────────────────────────

// 48 balanced 8-ply openings extracted from Othello opening theory.
// All lines have 45-55% win rates for both colors in tournament play.
//
//go:embed openings_8ply.txt
var embeddedOpeningsBook string

var (
	openingsCache []string
	openingsOnce  sync.Once
)

// loadOpenings parses the embedded opening book into lines, filtering
// comments and empty lines. Cached after first call.
func loadOpenings() []string {
	openingsOnce.Do(func() {
		for _, line := range strings.Split(embeddedOpeningsBook, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			openingsCache = append(openingsCache, line)
		}
		if len(openingsCache) == 0 {
			// Fallback: empty opening (start from initial position)
			openingsCache = append(openingsCache, "")
		}
	})
	return openingsCache
}

// ── Game structures ────────────────────────────────────────────────────

type gameMove struct {
	Side      string
	Move      string
	Nodes     int64
	Depth     int
	TimeMs    float64
	Score     int
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
	Disconnect   bool // stream/timeout error, not a real game
	BlackDepth   int
	WhiteDepth   int
	Moves        []gameMove
}

// ── WebSocket helpers ──────────────────────────────────────────────────

func wsSend(stream coach.Stream, cmd string, timeoutSec float64) (string, error) {
	select {
	case stream.Out <- cmd:
	case <-time.After(time.Duration(timeoutSec) * time.Second):
		return "", fmt.Errorf("write timeout: %s", cmd)
	}

	for {
		select {
		case resp, ok := <-stream.In:
			if !ok {
				return "", fmt.Errorf("stream closed")
			}
			s := strings.TrimSpace(resp)
			if s != "" && !strings.HasPrefix(s, "#") {
				return resp, nil
			}
		case <-time.After(time.Duration(timeoutSec) * time.Second):
			return "", fmt.Errorf("read timeout: %s", cmd)
		}
	}
}

// ── Game execution ─────────────────────────────────────────────────────

func playGames(ctx context.Context, black, white coach.Stream, numGames int, gameTimeSec float64, assignmentID int) []gameResult {
	openings := loadOpenings()
	var results []gameResult

	for i := 0; i < numGames; i++ {
		// Per PAIR (every 2 games): pick a random opening line.
		// Both games in the pair use the SAME line with colors swapped
		// so each engine plays both sides of the identical position.
		opening := openings[rand.Intn(len(openings))]
		if i%2 == 1 && i > 0 {
			// Reuse the previous game's opening for the color-swapped rematch.
			opening = results[i-1].OpeningLine
		}

		var e1, e2 coach.Stream
		bName, wName := "Black", "White"
		if i%2 == 1 {
			e1, e2 = white, black
			bName, wName = wName, bName
		} else {
			e1, e2 = black, white
		}

		gr := playOneGame(ctx, e1, e2, opening, gameTimeSec, bName, wName, assignmentID, i)
		results = append(results, gr)
	}
	return results
}

func playOneGame(ctx context.Context, black, white coach.Stream, opening string, gameTimeSec float64, bName, wName string, assignmentID, gameIdx int) gameResult {
	gr := gameResult{Black: bName, White: wName, OpeningLine: opening}

	for _, s := range []coach.Stream{black, white} {
		if _, err := wsSend(s, "boardsize 8", 10); err != nil {
			slog.Error("init failed", "cmd", "boardsize 8", "err", err)
			gr.Result = "0-1"; gr.Disconnect = true
			return gr
		}
		if _, err := wsSend(s, "clear_board", 10); err != nil {
			slog.Error("init failed", "cmd", "clear_board", "err", err)
			gr.Result = "0-1"; gr.Disconnect = true
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
			resp, err := wsSend(s, cmd, 10)
			if err != nil {
				slog.Error("opening play failed", "assign", assignmentID, "game", gameIdx+1, "move", mv, "color", color, "err", err)
				gr.Result = "0-1"; gr.Disconnect = true
				return gr
			}
			if strings.HasPrefix(resp, "?") {
				slog.Error("opening move REJECTED", "assign", assignmentID, "game", gameIdx+1,
					"move", mv, "color", color, "opening", opening, "response", strings.TrimSpace(resp))
				gr.Result = "0-1"; gr.Disconnect = true
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
		resp, err := wsSend(current, "genmove "+sideToMove, gameTimeSec)
		elapsed := time.Since(t0).Seconds()
		slog.Info("genmove", "assign", assignmentID, "game", gameIdx+1, "side", sideToMove, "move", strings.TrimSpace(resp)[:min(60, len(strings.TrimSpace(resp)))], "ms", int(elapsed*1000))
		if sideToMove == "b" {
			gr.BlackTimeS += elapsed
		} else {
			gr.WhiteTimeS += elapsed
		}

		if err != nil {
			// "read timeout" = engine hung, counts as loss
			// "stream closed" / "write timeout" = infrastructure, no Elo
			isInfra := strings.Contains(err.Error(), "stream closed") || strings.Contains(err.Error(), "write timeout")
			gr.Disconnect = isInfra
			if sideToMove == "b" {
				gr.Result = "0-1"
			} else {
				gr.Result = "1-0"
			}
			if isInfra {
				slog.Error("genmove failed (infra)", "assign", assignmentID, "game", gameIdx+1, "side", sideToMove, "err", err)
			} else {
				slog.Error("genmove failed (engine)", "assign", assignmentID, "game", gameIdx+1, "side", sideToMove, "err", err)
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
		opponent := white
		if sideToMove == "w" {
			opponent = black
		}
		playResp, _ := wsSend(opponent, "play "+sideToMove+" "+mv, 10)
		if strings.HasPrefix(playResp, "?") {
			slog.Warn("play rejected, ending game", "move", mv, "response", playResp)
			if sideToMove == "b" {
				gr.Result = "1-0"
			} else {
				gr.Result = "0-1"
			}
			break
		}

			// Consume all # stats lines, keeping the last.
			// Prefer JSON format (# neursi-stats v1: {...}), fall back to legacy.
			var nodes int64
			var depth int
			var timeout bool
			var score int
			var coachMs float64
			for {
				select {
				case statsLine := <-current.In:
					s := strings.TrimSpace(statsLine)
					if s == "" { continue }
					if strings.HasPrefix(s, "#") {
						// Try JSON format first
						if idx := strings.Index(s, "{"); idx >= 0 {
							var ns struct {
								Nodes   int64 `json:"nodes"`
								Depth   int   `json:"depth"`
								Score   int   `json:"score"`
								Timeout bool  `json:"timeout"`
							}
							if err := json.Unmarshal([]byte(s[idx:]), &ns); err == nil {
								nodes = ns.Nodes
								depth = ns.Depth
								score = ns.Score
								timeout = ns.Timeout
							}
						} else {
							// Legacy format: # time_ms X nodes Y depth Z score W timeout T
							n, _ := fmt.Sscanf(s, "# time_ms %f nodes %d depth %d score %d timeout %t",
								&coachMs, &nodes, &depth, &score, &timeout)
							if n == 0 {
								fmt.Sscanf(s, "# time_ms %f", &coachMs)
							}
						}
						continue
					}
				default:
				}
				break
			}
			if coachMs <= 0 {
				coachMs = elapsed * 1000
			}

			gr.Moves = append(gr.Moves, gameMove{
				Side: sideToMove, Move: mv,
				Nodes: nodes, Depth: depth, TimeMs: coachMs, Score: score,
			})
			slog.Info("move stored", "side", sideToMove, "move", mv, "nodes", nodes, "depth", depth, "score", score, "ms", coachMs, "total", len(gr.Moves))

		sideToMove, curPlayer, oppPlayer = flipSide(sideToMove, curPlayer, oppPlayer, board)

		if len(gr.Moves) > 90 {
			break
		}
	}

	// Aggregate game-level stats from per-move data
	for _, m := range gr.Moves {
		if m.Side == "b" {
			gr.BlackNodes += m.Nodes
			if m.Depth > gr.BlackDepth { gr.BlackDepth = m.Depth }
		} else {
			gr.WhiteNodes += m.Nodes
			if m.Depth > gr.WhiteDepth { gr.WhiteDepth = m.Depth }
		}
	}

	// Compute final score from the board (works for all endings: timeout, resign, normal)
	bCount := popcount(board.black)
	wCount := popcount(board.white)
	if gr.FinalScore == 0 {
		if bCount > wCount {
			gr.FinalScore = bCount - wCount
		} else if wCount > bCount {
			gr.FinalScore = wCount - bCount
		}
	}

	if gr.Result == "" {
		if bCount > wCount {
			gr.Result = "1-0"
		} else if wCount > bCount {
			gr.Result = "0-1"
		} else {
			gr.Result = "1/2"
		}
	}

	slog.Info("game result", "assign", assignmentID, "game", gameIdx+1, "result", gr.Result, "score", gr.FinalScore, "moves", len(gr.Moves), "black_s", gr.BlackTimeS, "white_s", gr.WhiteTimeS)
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
