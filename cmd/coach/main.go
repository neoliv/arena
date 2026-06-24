// Coach runs on contributor machines. It manages AI lifecycles:
// registers available AIs with the Arena, polls for match assignments,
// launches engines as subprocesses, and bridges stdin/stdout to a WebSocket GTP relay.
package main

import (
	"bufio"
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
	"nhooyr.io/websocket"

	"github.com/neoliv/arena/internal/version"
)

type config struct {
	CoachID    string     `yaml:"coach_id"`
	Token      string     `yaml:"token"`
	Label      string     `yaml:"label"`
	ArenaURL   string     `yaml:"arena_url"`
	MaxCores   int        `yaml:"max_cores"`
	MaxRAMMB   int        `yaml:"max_ram_mb"`
	EnginesDir string     `yaml:"engines_dir"`
		ShareDir   string     `yaml:"share_dir"`
	AIs        []aiConfig `yaml:"-"` // populated from engines dirs
}

type aiConfig struct {
	Name           string `yaml:"name"`
	Version        string `yaml:"version"`
	Created        string `yaml:"created"`
	ChangelogShort string `yaml:"changelog_short"`
	ChangelogFull  string `yaml:"changelog_full"`
	BuildCmd       string `yaml:"build"`
	Binary         string `yaml:"binary"`
	Args           string `yaml:"args"`
	Cores          int    `yaml:"cores"`
	MemoryMB       int    `yaml:"memory_mb"`
	MaxInstances   int    `yaml:"max_concurrency"`
	EngineID       string `yaml:"-"` // computed at load time
	EngineManifest string `yaml:"-"` // computed at load time
	RunCmd         string `yaml:"-"` // resolved full command
}

type runningEngine struct {
	ai          aiConfig
	cmd         *exec.Cmd
	cancel      context.CancelFunc
	sessionID   string
	stderrBuf   *bytes.Buffer
	stderrPipeW *io.PipeWriter
}

var coachSession string

