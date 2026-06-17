package game

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// GameResult holds the outcome of a single Othello game.
type GameResult struct {
	BlackName    string
	WhiteName    string
	Result       string // "1-0", "0-1", "1/2"
	BlackScore   int
	WhiteScore   int
	TotalMoves   int
	BlackTimeS   float64
	WhiteTimeS   float64
	OpeningLine  string
	Disconnect   bool
	Moves        []string // move sequence as squares (e.g. ["F5","D6",...])
}

// PlayGame runs one full Othello game between two engines via GTP.
// black and white are engine sessions. opening is the opening line in
// continuous format (e.g. "f5d6c3d3c4"). gameTimeSec is total time per side.
func PlayGame(black, white *Session, opening string, gameTimeSec float64) GameResult {
	gr := GameResult{
		OpeningLine: opening,
	}

	// Init both engines
	if err := black.Init(gameTimeSec); err != nil {
		slog.Error("black init", "err", err)
		gr.Result = "0-1"; gr.Disconnect = true
		return gr
	}
	if err := white.Init(gameTimeSec); err != nil {
		slog.Error("white init", "err", err)
		gr.Result = "1-0"; gr.Disconnect = true
		return gr
	}

	// Play opening moves on both engines
	openMoves := parseMoveList(opening)
	for i, mv := range openMoves {
		color := "B"
		if i%2 == 1 {
			color = "W"
		}
		cmd := "play " + color + " " + mv
		for _, s := range []*Session{black, white} {
			resp := s.Send(cmd)
			if strings.HasPrefix(resp, "?") {
				slog.Warn("opening move rejected", "move", mv, "color", color, "resp", strings.TrimSpace(resp))
				if color == "B" {
					gr.Result = "0-1"
				} else {
					gr.Result = "1-0"
				}
				return gr
			}
		}
	}

	// Set up board for validation
	board := NewBoard()
	for i, mv := range openMoves {
		var player uint64
		if i%2 == 0 {
			player = board.black
		} else {
			player = board.white
		}
		sq := SqFromString(mv)
		if sq >= 0 {
			board = board.ApplyMove(player, sq)
		}
	}

	// Determine side to move after opening
	moveCount := len(openMoves)
	sideToMove := "B"
	if moveCount%2 == 1 {
		sideToMove = "W"
	}

	timeLimit := gameTimeSec * 1.05
	consecutivePasses := 0

	for gr.TotalMoves < 90 {
		// Time enforcement
		if gr.BlackTimeS >= timeLimit {
			gr.Result = "0-1"
			break
		}
		if gr.WhiteTimeS >= timeLimit {
			gr.Result = "1-0"
			break
		}
		if board.IsOver() {
			break
		}

		// Determine player for this turn
		var curPlayer uint64
		if sideToMove == "B" {
			curPlayer = board.black
		} else {
			curPlayer = board.white
		}

		legal := board.LegalMoves(curPlayer)
		if legal == 0 {
			consecutivePasses++
			if consecutivePasses >= 2 {
				break
			}
			// Pass: swap sides
			gr.Moves = append(gr.Moves, "PASS")
			sideToMove = flipSide(sideToMove)
			continue
		}
		consecutivePasses = 0

		current := black
		if sideToMove == "W" {
			current = white
		}

		t0 := time.Now()
		resp := current.Send("genmove " + sideToMove)
		elapsed := time.Since(t0).Seconds()
		if sideToMove == "B" {
			gr.BlackTimeS += elapsed
		} else {
			gr.WhiteTimeS += elapsed
		}

		resp = strings.TrimSpace(strings.TrimPrefix(resp, "= "))
		parts := strings.Fields(resp)
		if len(parts) == 0 {
			slog.Warn("empty genmove", "side", sideToMove)
			break
		}
		mv := strings.ToUpper(parts[0])

		if mv == "RESIGN" {
			if sideToMove == "B" {
				gr.Result = "0-1"
			} else {
				gr.Result = "1-0"
			}
			break
		}

		if mv == "PASS" {
			consecutivePasses++
			gr.Moves = append(gr.Moves, "PASS")
			if consecutivePasses >= 2 {
				break
			}
			sideToMove = flipSide(sideToMove)
			continue
		}

		// Validate move
		sq := SqFromString(mv)
		if sq < 0 || (legal>>sq)&1 == 0 {
			slog.Warn("illegal move from engine", "side", sideToMove, "move", mv)
			if sideToMove == "B" {
				gr.Result = "0-1"
			} else {
				gr.Result = "1-0"
			}
			break
		}

		// Apply move to validation board
		board = board.ApplyMove(curPlayer, sq)
		gr.Moves = append(gr.Moves, mv)
		moveCount++

		// Inform the other engine
		opponent := white
		if sideToMove == "W" {
			opponent = black
		}
		opponent.Send("play " + sideToMove + " " + mv)

		sideToMove = flipSide(sideToMove)
	}

	if gr.Result == "" {
		bc, wc, result := board.Result()
		gr.BlackScore = bc
		gr.WhiteScore = wc
		gr.Result = result
	}

	// Get final scores from engine for accurate count
	finalResp := black.Send("final_score")
	finalResp = strings.TrimPrefix(strings.TrimSpace(finalResp), "= ")
	if strings.HasPrefix(finalResp, "B+") {
		fmt.Sscanf(finalResp, "B+%d", &gr.BlackScore)
	} else if strings.HasPrefix(finalResp, "W+") {
		fmt.Sscanf(finalResp, "W+%d", &gr.WhiteScore)
	}

	return gr
}

func flipSide(side string) string {
	if side == "B" {
		return "W"
	}
	return "B"
}
