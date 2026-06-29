// match_runner runs engine-vs-engine Othello games and POSTs results
// to the Arena server. Time control is total game time per side
// (engines manage their own internal allocation).
package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/neoliv/arena/internal/version"
)

//go:embed openings_8ply.txt
var embeddedBook string

type gameResult struct {
	Black, White                        string
	Result                              string
	FinalScore                          int
	OpeningLine                         string
	PGN                                 string
	BlackTime, WhiteTime                float64
	BlackNodes, WhiteNodes              int64
	BlackDepth, WhiteDepth              int
	perMoveStats                        []moveStat
}

type moveStat struct {
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
}

type engineRef struct{ Name, Version string }

func main() {
	var (
		engine1Path    = flag.String("engine1", "", "Path to engine 1 binary")
		engine1Name    = flag.String("engine1-name", "engine1", "Name of engine 1")
		engine1Version = flag.String("engine1-version", "0.0.0", "Version of engine 1")
		engine2Path    = flag.String("engine2", "", "Path to engine 2 binary")
		engine2Name    = flag.String("engine2-name", "engine2", "Name of engine 2")
		engine2Version = flag.String("engine2-version", "0.0.0", "Version of engine 2")
		gameTime       = flag.Float64("time", 60.0, "Total game time per side in seconds")
		games          = flag.Int("games", 10, "Number of games to play")
		bookFile       = flag.String("book", "", "Opening book file (default: embedded 49-line balanced book)")
		arenaURL       = flag.String("arena-url", "", "Arena server URL")
		arenaToken     = flag.String("arena-token", "", "Arena API token")
		concurrency    = flag.Int("concurrency", 1, "Number of concurrent games")
	showVer        = flag.Bool("version", false, "Print version and exit")
	)
		handleShortFlags("match_runner")
		flag.Parse()

	if *showVer {
		fmt.Print(version.PrintVersion("match_runner"))
		return
	}

	if *engine1Path == "" || *engine2Path == "" {
		slog.Error("both --engine1 and --engine2 are required")
		os.Exit(1)
	}

	// Load book
	bookData := embeddedBook
	if *bookFile != "" {
		data, err := os.ReadFile(*bookFile)
		if err != nil { slog.Error("read book", "err", err); os.Exit(1) }
		bookData = string(data)
	}
	var bookLines []string
	for _, line := range strings.Split(bookData, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") { bookLines = append(bookLines, line) }
	}
	if len(bookLines) == 0 { bookLines = []string{""} }

	slog.Info("match starting", "e1", *engine1Name+" "+*engine1Version, "e2", *engine2Name+" "+*engine2Version, "games", *games, "time_per_side", *gameTime, "book_lines", len(bookLines))
	startTime := time.Now()

	var (
		results []gameResult
		mu      sync.Mutex
		wg      sync.WaitGroup
		sem     = make(chan struct{}, *concurrency)
	)

	for i := 0; i < *games; i++ {
		wg.Add(1); sem <- struct{}{}
		go func(num int) {
			defer wg.Done(); defer func() { <-sem }()
			opening := bookLines[num%len(bookLines)]
			bPath, bName, bVer := *engine1Path, *engine1Name, *engine1Version
			wPath, wName, wVer := *engine2Path, *engine2Name, *engine2Version
			if num%2 == 1 { bPath, bName, bVer, wPath, wName, wVer = wPath, wName, wVer, bPath, bName, bVer }
			gr := runGame(bPath, wPath, opening, *gameTime, bName, wName)
			gr.Black, gr.White = bName+" "+bVer, wName+" "+wVer
			mu.Lock(); results = append(results, gr); mu.Unlock()
			if *arenaURL != "" {
				submitResults(*arenaURL, *arenaToken, *engine1Name, *engine1Version, *engine2Name, *engine2Version, *gameTime, []gameResult{gr})
			}
			w1, w2, d := 0, 0, 0
			for _, r := range results { switch r.Result { case "1-0": w1++; case "0-1": w2++; case "1/2": d++ } }
			slog.Info("game done", "num", num+1, "total", *games, "score", fmt.Sprintf("%d-%d-%d", w1, w2, d))
		}(i)
	}
	wg.Wait()

	w1, w2, d := 0, 0, 0
	for _, r := range results { switch r.Result { case "1-0": w1++; case "0-1": w2++; case "1/2": d++ } }
	slog.Info("match complete", "e1", *engine1Name, "e2", *engine2Name, "score", fmt.Sprintf("%d-%d-%d", w1, w2, d), "elapsed", time.Since(startTime).Round(time.Second))
	if *arenaURL != "" {
		submitSpeedStats(*arenaURL, *arenaToken, *engine1Name, *engine1Version, *engine2Name, *engine2Version, results)
	}
}

