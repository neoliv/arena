package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/neoliv/arena/internal/coach"
	"nhooyr.io/websocket"
)

// runWSLoop connects to the arena via WebSocket, registers engines, sends
// heartbeats, and executes launch/kill commands from the MM. Replaces the
// old poll-based main loop.
func runWSLoop(ctx context.Context, cfg *config, cfgMu *sync.RWMutex, running map[string]*runningEngine, healthErrors map[string]string, logDir string) {
	connect := func() (*websocket.Conn, error) {
		wsURL := strings.Replace(cfg.ArenaURL, "https://", "wss://", 1) + "/api/coach/ws"
		conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": []string{"Bearer " + cfg.Token}},
		})
		return conn, err
	}
	sendMsg := func(conn *websocket.Conn, msg coach.CoachMessage) {
		data, _ := json.Marshal(msg)
		conn.Write(ctx, websocket.MessageText, data)
	}
	register := func(conn *websocket.Conn) {
		cfgMu.RLock()
		infos := make([]coach.EngineInfo, len(cfg.AIs))
		for i, ai := range cfg.AIs {
			infos[i] = coach.EngineInfo{
				Name: ai.Name, Version: ai.Version, Cores: ai.Cores, MemMB: ai.MemoryMB,
				MaxInstances: ai.MaxInstances, RunCmd: ai.RunCmd, EngineID: ai.EngineID,
			}
		}
		sendMsg(conn, coach.CoachMessage{
			Type: "register", CoachID: cfg.CoachID, Cores: totalCores(*cfg), MemMB: totalMem(*cfg),
			Engines: infos,
		})
		cfgMu.RUnlock()
	}

	var conn *websocket.Conn
	for {
		var err error
		conn, err = connect()
		if err != nil {
			slog.Warn("ws connect failed, retrying in 5s", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		break
	}
	defer conn.Close(websocket.StatusNormalClosure, "shutdown")
	register(conn)
	slog.Info("registered with arena via ws", "engines", len(cfg.AIs))

	// Heartbeat every 10s
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var players []coach.PlayerStatus
				instCounts := make(map[string]int)
				for _, re := range running {
					instCounts[re.ai.Name+":"+re.ai.Version]++
				}
				for key, count := range instCounts {
					parts := strings.SplitN(key, ":", 2)
					players = append(players, coach.PlayerStatus{Name: parts[0], Version: parts[1], Instances: count})
				}
				sendMsg(conn, coach.CoachMessage{Type: "heartbeat", Players: players})
			}
		}
	}()

	// Main command loop
	for {
		_, msgBytes, err := conn.Read(ctx)
		if err != nil {
			slog.Warn("ws disconnected, reconnecting", "err", err)
			var killMu sync.Mutex
			killAllRunning(&killMu, running)
			for {
				conn.Close(websocket.StatusNormalClosure, "reconnect")
				var err2 error
				conn, err2 = connect()
				if err2 != nil {
					select {
					case <-ctx.Done():
						return
					case <-time.After(5 * time.Second):
					}
					continue
				}
				register(conn)
				break
			}
			continue
		}
		var mmMsg coach.MMMessage
		if err := json.Unmarshal(msgBytes, &mmMsg); err != nil {
			continue
		}

		switch mmMsg.Type {
		case "launch":
			cfgMu.RLock()
			ai := findAIByKey(*cfg, mmMsg.Engine)
			cfgMu.RUnlock()
			if ai == nil {
				slog.Warn("launch for unknown engine", "engine", mmMsg.Engine)
				continue
			}
			aiCopy := *ai
			if mmMsg.TimeControl.Seconds > 0 {
				aiCopy.RunCmd = strings.Replace(aiCopy.RunCmd, "%game_time%", fmt.Sprintf("%.0f", mmMsg.TimeControl.Seconds), -1)
				aiCopy.RunCmd = strings.Replace(aiCopy.RunCmd, "%share_dir%", cfg.ShareDir, -1)
			}
			re, err := launchEngine(ctx, aiCopy, cfg.ArenaURL, cfg.Token, mmMsg.Session, logDir, mmMsg.TimeControl.Seconds)
			if err != nil {
				slog.Error("launch engine failed", "err", err)
				continue
			}
			running[mmMsg.Session] = re

			go func(sid string, engineKey string) {
				err := re.cmd.Wait()
				re.killMu.Lock()
				kr := re.killReason
				re.killMu.Unlock()
				if err != nil {
					switch kr {
					case "timeout":
						slog.Warn("engine timeout", "session", sid)
						sendMsg(conn, coach.CoachMessage{Type: "engine_timeout", Session: sid})
					case "nopartner", "shutdown":
						slog.Warn("engine killed (no partner)", "session", sid)
						sendMsg(conn, coach.CoachMessage{Type: "engine_exited", Session: sid, OK: true})
					default:
						slog.Warn("engine crash", "session", sid, "err", err)
						sendMsg(conn, coach.CoachMessage{Type: "engine_crash", Session: sid, Error: err.Error()})
					}
				} else {
					slog.Info("engine exited cleanly", "session", sid)
					sendMsg(conn, coach.CoachMessage{Type: "engine_exited", Session: sid, OK: true})
				}
				re.cancel()
				delete(running, sid)
			}(mmMsg.Session, ai.Name+":"+ai.Version)

		case "kill":
			if re, ok := running[mmMsg.Session]; ok {
				slog.Warn("mm requested kill", "session", mmMsg.Session, "reason", mmMsg.Reason)
				re.killMu.Lock()
				if re.killReason == "" {
					re.killReason = mmMsg.Reason
				}
				re.killMu.Unlock()
				re.cancel()
				if re.cmd.Process != nil {
					re.cmd.Process.Signal(os.Kill)
				}
			}
		}
	}
}
