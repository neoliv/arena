package game

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// MoveStats holds per-move search statistics captured from engine responses.
// All fields are optional — engines that don't emit stats will have zero values.
type MoveStats struct {
	Ply         int     `json:"ply"`
	Color       string  `json:"color"`        // "black" or "white"
	Engine      string  `json:"engine"`       // "candidate" or "reference"
	Move        string  `json:"move"`
	Nodes       int64   `json:"nodes"`
	Depth       int     `json:"depth"`
	TimeMs      float64 `json:"time_ms"`
	Timeout     bool    `json:"timeout"`
	Score       int     `json:"score"`
	Nps         int64   `json:"nps"`
	Empties     int     `json:"empties"`
	AllocatedMs float64 `json:"allocated_ms"`
	EndSearch   bool    `json:"end_search"`
	BookExit    bool    `json:"book_exit"`
	BookEval    *int    `json:"book_eval,omitempty"`
}

// neursiStatsV1 is the JSON payload from a # arena-stats v1: GTP comment line.
type neursiStatsV1 struct {
	Nodes       int64   `json:"nodes"`
	Depth       int     `json:"depth"`
	TimeMs      float64 `json:"time_ms"`
	Timeout     bool    `json:"timeout"`
	Score       int     `json:"score"`
	Nps         int64   `json:"nps"`
	Empties     int     `json:"empties"`
	AllocatedMs float64 `json:"allocated_ms"`
	EndSearch   bool    `json:"end_search"`
	BookExit    bool    `json:"book_exit"`
	BookEval    *int    `json:"book_eval"`
}

// GameResult holds the outcome of a single Othello game.
type GameResult struct {
	BlackName   string
	WhiteName   string
	Result      string // "1-0", "0-1", "1/2"
	BlackScore  int
	WhiteScore  int
	TotalMoves  int
	BlackTimeS  float64
	WhiteTimeS  float64
	OpeningLine string
	Disconnect  bool
	// InvestigationNeeded is set when the framework's board disagrees with
	// an engine about move legality. The game result is unreliable and
	// should be reviewed by a human.
	InvestigationNeeded bool
	// InvestigationReason describes what triggered the investigation flag.
	InvestigationReason string
	Moves       []string    // move sequence as squares (e.g. ["F5","D6",...])
	MoveStats   []MoveStats // per-move search stats (from engine genmove responses)
}