func runGame(blackPath, whitePath, opening string, gameTimeSec float64, blackName, whiteName string) gameResult {
	gr := gameResult{OpeningLine: opening}
	black, err := startEngine(blackPath)
	if err != nil {
		slog.Error("start black engine", "path", blackPath, "err", err)
		return gr
	}
	white, err2 := startEngine(whitePath)
	if err2 != nil {
		slog.Error("start white engine", "path", whitePath, "err", err2)
		black.stop()
		return gr
	}
	defer black.stop(); defer white.stop()
	for _, e := range []*gtpEngine{black, white} {
		e.send("boardsize 8"); e.send("clear_board")
		if resp := e.send(fmt.Sprintf("game_time %.1f", gameTimeSec)); strings.HasPrefix(resp, "?") {
			slog.Warn("game_time not supported by engine (deprecated GTP extension)", "response", strings.TrimSpace(resp))
		}
	}
	moves := parseMoveList(opening)
	for i, mv := range moves {
		color := "B"; if i%2 == 1 { color = "W" }
		for _, e := range []*gtpEngine{black, white} { e.send("play " + color + " " + mv) }
	}
	var pgnBuf bytes.Buffer
	fmt.Fprintf(&pgnBuf, "[Event \"Arena\"]\n[Date \"%s\"]\n[TimeControl \"%.0fs total\"]\n", time.Now().Format("2006.01.02"), gameTimeSec)
	if opening != "" { fmt.Fprintf(&pgnBuf, "[Opening \"%s\"]\n", opening) }
	pgnBuf.WriteString("\n")
	moveNum, sideToMove, consecutivePasses := len(moves), "B", 0
	if moveNum%2 == 1 { sideToMove = "W" }
	for {
		// Check total time exceeded
		if gr.BlackTime >= gameTimeSec { gr.Result = "0-1"; gr.FinalScore = 0; break }
		if gr.WhiteTime >= gameTimeSec { gr.Result = "1-0"; gr.FinalScore = 0; break }
		current := black; if sideToMove == "W" { current = white }
		t0 := time.Now()
		resp := current.send(fmt.Sprintf("genmove %s", sideToMove))
		elapsed := time.Since(t0).Seconds()
		if sideToMove == "B" { gr.BlackTime += elapsed } else { gr.WhiteTime += elapsed }
		resp = strings.TrimSpace(resp)
		if strings.HasPrefix(resp, "= ") { resp = strings.TrimPrefix(resp, "= ") }
		parts := strings.Fields(resp)
		if len(parts) == 0 { break }
		mv := strings.ToUpper(parts[0])
		if mv == "RESIGN" { if sideToMove == "B" { gr.Result = "0-1" } else { gr.Result = "1-0" }; break }
		if mv == "PASS" { consecutivePasses++; if consecutivePasses >= 2 { break }; sideToMove = map[string]string{"B":"W","W":"B"}[sideToMove]; continue }
		consecutivePasses = 0
		moveNum++; sideToMove = map[string]string{"B":"W","W":"B"}[sideToMove]
		other := white; if sideToMove == "W" { other = black }
		other.send("play " + sideToMove + " " + mv)
		if moveNum%2 == 1 { fmt.Fprintf(&pgnBuf, "%d. %s", (moveNum+1)/2, mv) } else { fmt.Fprintf(&pgnBuf, " %s\n", mv) }

		// Engine stats
		statsResp := current.send("stats")
		statsResp = strings.TrimSpace(strings.TrimPrefix(statsResp, "= "))
		var nodes, nps int64; var depth, branch, empties int; var timeMs float64; var timeout bool; var score int
		fmt.Sscanf(statsResp, "nodes %d depth %d time_ms %f timeout %t score %d nps %d branching %d empties %d", &nodes, &depth, &timeMs, &timeout, &score, &nps, &branch, &empties)
		if nodes > 0 {
			en := blackName; if sideToMove == "W" { en = whiteName }
			gr.perMoveStats = append(gr.perMoveStats, moveStat{Ply: moveNum, Color: sideToMove, Nodes: nodes, TimeS: timeMs/1000.0, Depth: depth, Timeout: timeout, Score: score, NPS: nps, Branching: branch, Empties: empties, EngineName: en})
			if sideToMove == "B" { gr.BlackNodes += nodes; if depth > gr.BlackDepth { gr.BlackDepth = depth } } else { gr.WhiteNodes += nodes; if depth > gr.WhiteDepth { gr.WhiteDepth = depth } }
		}
		if moveNum > 120 { break }
	}
	if moveNum%2 == 1 { pgnBuf.WriteString("\n") }
	finalResp := black.send("final_score")
	finalResp = strings.TrimSpace(finalResp)
	if strings.HasPrefix(finalResp, "?") {
		slog.Warn("final_score not supported by engine (deprecated GTP extension)", "response", finalResp)
		if gr.Result == "" { gr.Result = "1/2" }
	} else {
		finalResp = strings.TrimPrefix(finalResp, "= ")
		var finalScore int
		if strings.HasPrefix(finalResp, "B+") { fmt.Sscanf(finalResp, "B+%d", &finalScore); gr.Result = "1-0" } else if strings.HasPrefix(finalResp, "W+") { fmt.Sscanf(finalResp, "W+%d", &finalScore); gr.Result = "0-1" } else { gr.Result = "1/2" }
		gr.FinalScore = finalScore
	}
	fmt.Fprintf(&pgnBuf, "{%s} %s\n", finalResp, gr.Result)
	gr.PGN = pgnBuf.String()
	return gr
}