func main() {
	b := make([]byte, 8); crand.Read(b); coachSession = hex.EncodeToString(b)
	configPath := flag.String("config", "coach.yaml", "Path to coach config file")
	playersDir := flag.String("players", "players.d", "Directory containing player .yaml files")
	showVer    := flag.Bool("version", false, "Print version and exit")
	aisFilter  := flag.String("ais", "", "Comma-separated list of AI names to load from coach.d/ (default: all)")
	handleShortFlags("coach")
	flag.Parse()

	logDir := filepath.Join(os.Getenv("HOME"), "dev", "agent", "arena", "log")
	os.MkdirAll(logDir, 0755)
	if lf, err := os.Create(filepath.Join(logDir, "coach.log")); err == nil {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, lf), &slog.HandlerOptions{Level: slog.LevelInfo})))
	}
	slog.Info("coach starting", "pid", os.Getpid(), "log_dir", logDir)

	if *showVer {
		fmt.Print(version.PrintVersion("coach"))
		return
	}
	allowedAIs := make(map[string]bool)
	if *aisFilter != "" {
		for _, name := range strings.Split(*aisFilter, ",") {
			allowedAIs[strings.TrimSpace(name)] = true
		}
	}

	data, err := os.ReadFile(*configPath)
	if err != nil {
		slog.Error("read config", "path", *configPath, "err", err)
		os.Exit(1)
	}
	var cfg config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		slog.Error("parse config", "err", err)
		os.Exit(1)
	}
	if cfg.CoachID == "" { slog.Error("coach_id is required"); os.Exit(1) }
	if cfg.ArenaURL == "" { cfg.ArenaURL = "https://arena.arsac.org" }
	tokenSource := "coach.yaml"
	if cfg.Token == "" { cfg.Token = os.Getenv("ARENA_TOKEN"); tokenSource = "ARENA_TOKEN env" }
	if cfg.Token == "" {
		slog.Error("NO TOKEN CONFIGURED — set token in coach.yaml or ARENA_TOKEN env var")
	} else {
		obs := cfg.Token[:min(4,len(cfg.Token))] + "..." + cfg.Token[max(0,len(cfg.Token)-4):]
		slog.Info("using token", "source", tokenSource, "token", obs)
	}

	client := &http.Client{Timeout: 30 * time.Second}

	enginesDir := cfg.EnginesDir
	if enginesDir == "" {
		enginesDir = *playersDir
	}
	if enginesDir == "players.d" {
		enginesDir = "~/coach/engines"
	}
	if strings.HasPrefix(enginesDir, "~/") {
		enginesDir = filepath.Join(os.Getenv("HOME"), enginesDir[2:])
	}
	slog.Info("scanning for players", "engines_dir", enginesDir)

	if cfg.ShareDir == "" {
		cfg.ShareDir = filepath.Join(os.Getenv("HOME"), "coach", "share")
	}
	if strings.HasPrefix(cfg.ShareDir, "~/") {
		cfg.ShareDir = filepath.Join(os.Getenv("HOME"), cfg.ShareDir[2:])
	}
	slog.Info("share dir", "share_dir", cfg.ShareDir)

	var mu sync.Mutex           // protects running map
	var cfgMu sync.RWMutex      // protects cfg.AIs slice
	running := make(map[string]*runningEngine)

	loadAndRegister := func() {
		var ais []aiConfig
		matches, err := filepath.Glob(filepath.Join(enginesDir, "*", "players.d", "*.yaml"))
		if err == nil {
			for _, yamlPath := range matches {
				engineDir := filepath.Dir(filepath.Dir(yamlPath))
				aiData, err := os.ReadFile(yamlPath)
				if err != nil { slog.Warn("read ai config", "file", yamlPath, "err", err); continue }
				var ai aiConfig
				if err := yaml.Unmarshal(aiData, &ai); err != nil {
					slog.Warn("parse ai config", "file", yamlPath, "err", err); continue
				}
				if ai.Name == "" || ai.Version == "" { continue }
				if len(allowedAIs) > 0 && !allowedAIs[ai.Name] { continue }
				if ai.Binary != "" && !strings.HasPrefix(ai.Binary, "/") {
					ai.Binary = filepath.Join(engineDir, ai.Binary)
				}
				ai.RunCmd = strings.TrimSpace(ai.Binary + " " + ai.Args)
				ai.EngineID, ai.EngineManifest = computeEngineIdentity(ai)
				ais = append(ais, ai)
			}
		}
		if len(ais) == 0 { slog.Error("no players found in " + enginesDir + "/*/players.d/*.yaml"); return }
		slog.Info("loaded AIs", "count", len(ais))
		cfgMu.Lock()
		cfg.AIs = ais
		cfgMu.Unlock()
		slog.Info("registering with arena", "url", cfg.ArenaURL, "ais", len(ais))
		for _, a := range ais {
			slog.Info("  player", "name", a.Name, "version", a.Version, "binary", a.Binary, "args", a.Args, "engine_id", a.EngineID[:min(16,len(a.EngineID))])
		}
		if err := register(client, cfg, &cfgMu); err != nil {
			slog.Error("REGISTRATION FAILED", "err", err)
		} else {
			slog.Info("REGISTRATION SUCCEEDED", "ais", len(ais))
		}
	}
	loadAndRegister()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-ctx.Done():
				signal.Stop(sighup)
				return
			case <-sighup:
				slog.Info("SIGHUP received, reloading config")
				loadAndRegister()
			}
		}
	}()

	go heartbeatLoop(ctx, client, cfg, &mu, &cfgMu, running, loadAndRegister)

	slog.Info("starting task poll loop")
	lastReReg := time.Now().Add(-5 * time.Minute)
	for ctx.Err() == nil {
		if time.Since(lastReReg) > 5*time.Minute {
			loadAndRegister()
			lastReReg = time.Now()
		}
		tasks := pollTasks(client, cfg)
		for _, t := range tasks {
			cfgMu.RLock()
			ai := findAI(cfg, t.EngineName, t.EngineVersion)
			cfgMu.RUnlock()
			if ai == nil {
				slog.Warn("task for unknown AI", "name", t.EngineName, "version", t.EngineVersion)
				continue
			}

			mu.Lock()
			if _, exists := running[t.SessionID]; exists {
				mu.Unlock()
				slog.Warn("duplicate session, skipping", "session", t.SessionID)
				continue
			}
			usedCores, usedMem := 0, 0
			instCount := 0
			for _, re := range running {
				usedCores += re.ai.Cores
				usedMem += re.ai.MemoryMB
				if re.ai.Name == ai.Name && re.ai.Version == ai.Version {
					instCount++
				}
			}
			if instCount >= ai.MaxInstances || usedCores+ai.Cores > totalCores(cfg) || usedMem+ai.MemoryMB > totalMem(cfg) {
				mu.Unlock()
				slog.Info("declining task (at capacity)", "ai", ai.Name,
					"instances", instCount, "max_instances", ai.MaxInstances,
					"cores_used", usedCores, "cores_needed", ai.Cores, "cores_total", totalCores(cfg),
					"mem_used", usedMem, "mem_needed", ai.MemoryMB, "mem_total", totalMem(cfg))
				declineTask(client, cfg, t.AssignmentID, fmt.Sprintf("at capacity: %d/%d instances, %d/%d cores, %d/%d MB for %s",
					instCount, ai.MaxInstances, usedCores, totalCores(cfg), usedMem, totalMem(cfg), ai.Name))
				continue
			}
			mu.Unlock()

			acceptTask(client, cfg, t.AssignmentID)
			aiCopy := *ai
			gameSecs := parseGameTime(t.TimeControl)
			if gameSecs > 0 {
				aiCopy.RunCmd = strings.Replace(aiCopy.RunCmd, "%game_time%", fmt.Sprintf("%.0f", gameSecs), -1)
				aiCopy.RunCmd = strings.Replace(aiCopy.RunCmd, "%share_dir%", cfg.ShareDir, -1)
			}
			re, err := launchEngine(ctx, aiCopy, cfg.ArenaURL, t.RelayPath, t.SessionID, logDir, gameSecs, t.NumGames)
			if err != nil {
				slog.Error("launch engine", "ai", ai.Name, "err", err)
				failTask(client, cfg, t.AssignmentID, "launch failed: "+err.Error())
				continue
			}

			mu.Lock()
			running[t.SessionID] = re
			mu.Unlock()

			readyTask(client, cfg, t.AssignmentID, t.SessionID)
			sendHeartbeat(client, cfg, &mu, &cfgMu, running)

			go func(sid string, aid int) {
				err := re.cmd.Wait()
				stderrOut := strings.TrimSpace(re.stderrBuf.String())
				if err != nil {
					slog.Warn("engine exited with error", "session", sid, "err", err, "stderr", stderrOut)
				} else if stderrOut != "" {
					slog.Info("engine exited", "session", sid, "stderr", stderrOut)
				} else {
					slog.Info("engine exited", "session", sid)
				}
				mu.Lock()
				delete(running, sid)
				mu.Unlock()
				sendHeartbeat(client, cfg, &mu, &cfgMu, running)
			}(t.SessionID, t.AssignmentID)
		}
		select {
		case <-ctx.Done():
			break
		case <-time.After(5 * time.Second):
		}
	}

	mu.Lock()
	for _, re := range running {
		re.cancel()
		re.cmd.Process.Kill()
	}
	mu.Unlock()
	slog.Info("coach shutting down")
}

