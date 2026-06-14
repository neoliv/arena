package matchmaker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"nhooyr.io/websocket"
)

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
}

func wsSend(ctx context.Context, conn *websocket.Conn, cmd string) (string, error) {
	if err := conn.Write(ctx, websocket.MessageText, []byte(cmd+"\n")); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	// Read response lines until = or ?
	var buf strings.Builder
	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			return "", fmt.Errorf("read: %w", err)
		}
		line := string(msg)
		buf.WriteString(line)
		if strings.HasPrefix(line, "=") || strings.HasPrefix(line, "?") {
			break
		}
	}
	return buf.String(), nil
}

func playGames(ctx context.Context, black, white *websocket.Conn, numGames int, gameTimeSec float64) []gameResult {
	openings := defaultBook()
	var results []gameResult

	for i := 0; i < numGames; i++ {
		opening := openings[i%len(openings)]

		// Swap colors for even-numbered games
		var e1, e2 *websocket.Conn
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

func playOneGame(ctx context.Context, black, white *websocket.Conn, opening string, gameTimeSec float64, bName, wName string) gameResult {
	gr := gameResult{Black: bName, White: wName, OpeningLine: opening}

	// Initialize both engines
	for _, eng := range []*websocket.Conn{black, white} {
		wsSend(ctx, eng, "boardsize 8")
		wsSend(ctx, eng, "clear_board")
		wsSend(ctx, eng, fmt.Sprintf("game_time %.1f", gameTimeSec))
	}

	// Play opening moves
	moves := parseMoveList(opening)
	for i, mv := range moves {
		color := "B"
		if i%2 == 1 { color = "W" }
		for _, eng := range []*websocket.Conn{black, white} {
			wsSend(ctx, eng, "play "+color+" "+mv)
		}
	}

	moveNum := len(moves)
	sideToMove := "B"
	if moveNum%2 == 1 { sideToMove = "W" }
	consecutivePasses := 0

	for {
		if gr.BlackTimeS >= gameTimeSec { gr.Result = "0-1"; break }
		if gr.WhiteTimeS >= gameTimeSec { gr.Result = "1-0"; break }

		current := black
		if sideToMove == "W" { current = white }

		t0 := time.Now()
		resp, err := wsSend(ctx, current, "genmove "+sideToMove)
		elapsed := time.Since(t0).Seconds()
		if sideToMove == "B" { gr.BlackTimeS += elapsed } else { gr.WhiteTimeS += elapsed }

		if err != nil {
			slog.Error("genmove failed", "side", sideToMove, "err", err)
			if sideToMove == "B" { gr.Result = "0-1" } else { gr.Result = "1-0" }
			break
		}

		resp = strings.TrimSpace(strings.TrimPrefix(resp, "= "))
		parts := strings.Fields(resp)
		if len(parts) == 0 { break }
		mv := strings.ToUpper(parts[0])

		if mv == "RESIGN" {
			if sideToMove == "B" { gr.Result = "0-1" } else { gr.Result = "1-0" }
			break
		}
		if mv == "PASS" {
			consecutivePasses++
			if consecutivePasses >= 2 { break }
			sideToMove = map[string]string{"B": "W", "W": "B"}[sideToMove]
			continue
		}
		consecutivePasses = 0
		moveNum++
		sideToMove = map[string]string{"B": "W", "W": "B"}[sideToMove]

		// Tell the other engine
		other := white
		if sideToMove == "W" { other = black }
		wsSend(ctx, other, "play "+sideToMove+" "+mv)

		// Gather stats
		statsResp, _ := wsSend(ctx, current, "stats")
		statsResp = strings.TrimPrefix(strings.TrimSpace(statsResp), "= ")
		var nodes, nps int64
		var depth, branch, empties int
		var tm float64
		var timeout bool
		var score int
		fmt.Sscanf(statsResp, "nodes %d depth %d time_ms %f timeout %t score %d nps %d branching %d empties %d",
			&nodes, &depth, &tm, &timeout, &score, &nps, &branch, &empties)

		if sideToMove == "B" {
			gr.BlackNodes += nodes
			if depth > gr.BlackDepth { gr.BlackDepth = depth }
		} else {
			gr.WhiteNodes += nodes
			if depth > gr.WhiteDepth { gr.WhiteDepth = depth }
		}

		if moveNum > 120 { break }
	}

	// Get final score
	if gr.Result == "" {
		finalResp, _ := wsSend(ctx, black, "final_score")
		finalResp = strings.TrimPrefix(strings.TrimSpace(finalResp), "= ")
		if strings.HasPrefix(finalResp, "B+") {
			fmt.Sscanf(finalResp, "B+%d", &gr.FinalScore)
			gr.Result = "1-0"
		} else if strings.HasPrefix(finalResp, "W+") {
			fmt.Sscanf(finalResp, "W+%d", &gr.FinalScore)
			gr.Result = "0-1"
		} else {
			gr.Result = "1/2"
		}
	}

	return gr
}

func parseMoveList(line string) []string {
	if line == "" { return nil }
	line = strings.TrimSpace(line)
	var m []string
	for i := 0; i < len(line); i += 2 {
		if i+1 < len(line) { m = append(m, strings.ToUpper(line[i:i+2])) }
	}
	return m
}

func defaultBook() []string {
	return []string{
		"f5d6c3d3c4e3f4c5",
		"f5f6e6f4e3d6c5c4",
		"e6f6f5e3d3c5c4d6",
		"e6f4d6c5f5e3c4d3",
		"d3c5f6e3c4f5e6f4",
		"d3c4f5d6c3e3f4c5",
		"c4e3f5e6f4c5d6f6",
		"c4c3d3c5f4e3f5d6",
	}
}
