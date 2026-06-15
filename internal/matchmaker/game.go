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
	Side   string
	Move   string
	Nodes  int64
	Depth  int
	TimeMs float64
	Score  int
	NPS    int64
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

	moves := parseMoveList(opening)
	for i, mv := range moves {
		color := "b"
		if i%2 == 1 {
			color = "w"
		}
		cmd := "play " + color + " " + mv
		for _, s := range []coach.Stream{black, white} {
			select {
			case s.Out <- cmd:
			default:
			}
		}
		for _, s := range []coach.Stream{black, white} {
			var resp string
			for {
				select {
				case r := <-s.In:
					if strings.TrimSpace(r) != "" {
						resp = r
					} else {
						continue
					}
				case <-time.After(3 * time.Second):
					slog.Error("opening ack timeout", "move", mv)
					gr.Result = "0-1"
					return gr
				}
				break
			}
			if strings.HasPrefix(resp, "?") {
				slog.Warn("opening move rejected", "move", mv, "color", color, "response", resp)
				gr.Result = "0-1"
				return gr
			}
		}
	}

	moveNum := len(moves)
	sideToMove := "b"
	if moveNum%2 == 1 {
		sideToMove = "w"
	}
	consecutivePasses := 0
	timeLimit := gameTimeSec * 1.05

	for {
		if gr.BlackTimeS >= timeLimit {
			gr.Result = "0-1"
			break
		}
		if gr.WhiteTimeS >= timeLimit {
			gr.Result = "1-0"
			break
		}

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
		if mv != "PASS" && mv != "RESIGN" && (len(mv) != 2 || mv[0] < 'A' || mv[0] > 'H' || mv[1] < '1' || mv[1] > '8') {
			slog.Warn("invalid genmove response", "side", sideToMove, "raw", resp)
			if sideToMove == "b" {
				gr.Result = "0-1"
			} else {
				gr.Result = "1-0"
			}
			break
		}

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
			sideToMove = map[string]string{"b": "w", "w": "b"}[sideToMove]
			continue
		}
		consecutivePasses = 0
		moveNum++

		playedColor := sideToMove
		sideToMove = map[string]string{"b": "w", "w": "b"}[sideToMove]
		other := black
		if playedColor == "b" {
			other = white
		}
		wsSend(other, "play "+playedColor+" "+mv)

		gr.Moves = append(gr.Moves, gameMove{
			Side: playedColor, Move: mv,
		})

		if moveNum > 120 {
			break
		}
	}

	if gr.Result == "" {
		gr.Result = "1/2"
	}
	switch gr.Result {
	case "1-0":
		gr.FinalScore = 64
	case "0-1":
		gr.FinalScore = -64
	default:
		gr.FinalScore = 0
	}

	return gr
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