// ── HTTP helpers ─────────────────────────────────────────────────────────

func postJSON(client *http.Client, cfg config, path string, body any) (*http.Response, error) {
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(body)
	req, _ := http.NewRequest("POST", cfg.ArenaURL+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	return client.Do(req)
}

func register(client *http.Client, cfg config, cfgMu *sync.RWMutex) error {
	type aiReg struct {
		Name             string `json:"name"`
		Version          string `json:"version"`
		Created          string `json:"created"`
		ChangelogShort   string `json:"changelog_short"`
		ChangelogFull    string `json:"changelog_full"`
		BuildCmd         string `json:"build_cmd"`
		RunCmd           string `json:"run_cmd"`
		EngineID         string `json:"engine_id"`
		EngineManifest   string `json:"engine_manifest"`
		ResourceCores    int    `json:"resource_cores"`
		ResourceMemoryMB int    `json:"resource_memory_mb"`
		MaxConcurrency   int    `json:"max_concurrency"`
	}
	body := map[string]any{
		"coach_id": cfg.CoachID, "token": cfg.Token, "label": cfg.Label, "version": version.Version,
		"resources": map[string]int{"cores": totalCores(cfg), "memory_mb": totalMem(cfg)},
	}
	cfgMu.RLock()
	var ais []aiReg
	for _, a := range cfg.AIs {
		cores := a.Cores; if cores == 0 { cores = 1 }
		mem := a.MemoryMB; if mem == 0 { mem = 64 }
		maxInst := a.MaxInstances; if maxInst == 0 { maxInst = 1 }
		ais = append(ais, aiReg{a.Name, a.Version, a.Created, a.ChangelogShort, a.ChangelogFull, a.BuildCmd, a.RunCmd, a.EngineID, a.EngineManifest, cores, mem, maxInst})
	}
	cfgMu.RUnlock()
	body["ais"] = ais
	resp, err := postJSON(client, cfg, "/api/coach/register", body)
	if err != nil { return fmt.Errorf("register POST failed: %w", err) }
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var errResp map[string]string
		bodyBytes, _ := io.ReadAll(resp.Body)
		json.Unmarshal(bodyBytes, &errResp)
		slog.Error("registration failed", "status", resp.StatusCode, "body", string(bodyBytes))
		return fmt.Errorf("register: %s (%s)", resp.Status, errResp["error"])
	}
	return nil
}

