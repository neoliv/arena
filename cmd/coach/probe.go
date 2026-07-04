package main

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// probeVersion starts the engine binary and queries its GTP version.
// The engine is killed immediately after the response — no 5s timeout wait.
// A 5s deadline guards against hung engines.
func probeVersion(ai aiConfig) (string, error) {
	parts := strings.Fields(ai.RunCmd)
	if len(parts) == 0 {
		return "", fmt.Errorf("empty run command")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
	cmd.Dir = filepath.Dir(parts[0])
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start: %w", err)
	}

	stdin.Write([]byte("version\n"))
	ch := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			ch <- scanner.Text()
		} else {
			ch <- ""
		}
	}()
	select {
	case resp := <-ch:
		// Got response — kill immediately, don't wait for 5s timeout.
		cmd.Process.Kill()
		cmd.Wait()
		if strings.HasPrefix(resp, "= ") {
			return strings.TrimSpace(strings.TrimPrefix(resp, "= ")), nil
		}
		return "", fmt.Errorf("bad GTP response: %s", resp)
	case <-ctx.Done():
		cmd.Process.Kill()
		cmd.Wait()
		return "", fmt.Errorf("timeout")
	}
}