func startEngine(path string) (*gtpEngine, error) {
	parts := strings.Fields(path)
	cmd := exec.Command(parts[0], parts[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	return &gtpEngine{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}, nil
}

type gtpEngine struct {
	cmd    *exec.Cmd; stdin io.WriteCloser; stdout *bufio.Reader
}

func (e *gtpEngine) send(cmd string) string {
	if e.stdin == nil { return "" }
	e.stdin.Write([]byte(cmd + "\n"))
	var buf bytes.Buffer
	for { line, err := e.stdout.ReadString('\n'); if err != nil { break }; buf.WriteString(line); if strings.HasPrefix(line, "=") || strings.HasPrefix(line, "?") { break } }
	return buf.String()
}

func (e *gtpEngine) stop() { if e.stdin != nil { e.stdin.Write([]byte("quit\n")) }; e.cmd.Wait() }

func parseMoveList(line string) []string {
	if line == "" { return nil }
	var m []string
	for i := 0; i < len(line); i += 2 { if i+1 < len(line) { m = append(m, strings.ToUpper(line[i:i+2])) } }
	return m
}

func submitResults(url, token, name1, ver1, name2, ver2 string, gameTime float64, games []gameResult) {
	type gr struct {
		Black, White, Result, OpeningLine, PGN string; FinalScore int
		BlackTimeS, WhiteTimeS float64; BlackNodes, WhiteNodes int64; BlackDepth, WhiteDepth int
	}
	var gs []gr
	for _, g := range games {
		gs = append(gs, gr{Black: g.Black, White: g.White, Result: g.Result, FinalScore: g.FinalScore, OpeningLine: g.OpeningLine, PGN: g.PGN, BlackTimeS: g.BlackTime, WhiteTimeS: g.WhiteTime, BlackNodes: g.BlackNodes, WhiteNodes: g.WhiteNodes, BlackDepth: g.BlackDepth, WhiteDepth: g.WhiteDepth})
	}
	payload := struct{ Engine1, Engine2 engineRef; TimeControl map[string]any; OpeningBook, RunnerID string; Games []gr }{
		Engine1: engineRef{Name: name1, Version: ver1}, Engine2: engineRef{Name: name2, Version: ver2},
		TimeControl: map[string]any{"type":"total","seconds":gameTime}, RunnerID: hostname(), Games: gs,
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", url+"/matches", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" { req.Header.Set("Authorization", "Bearer "+token) }
	resp, err := http.DefaultClient.Do(req)
	if err != nil { slog.Error("submit", "err", err); return }
	resp.Body.Close()
	if resp.StatusCode == 200 { slog.Info("results submitted to arena") } else { slog.Error("submit", "status", resp.StatusCode) }
}

func submitSpeedStats(url, token, name1, ver1, name2, ver2 string, games []gameResult) {
	var moves []moveStat
	for _, g := range games { moves = append(moves, g.perMoveStats...) }
	if len(moves) == 0 { return }
	payload := struct{ Engine1, Engine2 engineRef; Moves []moveStat }{Engine1: engineRef{Name: name1, Version: ver1}, Engine2: engineRef{Name: name2, Version: ver2}, Moves: moves}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", url+"/speed", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" { req.Header.Set("Authorization", "Bearer "+token) }
	resp, err := http.DefaultClient.Do(req)
	if err != nil { return }
	resp.Body.Close()
	if resp.StatusCode == 200 { slog.Info("speed stats submitted", "moves", len(moves)) }
}

func hostname() string { h, _ := os.Hostname(); return h }

// handleShortFlags is duplicated across cmd/*/main.go.
// Canonical source: cmd/coach/main.go
// TODO: move to internal/cmdutil/
func handleShortFlags(name string) {
	for _, a := range os.Args[1:] {
		if a == "-h" {
			flag.CommandLine.SetOutput(os.Stdout)
			flag.PrintDefaults()
			fmt.Printf("\nShort flags: -h (help), -V (version), --version\n")
			os.Exit(0)
		}
		if a == "-V" {
			fmt.Print(version.PrintVersion(name))
			os.Exit(0)
		}
	}
}
