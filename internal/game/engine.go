// Package game provides shared GTP engine management and Othello game logic
// used by both the match runner and SPRT tool.
package game

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

// Session manages a single GTP engine subprocess.
type Session struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

// StartEngine launches an engine subprocess. path may include arguments
// (e.g. "./neursi --weights eval.bin").
func StartEngine(path string) *Session {
	parts := strings.Fields(path)
	cmd := exec.Command(parts[0], parts[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		slog.Error("engine stdin pipe", "path", path, "err", err)
		return nil
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		slog.Error("engine stdout pipe", "path", path, "err", err)
		return nil
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		slog.Error("engine start", "path", path, "err", err)
		return nil
	}
	return &Session{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}
}

// Send sends a GTP command and returns the response (everything up to and
// including the = or ? status line). Lines prefixed with # (stats/comments)
// are discarded.
func (s *Session) Send(cmd string) string {
	if s.stdin == nil {
		return ""
	}
	s.stdin.Write([]byte(cmd + "\n"))
	var buf bytes.Buffer
	for {
		line, err := s.stdout.ReadString('\n')
		if err != nil {
			break
		}
		buf.WriteString(line)
		if strings.HasPrefix(line, "=") || strings.HasPrefix(line, "?") {
			break
		}
	}
	return buf.String()
}

// Init sets up the engine for a new game.
func (s *Session) Init(gameTimeSec float64) error {
	resp := s.Send("boardsize 8")
	if strings.HasPrefix(resp, "?") {
		return fmt.Errorf("boardsize: %s", strings.TrimSpace(resp))
	}
	resp = s.Send("clear_board")
	if strings.HasPrefix(resp, "?") {
		return fmt.Errorf("clear_board: %s", strings.TrimSpace(resp))
	}
	resp = s.Send(fmt.Sprintf("game_time %.1f", gameTimeSec))
	if strings.HasPrefix(resp, "?") {
		return fmt.Errorf("game_time: %s", strings.TrimSpace(resp))
	}
	return nil
}

// Stop sends quit and waits for the engine to exit.
func (s *Session) Stop() {
	if s.stdin != nil {
		s.stdin.Write([]byte("quit\n"))
	}
	s.cmd.Wait()
}

// PID returns the engine process PID.
func (s *Session) PID() int {
	if s.cmd != nil && s.cmd.Process != nil {
		return s.cmd.Process.Pid
	}
	return 0
}