func sendHeartbeat(client *http.Client, cfg config, mu *sync.Mutex, cfgMu *sync.RWMutex, running map[string]*runningEngine) {
	mu.Lock()
	var ais []map[string]any
	counts := map[string]int{}
	usedCores, usedMem := 0, 0
	for _, re := range running {
		key := re.ai.Name + ":" + re.ai.Version
		counts[key]++
		usedCores += re.ai.Cores
		usedMem += re.ai.MemoryMB
	}
	mu.Unlock()
	cfgMu.RLock()
	for _, ai := range cfg.AIs {
		key := ai.Name + ":" + ai.Version
		count := counts[key]
		ais = append(ais, map[string]any{
			"name": ai.Name, "version": ai.Version,
			"current_matches": count, "max_concurrency": ai.MaxInstances,
		})
	}
	cfgMu.RUnlock()
	body := map[string]any{
		"coach_id": cfg.CoachID, "token": cfg.Token, "session_id": coachSession,
		"ais_available": ais,
		"resources": map[string]int{"cores_used": usedCores, "memory_mb_used": usedMem},
	}
	resp, err := postJSON(client, cfg, "/api/coach/heartbeat", body)
	if err != nil { return }
	resp.Body.Close()
}

func heartbeatLoop(ctx context.Context, client *http.Client, cfg config, mu *sync.Mutex, cfgMu *sync.RWMutex, running map[string]*runningEngine, reload func()) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	var lastServerGen string
	for {
		select {
		case <-ctx.Done(): return
		case <-ticker.C:
		}
		mu.Lock()
		counts := map[string]int{}
		usedCores, usedMem := 0, 0
		for _, re := range running {
			key := re.ai.Name + ":" + re.ai.Version
			counts[key]++
			usedCores += re.ai.Cores
			usedMem += re.ai.MemoryMB
		}
		mu.Unlock()
		cfgMu.RLock()
		var ais []map[string]any
		for _, ai := range cfg.AIs {
			key := ai.Name + ":" + ai.Version
			count := counts[key]
			ais = append(ais, map[string]any{
				"name": ai.Name, "version": ai.Version,
				"current_matches": count, "max_concurrency": ai.MaxInstances,
			})
		}
		cfgMu.RUnlock()
		body := map[string]any{
			"coach_id": cfg.CoachID, "token": cfg.Token, "session_id": coachSession,
			"ais_available": ais,
			"resources": map[string]int{"cores_used": usedCores, "memory_mb_used": usedMem},
		}
		resp, err := postJSON(client, cfg, "/api/coach/heartbeat", body)
		if err != nil { slog.Warn("heartbeat failed", "err", err); continue }
		var hb struct {
			ServerGen string `json:"server_gen"`
		}
		json.NewDecoder(resp.Body).Decode(&hb)
		resp.Body.Close()
		if hb.ServerGen != "" && hb.ServerGen != lastServerGen {
			slog.Info("server restart detected, re-registering", "server_gen", hb.ServerGen[:min(8, len(hb.ServerGen))])
			lastServerGen = hb.ServerGen
			reload()
		}
	}
}