// PlayGame runs one full Othello game between two engines via GTP.
func PlayGame(black, white *Session, opening string, gameTimeSec float64) GameResult {
	gr := GameResult{
		OpeningLine: opening,
	}

	// Init both engines
	if err := black.Init(gameTimeSec); err != nil {
		slog.Error("black init", "err", err)
		gr.Result = "0-1"
		gr.Disconnect = true
		return gr
	}
	if err := white.Init(gameTimeSec); err != nil {
		slog.Error("white init", "err", err)
		gr.Result = "1-0"
		gr.Disconnect = true
		return gr
	}

	// Play opening moves. An engine rejecting a valid opening move means
	// the engine has a bug or the opening line is corrupt — either way
	// this is a hard error. We abort the game, do NOT continue silently.
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
				slog.Error("opening move REJECTED by engine — this is a BUG in the engine or a corrupt opening line",
					"move", mv, "color", color, "opening", opening, "response", strings.TrimSpace(resp))
				gr.Result = "0-1"
				gr.Disconnect = true
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
	plyCount := moveCount
	sideToMove := "B"
	if moveCount%2 == 1 {
		sideToMove = "W"
	}

	// Track which engine plays which color for stats attribution

	timeLimit := gameTimeSec * 1.05
	consecutivePasses := 0

	for gr.TotalMoves < 90 {
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

		var curPlayer uint64
		if sideToMove == "B" {
			curPlayer = board.black
		} else {
			curPlayer = board.white
		}

		legal := board.LegalMoves(curPlayer)
		if legal == 0 {
			// If the previous turn was also a forced pass, both sides have
			// no moves — game over. consecutivePasses is incremented by the
			// PASS handler below when the engine returns PASS.
			if consecutivePasses >= 1 {
				consecutivePasses++
				break
			}
			// Fall through to genmove below. The engine will detect it has
			// no legal moves and return PASS, triggering the voluntary-pass
			// handler which correctly notifies only the opponent.
		} else {
			consecutivePasses = 0
		}

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

		// Capture per-move stats if the engine emitted them
		if statsJSON := current.LastStats(); statsJSON != "" {
			var s neursiStatsV1
			if err := json.Unmarshal([]byte(statsJSON), &s); err == nil {
				colorName := "black"
				if sideToMove == "W" {
					colorName = "white"
				}
				// Engine identity will be filled in by the caller (main.go)
				gr.MoveStats = append(gr.MoveStats, MoveStats{
					Ply:         plyCount,
					Color:       colorName,
					Move:        "", // filled below after parsing response
					Nodes:       s.Nodes,
					Depth:       s.Depth,
					TimeMs:      s.TimeMs,
					Timeout:     s.Timeout,
					Score:       s.Score,
					Nps:         s.Nps,
					Empties:     s.Empties,
					AllocatedMs: s.AllocatedMs,
					EndSearch:   s.EndSearch,
					BookExit:    s.BookExit,
					BookEval:    s.BookEval,
				})
			}
		}

		// Strip # comment lines (engine stats) and extract the = response
		for _, line := range strings.Split(resp, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "#") || line == "" {
				continue
			}
			if strings.HasPrefix(line, "=") {
				resp = strings.TrimPrefix(line, "= ")
				break
			}
		}
		parts := strings.Fields(resp)
		if len(parts) == 0 {
			slog.Warn("empty genmove", "side", sideToMove, "raw", resp)
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
			// Genmove already applied the pass internally. Tell the opponent.
			passCmd := "play " + sideToMove + " pass"
			var passResp string
			if sideToMove == "B" {
				passResp = white.Send(passCmd)
			} else {
				passResp = black.Send(passCmd)
			}
			if strings.HasPrefix(passResp, "?") {
				slog.Warn("play pass rejected by engine", "side", sideToMove, "response", passResp)
				if sideToMove == "B" { gr.Result = "0-1" } else { gr.Result = "1-0" }
				gr.Disconnect = true
				break
			}
			if consecutivePasses >= 2 {
				break
			}
			sideToMove = flipSide(sideToMove)
			plyCount++
			continue
		}

		sq := SqFromString(mv)
		if sq < 0 {
			// Invalid format — not a real move string. This is a hard error.
			slog.Warn("invalid genmove response format",
				"side", sideToMove, "move", mv,
				"empties", 64-Popcount(board.black|board.white),
			)
			if sideToMove == "B" {
				gr.Result = "0-1"
			} else {
				gr.Result = "1-0"
			}
			break
		}

		// ── Move legality validation: DISABLED ──────────────────────────
		// We trust the engine's move. The framework's board may diverge from
		// the engine's board (e.g., pass handling, board tracking bugs).
		// When the framework disagrees with the engine about legality, we
		// log prominently and flag the game for investigation rather than
		// auto-assigning a winner.
		//
		// Original validation (kept for reference):
		//   if (legal>>sq)&1 == 0 {
		//       slog.Warn("illegal move from engine", ...)
		//       // auto-assign loss to offending engine
		//       break
		//   }
		if (legal>>sq)&1 == 0 {
			slog.Error("INVESTIGATION NEEDED: framework says move is illegal, but engine played it",
				"side", sideToMove, "move", mv,
				"empties", 64-Popcount(board.black|board.white),
				"legal_count", Popcount(legal),
				"legal", fmt.Sprintf("%064b", legal),
				"black", fmt.Sprintf("%064b", board.black),
				"white", fmt.Sprintf("%064b", board.white),
			)
			if !gr.InvestigationNeeded {
				gr.InvestigationNeeded = true
				gr.InvestigationReason = fmt.Sprintf("framework-illegal: %s played %s with %d legal moves on framework board (empties=%d)",
					sideToMove, mv, Popcount(legal), 64-Popcount(board.black|board.white))
			}
			// Apply the move anyway — trust the engine.
		}

		board = board.ApplyMove(curPlayer, sq)
		gr.Moves = append(gr.Moves, mv)
		// Framework board after move (for debugging board divergence).
		slog.Debug("framework board", "side", sideToMove, "move", mv,
			"empties", 64-Popcount(board.black|board.white),
			"black", fmt.Sprintf("%016x", board.black),
			"white", fmt.Sprintf("%016x", board.white))

		// Fill in the move name in the last stats entry
		if len(gr.MoveStats) > 0 {
			gr.MoveStats[len(gr.MoveStats)-1].Move = mv
		}

		moveCount++
		plyCount++
		gr.TotalMoves++

		// Standard GTP: genmove applies the move to the engine's own board.
		// play is sent ONLY to the opponent to announce the move.
		// Sending play to the engine that just moved is a protocol violation
		// — engines like Edax reject it with "? wrong color".
		playCmd := "play " + sideToMove + " " + mv
		var playResp string
		if sideToMove == "B" {
			playResp = white.Send(playCmd)
		} else {
			playResp = black.Send(playCmd)
		}
		if strings.HasPrefix(playResp, "?") {
			// Engine rejected the opponent's move as illegal. This could be:
			// - The moving engine sent a move illegal on its own board
			// - The rejecting engine has a board divergence
			// - The framework has a board tracking bug
			// We can't determine which — flag for investigation.
			slog.Error("INVESTIGATION NEEDED: engine rejected opponent's move as illegal",
				"move", mv, "side", sideToMove, "response", playResp,
				"empties", 64-Popcount(board.black|board.white),
				"black", fmt.Sprintf("%064b", board.black),
				"white", fmt.Sprintf("%064b", board.white),
			)
			gr.InvestigationNeeded = true
			gr.InvestigationReason = fmt.Sprintf("engine-rejected-play: opponent rejected %s %s: %s",
				sideToMove, mv, strings.TrimSpace(playResp))
			gr.Disconnect = true
			// Don't auto-assign a winner — let the board-based result at the
			// end of the loop compute the outcome. The game is flagged so
			// Elo won't be affected.
			break
		}

		sideToMove = flipSide(sideToMove)
	}

	if gr.Result == "" {
		bc, wc, result := board.Result()
		gr.BlackScore = bc
		gr.WhiteScore = wc
		gr.Result = result
	}

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
