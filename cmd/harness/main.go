package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/neoliv/arena/internal/game"
)

type Engine struct {
	label     string
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    *bufio.Reader
	lastBoard string
}

var boardRE = regexp.MustCompile(`^# board:([.BW]{64})$`)

func startEngine(label, bin string, args ...string) *Engine {
	cmd := exec.Command(bin, args...)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	cmd.Start()
	return &Engine{label, cmd, stdin, bufio.NewReader(stdout), ""}
}

var logFile *os.File

func log(fmtStr string, args ...interface{}) {
	s := time.Now().Format("15:04:05.000") + " " + fmt.Sprintf(fmtStr, args...)
	os.Stderr.WriteString(s + "\n")
	if logFile != nil { fmt.Fprintln(logFile, s) }
}

func (e *Engine) Send(cmd string) string {
	e.stdin.Write([]byte(cmd + "\n"))
	var buf strings.Builder
	for {
		line, err := e.stdout.ReadString('\n')
		if err != nil { break }
		l := strings.TrimSpace(line)
		if l == "" { continue }
		if m := boardRE.FindStringSubmatch(l); m != nil {
			e.lastBoard = m[1]
			log("[%s] board after cmd: %s", e.label, e.lastBoard)
			continue
		}
		if strings.HasPrefix(l, "#") { continue }
		buf.WriteString(l + "\n")
		if strings.HasPrefix(l, "=") || strings.HasPrefix(l, "?") {
			r := strings.TrimSpace(buf.String())
			log("[%s] << %s", e.label, r)
			return r
		}
	}
	return buf.String()
}

func fwBoardStr(board game.Board) string {
	var buf [64]byte
	blk, wht := board.Black(), board.White()
	for sq := 0; sq < 64; sq++ {
		switch {
		case blk&(1<<sq) != 0: buf[sq] = 'B'
		case wht&(1<<sq) != 0: buf[sq] = 'W'
		default: buf[sq] = '.'
		}
	}
	return string(buf[:])
}

func main() {
	f, err := os.Create("/tmp/game_trace.log")
	if err == nil {
		logFile = f
		defer f.Close()
	}

	b := startEngine("B", "/home/oliv/dev/agent/othello/neursi/engine/target/release/neursi",
		"--weights", "/tmp/sprt-cand-weights.bin.mmap", "--require-nn", "--log-state")
	w := startEngine("W", "/home/oliv/dev/agent/othello/neursi/engine/target/release/neursi",
		"--weights", "/tmp/sprt-cand-weights.bin.mmap", "--require-nn", "--log-state")
	defer b.cmd.Process.Kill()
	defer w.cmd.Process.Kill()

	log("[MM] >> B: boardsize 8")
	b.Send("boardsize 8")
	log("[MM] >> W: boardsize 8")
	w.Send("boardsize 8")
	log("[MM] >> B: clear_board")
	b.Send("clear_board")
	log("[MM] >> W: clear_board")
	w.Send("clear_board")

	board := game.NewBoard()
	log("[MM] initial board: %s", fwBoardStr(board))

	side := "b"
	passes := 0

	for moveNum := 0; moveNum < 90; moveNum++ {
		player := board.Black()
		if side == "w" { player = board.White() }
		legal := board.LegalMoves(player)
		cur := b
		if side == "w" { cur = w }

		if legal == 0 {
			if passes >= 1 { passes++; break }
		} else {
			passes = 0
		}

		log("[MM] >> %s: genmove %s (legal=%d)", cur.label, side, game.Popcount(legal))
		resp := cur.Send("genmove " + side)
		parts := strings.Fields(strings.TrimPrefix(strings.TrimSpace(resp), "= "))
		if len(parts) == 0 { log("[MM] *** EMPTY genmove"); break }
		mv := strings.ToUpper(parts[0])


		if mv == "PASS" || mv == "P" {
			log("[MM] %s PASS (passes=%d)", side, passes+1)
			passes++
			opp := w; if side == "w" { opp = b }
			log("[MM] >> %s: play %s pass", opp.label, side)
			opp.Send("play " + side + " pass")
			if passes >= 2 { log("[MM] Game over — double pass"); break }
			if side == "b" { side = "w" } else { side = "b" }
			continue
		}

		sq := game.SqFromString(mv)
		engBoard := cur.lastBoard
		fwBoard := fwBoardStr(board)
		if sq < 0 || (legal>>sq)&1 == 0 {
			log("[MM] *** ILLEGAL: %s %s empties=%d legal=%d", side, mv,
				64-game.Popcount(board.Black()|board.White()), game.Popcount(legal))
			log("[MM] fw board: %s", fwBoard)
			log("[MM] en board: %s", engBoard)
			break
		}

		board = board.ApplyMove(player, sq)
		opp := w; if side == "w" { opp = b }
		log("[MM] >> %s: play %s %s", opp.label, side, mv)
		playResp := opp.Send("play " + side + " " + mv)
		if strings.HasPrefix(playResp, "?") {
			log("[MM] *** PLAY REJECTED: %s", playResp)
			break
		}
		log("[MM] board after %s %s: %s", side, mv, fwBoardStr(board))

		// POST-PLAY: compare opponent board with framework board
		if opp.lastBoard != "" && opp.lastBoard != fwBoardStr(board) {
			log("[MM] *** POST-PLAY DIVERGENCE after %s %s: opp=%s", side, mv, opp.lastBoard)
			log("[MM]   MM board: %s", fwBoardStr(board))
		}

		if side == "b" { side = "w" } else { side = "b" }
	}
	log("[MM] Done — %d discs", game.Popcount(board.Black()|board.White()))
}