type taskItem struct {
	AssignmentID  int    `json:"assignment_id"`
	EngineName    string `json:"engine_name"`
	EngineVersion string `json:"engine_version"`
	TimeControl   string `json:"time_control"`
	NumGames      int    `json:"num_games"`
	SessionID     string `json:"session_id"`
	RelayPath     string `json:"relay_path"`
}

func pollTasks(client *http.Client, cfg config) []taskItem {
	req, _ := http.NewRequest("GET", cfg.ArenaURL+"/api/coach/tasks?coach_id="+cfg.CoachID, nil)
	if cfg.Token != "" { req.Header.Set("Authorization", "Bearer "+cfg.Token) }
	resp, err := client.Do(req)
	if err != nil { return nil }
	defer resp.Body.Close()
	var result struct{ Tasks []taskItem }
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Tasks
}

func acceptTask(client *http.Client, cfg config, id int) {
	postJSON(client, cfg, fmt.Sprintf("/api/coach/tasks/%d/status", id), map[string]string{
		"coach_id": cfg.CoachID, "token": cfg.Token, "status": "accepted",
	})
}

func declineTask(client *http.Client, cfg config, id int, reason string) {
	postJSON(client, cfg, fmt.Sprintf("/api/coach/tasks/%d/status", id), map[string]string{
		"coach_id": cfg.CoachID, "token": cfg.Token, "status": "declined", "reason": reason,
	})
}

func readyTask(client *http.Client, cfg config, id int, sessionID string) {
	postJSON(client, cfg, fmt.Sprintf("/api/coach/tasks/%d/status", id), map[string]string{
		"coach_id": cfg.CoachID, "token": cfg.Token, "status": "ready", "session_id": sessionID,
	})
}

func failTask(client *http.Client, cfg config, id int, reason string) {
	postJSON(client, cfg, fmt.Sprintf("/api/coach/tasks/%d/status", id), map[string]string{
		"coach_id": cfg.CoachID, "token": cfg.Token, "status": "failed", "reason": reason,
	})
}

// ── Engine lifecycle ─────────────────────────────────────────────────────

