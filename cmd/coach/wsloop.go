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
	"sync/atomic"
	"time"

	"github.com/neoliv/arena/internal/coach"
	"nhooyr.io/websocket"
)

type connRef struct{ v atomic.Value } // stores *websocket.Conn

func (c *connRef) get() *websocket.Conn {
	if v := c.v.Load(); v != nil {
		return v.(*websocket.Conn)
	}
	return nil
}
func (c *connRef) set(conn *websocket.Conn) { c.v.Store(conn) }

func runWSLoop(ctx context.Context, cfg *config, cfgMu *sync.RWMutex, running map[string]*runningEngine, healthErrors map[string]string, logDir string) {
	var cr connRef
	var lastAckID int

	connect := func() (*websocket.Conn, error) {
		wsURL := strings.Replace(cfg.ArenaURL, "https://", "wss://", 1) + "/api/coach/ws"
		conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": []string{"Bearer " + cfg.Token}},
		})
		return conn, err
	}
	sendMsg := func(msg coach.CoachMessage) {
		conn := cr.get()
		if conn == nil {
			return
		}
		data, _ := json.Marshal(msg)
		conn.Write(ctx, websocket.MessageText, data)
	}

	// sendHeartbeat sends current coach state immediately.
	sendHeartbeat := func() {
		var players []coach.PlayerStatus
		instCounts := make(map[string]int)
		for _, re := range running {
			instCounts[re.ai.Name+":"+re.ai.Version]++
		}
		for key, count := range instCounts {
			parts := strings.SplitN(key, ":", 2)
			players = append(players, coach.PlayerStatus{Name: parts[0], Version: parts[1], Instances: count})
		}
		sendMsg(coach.CoachMessage{Type: "heartbeat", Players: players, AckID: lastAckID})
	}
	// heartbeatNeeded triggers an immediate heartbeat after state changes.
	heartbeatNeeded := make(chan struct{}, 1)
	notifyHeartbeat := func() {
		select {
		case heartbeatNeeded <- struct{}{}:
		default:
		}
	}

	register := func() {
		cfgMu.RLock()
		infos := make([]coach.EngineInfo, len(cfg.AIs))
		for i, ai := range cfg.AIs {
			infos[i] = coach.EngineInfo{
				Name: ai.Name, Version: ai.Version, Cores: ai.Cores, MemMB: ai.MemoryMB,
				MaxInstances: ai.MaxInstances, RunCmd: ai.RunCmd, EngineID: ai.EngineID,
			}
		}
		cfgMu.RUnlock()
		sendMsg(coach.CoachMessage{
			Type: "register", CoachID: cfg.CoachID, Cores: totalCores(*cfg), MemMB: totalMem(*cfg),
			Engines: infos, AckID: lastAckID,
		})
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
	cr.set(conn)
	register()
	slog.Info("registered with arena via ws", "engines", len(cfg.AIs))

	// Heartbeat goroutine: sends on state change, or every 10s as fallback.
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-heartbeatNeeded:
				sendHeartbeat()
			case <-ticker.C:
				sendHeartbeat()
			}
		}
	}()

	go coreSampler(ctx, *cfg, &sync.Mutex{}, running)

	reconnect := func() {
		killAllRunning(&sync.Mutex{}, running)
		for {
			old := cr.get()
			if old != nil {
				old.Close(websocket.StatusNormalClosure, "reconnect")
			}
			var err error
			conn, err = connect()
			if err != nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
				continue
			}
			cr.set(conn)
			register()
			return
		}
	}

	for {
		_, msgBytes, err := conn.Read(ctx)
		if err != nil {
			slog.Warn("ws disconnected, reconnecting", "err", err)
			reconnect()
			conn = cr.get()
			if conn == nil {
				return
			}
			continue
		}
		var mmMsg coach.MMMessage
		if err := json.Unmarshal(msgBytes, &mmMsg); err != nil {
			continue
		}
		// Track last processed command ID for heartbeat ack.
		if mmMsg.ID > lastAckID {
			lastAckID = mmMsg.ID
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
			notifyHeartbeat() // state changed

			engineKey := ai.Name + ":" + ai.Version
			go func(sid string, eKey string) {
				err := re.cmd.Wait()
				re.killMu.Lock()
				kr := re.killReason
				re.killMu.Unlock()
				switch {
				case kr == "timeout":
					slog.Warn("engine timeout", "session", sid)
					sendMsg(coach.CoachMessage{Type: "engine_timeout", Session: sid, Engine: eKey})
				case kr == "nopartner" || kr == "shutdown":
					slog.Warn("engine killed (infra)", "session", sid)
					sendMsg(coach.CoachMessage{Type: "engine_exited", Session: sid, OK: true, Engine: eKey})
				case err != nil:
					slog.Warn("engine crash", "session", sid, "err", err)
					sendMsg(coach.CoachMessage{Type: "engine_crash", Session: sid, Error: err.Error(), Engine: eKey})
				default:
					slog.Info("engine exited cleanly", "session", sid)
					sendMsg(coach.CoachMessage{Type: "engine_exited", Session: sid, OK: true, Engine: eKey})
				}
				re.cancel()
				delete(running, sid)
				notifyHeartbeat() // state changed
			}(mmMsg.Session, engineKey)

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