func launchEngine(ctx context.Context, ai aiConfig, arenaURL, relayPath, sessionID, logDir string, gameTimeSec float64, numGames int) (*runningEngine, error) {
	parts := strings.Fields(ai.RunCmd)
	if len(parts) == 0 { return nil, fmt.Errorf("empty run command") }

	engCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(engCtx, parts[0], parts[1:]...)
	cmd.Dir = filepath.Dir(parts[0])
	var stderrBuf bytes.Buffer
	stderrPipeR, stderrPipeW := io.Pipe()
	engineLogDir := filepath.Join(logDir, "engines")
	os.MkdirAll(engineLogDir, 0755)
	errLog, _ := os.Create(filepath.Join(engineLogDir, sessionID+".err"))
	stderrWriters := io.MultiWriter(os.Stderr, &stderrBuf, stderrPipeW)
	if errLog != nil { stderrWriters = io.MultiWriter(stderrWriters, errLog) }
	cmd.Stderr = stderrWriters

	// Parse edax search-log from stderr, store stats for the
	// timing goroutine to merge into a single stats line.
	var searchMu sync.Mutex
	var searchNodes int64
	var searchDepth int
	var searchScore int
	go func() {
		defer stderrPipeR.Close()
		scanner := bufio.NewScanner(stderrPipeR)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.Contains(line, "BEST MOVE FOUND") {
				continue
			}
			var n int64; var d, s, dummy int
			if _, err := fmt.Sscanf(line, "%d> => BEST MOVE FOUND! level = %d@", &dummy, &d); err == nil {
				if idx := strings.Index(line, "score = "); idx >= 0 {
					fmt.Sscanf(line[idx:], "score = %d", &s)
				}
				if idx := strings.Index(line, "nodes = "); idx >= 0 {
					fmt.Sscanf(line[idx:], "nodes = %d N", &n)
				}
				searchMu.Lock()
				searchNodes, searchDepth, searchScore = n, d, s
				searchMu.Unlock()
			}
		}
	}()
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()

	slog.Info("launching engine", "binary", parts[0], "args", strings.Join(parts[1:], " "), "dir", cmd.Dir, "session", sessionID)

	if err := cmd.Start(); err != nil {
		cancel()
		slog.Error("engine start failed", "binary", parts[0], "err", err, "stderr", stderrBuf.String())
		return nil, fmt.Errorf("start: %w", err)
	}

	// Pre-flight GTP health check — catch engines that crash on startup
	// before telling the server we're ready. Read byte-by-byte so the
	// WS bridging scanner gets all remaining stdout bytes.
	stdin.Write([]byte("name\n"))
	healthCh := make(chan string, 1)
	go func() {
		line := make([]byte, 0, 256)
		for {
			var b [1]byte
			if _, err := stdout.Read(b[:]); err != nil || b[0] == '\n' {
				break
			}
			line = append(line, b[0])
		}
		healthCh <- string(line)
	}()
	select {
	case resp := <-healthCh:
		if !strings.HasPrefix(resp, "= ") {
			cmd.Process.Kill()
			cancel()
			slog.Error("engine health check failed", "session", sessionID, "response", resp, "stderr", stderrBuf.String())
			return nil, fmt.Errorf("health check failed: %s", resp)
		}
		slog.Info("engine health check OK", "session", sessionID, "name", strings.TrimPrefix(resp, "= "))
	case <-time.After(3 * time.Second):
		cmd.Process.Kill()
		cancel()
		stderrPipeW.Close()
		return nil, fmt.Errorf("health check timeout")
	}

	wsURL := strings.Replace(arenaURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	wsURL += relayPath

	wsCtx, wsCancel := context.WithTimeout(ctx, 30*time.Second)
	conn, _, err := websocket.Dial(wsCtx, wsURL, &websocket.DialOptions{})
	wsCancel()
	if err != nil {
		stderrPipeW.Close()
		cmd.Process.Kill()
		cancel()
		return nil, fmt.Errorf("ws dial: %w", err)
	}


	// GTP-aware timing: track genmove wall-clock time and enforce
	// the time budget. If the engine exceeds gameTimeSec * 1.05
	// total thinking time, kill it. The arena no longer sends
	// game_time GTP commands — time enforcement is coach-side.
	var timingMu sync.Mutex
	var genmoveStart time.Time
	var totalThinkMs int64
	budgetMs := int64(gameTimeSec * 1000 * 105 / 100) // 5% margin per game
	if numGames > 0 {
		budgetMs = int64(gameTimeSec * 1000 * 105 / 100 * float64(numGames))
	}
	engineTimedOut := false

	// stdout → WS: track genmove response timing and always
	// emit a stats line with real wall-clock time.
	var lastElapsedMs int64
	statsSent := false
	go func() {
		defer conn.Close(websocket.StatusNormalClosure, "done")
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			raw := scanner.Bytes()
			line := string(raw)

			// Engine # stats: prepend measured time and forward.
			if strings.HasPrefix(line, "#") {
				timingMu.Lock()
				ms := lastElapsedMs
				timingMu.Unlock()
				raw = []byte(fmt.Sprintf("# time_ms %d %s", ms, strings.TrimPrefix(line, "# ")))
			}

			var injectLine string
			timingMu.Lock()
			if !genmoveStart.IsZero() && strings.HasPrefix(line, "= ") && !strings.HasPrefix(line, "= nodes ") && len(strings.TrimPrefix(line, "= ")) > 0 {
				lastElapsedMs = time.Since(genmoveStart).Milliseconds()
				totalThinkMs += lastElapsedMs
				genmoveStart = time.Time{}
				statsSent = false
				if totalThinkMs > budgetMs {
					engineTimedOut = true
					timingMu.Unlock()
					slog.Warn("engine time budget exceeded", "session", sessionID, "total_ms", totalThinkMs, "budget_ms", budgetMs)
					cmd.Process.Kill()
					return
				}
				// Inject timing + optional search-log data.
				searchMu.Lock()
				sn, sd, ss := searchNodes, searchDepth, searchScore
				searchNodes, searchDepth, searchScore = 0, 0, 0
				searchMu.Unlock()
				injectLine = fmt.Sprintf("# time_ms %d nodes %d depth %d score %d timeout false", lastElapsedMs, sn, sd, ss)
			}
			// If engine sent its own stats, enrich with real time.
			if strings.HasPrefix(line, "= nodes ") {
				injectLine = "" // engine provided data, skip injection
				rewritten := fmt.Sprintf("# time_ms %d nodes %s",
					lastElapsedMs, strings.TrimPrefix(line, "= nodes "))
				raw = []byte(rewritten)
				statsSent = true
			}
			timingMu.Unlock()
			if err := conn.Write(context.Background(), websocket.MessageText, raw); err != nil {
				break
			}
			if injectLine != "" {
				timingMu.Lock()
				sent := statsSent
				timingMu.Unlock()
				if !sent {
					conn.Write(context.Background(), websocket.MessageText, []byte(injectLine))
				}
			}
		}
	}()

	// WS → stdin: detect genmove commands to start the clock
	go func() {
		defer stdin.Close()
		for {
			_, msg, err := conn.Read(context.Background())
			if err != nil { break }
			cmdStr := string(msg)
			if strings.HasPrefix(cmdStr, "genmove") {
				timingMu.Lock()
				genmoveStart = time.Now()
				timingMu.Unlock()
			}
			io.WriteString(stdin, cmdStr+"\n")
		}
	}()

	re := &runningEngine{ai: ai, cmd: cmd, cancel: cancel, sessionID: sessionID, stderrBuf: &stderrBuf, stderrPipeW: stderrPipeW}

	// Watchdog: if the engine doesn't respond at all within
	// 2x the per-game budget, kill it.
	if gameTimeSec > 0 {
		wdSec := int(gameTimeSec * 2)
		if numGames > 0 {
			wdSec = int(gameTimeSec * 2 * float64(numGames))
		}
		if wdSec < 10 {
			wdSec = 10
		}
		go func() {
			timer := time.NewTimer(time.Duration(wdSec) * time.Second)
			defer timer.Stop()
			select {
			case <-engCtx.Done():
				return
			case <-timer.C:
			}
			timingMu.Lock()
			timedOut := engineTimedOut
			timingMu.Unlock()
			if !timedOut {
				slog.Warn("engine watchdog expired", "session", sessionID, "seconds", wdSec)
				cmd.Process.Kill()
			}
		}()
	}

	return re, nil
}

// ── Helpers ──────────────────────────────────────────────────────────────

func parseGameTime(tcJSON string) float64 {
	if tcJSON == "" {
		return 0
	}
	var tc struct {
		Seconds float64 `json:"seconds"`
	}
	if err := json.Unmarshal([]byte(tcJSON), &tc); err != nil {
		return 0
	}
	return tc.Seconds
}

func findAI(cfg config, name, version string) *aiConfig {
	for i := range cfg.AIs {
		if cfg.AIs[i].Name == name && cfg.AIs[i].Version == version {
			return &cfg.AIs[i]
		}
	}
	return nil
}

func totalCores(cfg config) int {
	if cfg.MaxCores > 0 { return cfg.MaxCores }
	return 1
}

func computeEngineIdentity(ai aiConfig) (string, string) {
	parts := strings.Fields(ai.RunCmd)
	if len(parts) == 0 { return "", "" }
	binPath := parts[0]
	var manifest strings.Builder
	fmt.Fprintf(&manifest, "Engine: %s %s\n", ai.Name, ai.Version)
	if ai.Created != "" { fmt.Fprintf(&manifest, "Date: %s\n", ai.Created) }
	if ai.ChangelogShort != "" { fmt.Fprintf(&manifest, "Changes: %s\n", ai.ChangelogShort) }
	fmt.Fprintf(&manifest, "Command: %s\n", ai.RunCmd)
	fmt.Fprintf(&manifest, "Resources: %d core(s), %d MB\n\n", ai.Cores, ai.MemoryMB)

	hasher := sha256.New()
	if data, err := os.ReadFile(binPath); err == nil {
		info, _ := os.Stat(binPath)
		h := sha256.Sum256(data)
		hasher.Write(h[:])
		fmt.Fprintf(&manifest, "Binary: %s\n  Size: %s\n  Modified: %s\n  SHA256: %s\n\n",
			binPath, niceSize(info.Size()), info.ModTime().Format("2006-01-02 15:04"), hex.EncodeToString(h[:])[:16])
	} else {
		fmt.Fprintf(&manifest, "Binary: %s (not found)\n\n", binPath)
	}

	dirs := []string{filepath.Dir(binPath), filepath.Join(filepath.Dir(binPath), "..", "data"),
		filepath.Join(filepath.Dir(binPath), "..", "Lib"), filepath.Join(filepath.Dir(binPath), "..", "Database")}
	seen := map[string]bool{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil { continue }
		for _, e := range entries {
			if e.IsDir() { continue }
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if ext == ".brn" || ext == ".bin" || ext == ".safetensors" || ext == ".raw" || ext == ".dat" || ext == ".txt" {
				if seen[e.Name()] { continue }
				seen[e.Name()] = true
				path := filepath.Join(dir, e.Name())
				data, err := os.ReadFile(path)
				if err != nil { continue }
				info, _ := os.Stat(path)
				h := sha256.Sum256(data)
				hasher.Write(h[:])
				fmt.Fprintf(&manifest, "Data: %s\n  Size: %s\n  Modified: %s\n  SHA256: %s\n\n",
					path, niceSize(info.Size()), info.ModTime().Format("2006-01-02 15:04"), hex.EncodeToString(h[:])[:16])
			}
		}
	}

	engineID := hex.EncodeToString(hasher.Sum(nil))[:16]
	manifest.WriteString(fmt.Sprintf("Engine ID: %s\n", engineID))
	return engineID, manifest.String()
}

func niceSize(n int64) string {
	suf := []string{"B","KB","MB","GB","TB"}
	f := float64(n); i := 0
	for i < len(suf)-1 && f >= 995 { f /= 1024; i++ }
	if f < 10 { return fmt.Sprintf("%.1f %s", f, suf[i]) }
	return fmt.Sprintf("%.0f %s", f, suf[i])
}

func totalMem(cfg config) int {
	if cfg.MaxRAMMB > 0 { return cfg.MaxRAMMB }
	return 256
}

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
