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
	ai        aiConfig
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	sessionID string
}

// assignment is a single side assignment from the matchmaker.
type assignment struct {
	SessionID   string `json:"session_id"`
	Engine      string `json:"engine"` // "name:version"
	Side        string `json:"side"`   // "black" or "white"
	TimeControl string `json:"time_control"`
	Opening     string `json:"opening"`
}

var coachSession string

func main() {
	b := make([]byte, 8)
	crand.Read(b)
	coachSession = hex.EncodeToString(b)
	configPath := flag.String("config", "coach.yaml", "Path to coach config file")
	playersDir := flag.String("players", "players.d", "Directory containing player .yaml files")
	showVer   := flag.Bool("version", false, "Print version and exit")
	aisFilter := flag.String("ais", "", "Comma-separated list of AI names to load from coach.d/ (default: all)")
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
	if cfg.CoachID == "" {
		slog.Error("coach_id is required")
		os.Exit(1)
	}
	if cfg.ArenaURL == "" {
		cfg.ArenaURL = "https://arena.arsac.org"
	}
	tokenSource := "coach.yaml"
	if cfg.Token == "" {
		cfg.Token = os.Getenv("ARENA_TOKEN")
		tokenSource = "ARENA_TOKEN env"
	}
	if cfg.Token == "" {
		slog.Error("NO TOKEN CONFIGURED — set token in coach.yaml or ARENA_TOKEN env var")
	} else {
		obs := cfg.Token[:min(4, len(cfg.Token))] + "..." + cfg.Token[max(0, len(cfg.Token)-4):]
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

	var mu sync.Mutex      // protects running map
	var cfgMu sync.RWMutex // protects cfg.AIs slice
	running := make(map[string]*runningEngine)

	// Tracks engines that failed to start or crashed repeatedly.
	// Cleared on successful completion. Sent to matchmaker on registration.
	healthErrors := make(map[string]string) // key: "name:version" → reason

	loadAndRegister := func() {
		var ais []aiConfig
		matches, err := filepath.Glob(filepath.Join(enginesDir, "*", "players.d", "*.yaml"))
		if err == nil {
			for _, yamlPath := range matches {
				engineDir := filepath.Dir(filepath.Dir(yamlPath))
				aiData, err := os.ReadFile(yamlPath)
				if err != nil {
					slog.Warn("read ai config", "file", yamlPath, "err", err)
					continue
				}
				var ai aiConfig
				if err := yaml.Unmarshal(aiData, &ai); err != nil {
					slog.Warn("parse ai config", "file", yamlPath, "err", err)
					continue
				}
				if ai.Name == "" || ai.Version == "" {
					continue
				}
				if len(allowedAIs) > 0 && !allowedAIs[ai.Name] {
					continue
				}
				if ai.Binary != "" && !strings.HasPrefix(ai.Binary, "/") {
					ai.Binary = filepath.Join(engineDir, ai.Binary)
				}
				ai.RunCmd = strings.TrimSpace(ai.Binary + " " + ai.Args)
				ai.EngineID, ai.EngineManifest = computeEngineIdentity(ai)
				ais = append(ais, ai)
			}
		}
		if len(ais) == 0 {
			slog.Error("no players found in " + enginesDir + "/*/players.d/*.yaml")
			return
		}
		slog.Info("loaded AIs", "count", len(ais))
		cfgMu.Lock()
		cfg.AIs = ais
		cfgMu.Unlock()

		// Register with arena DB (persistence for web dashboard)
		slog.Info("registering with arena", "url", cfg.ArenaURL, "ais", len(ais))
		for _, a := range ais {
			slog.Info("  player", "name", a.Name, "version", a.Version, "binary", a.Binary, "args", a.Args, "engine_id", a.EngineID[:min(16, len(a.EngineID))])
		}
		if err := registerWithArena(client, cfg, &cfgMu); err != nil {
			slog.Error("REGISTRATION FAILED", "err", err)
		} else {
			slog.Info("REGISTRATION SUCCEEDED", "ais", len(ais))
		}

		// Register with matchmaker (in-memory engine list for pairing)
		if err := registerWithMatchmaker(client, cfg, &cfgMu, healthErrors); err != nil {
			slog.Error("MATCHMAKER REGISTRATION FAILED", "err", err)
		} else {
			slog.Info("matchmaker registration succeeded")
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

	// notifyChange triggers an immediate heartbeat when resources change.
	notifyChange := make(chan struct{}, 1)
	go heartbeatLoop(ctx, client, cfg, &mu, &cfgMu, running, loadAndRegister, notifyChange)

	// ── Assignment poll loop ──────────────────────────────────────────
	// Replaces the old task-based push model. The coach polls the
	// matchmaker for assignments, launches engines, and bridges them
	// to the relay. The matchmaker executes the game when both sides
	// connect.
	slog.Info("starting assignment poll loop")
	lastReReg := time.Now().Add(-5 * time.Minute)
	for ctx.Err() == nil {
		if time.Since(lastReReg) > 5*time.Minute {
			loadAndRegister()
			lastReReg = time.Now()
		}

		assignments := pollAssignments(client, cfg)
		for _, a := range assignments {
			cfgMu.RLock()
			ai := findAIByKey(cfg, a.Engine)
			cfgMu.RUnlock()
			if ai == nil {
				slog.Warn("assignment for unknown AI", "engine", a.Engine)
				continue
			}

			mu.Lock()
			if _, exists := running[a.SessionID]; exists {
				mu.Unlock()
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
				slog.Debug("at capacity, skipping assignment", "ai", ai.Name,
					"instances", instCount, "max", ai.MaxInstances,
					"cores_used", usedCores, "cores_needed", ai.Cores)
				continue
			}
			mu.Unlock()

			aiCopy := *ai
			gameSecs := parseGameTime(a.TimeControl)
			if gameSecs > 0 {
				aiCopy.RunCmd = strings.Replace(aiCopy.RunCmd, "%game_time%", fmt.Sprintf("%.0f", gameSecs), -1)
				aiCopy.RunCmd = strings.Replace(aiCopy.RunCmd, "%share_dir%", cfg.ShareDir, -1)
			}

			re, err := launchEngine(ctx, aiCopy, cfg.ArenaURL, a.SessionID, logDir, gameSecs)
			if err != nil {
				slog.Error("launch engine", "ai", ai.Name, "session", a.SessionID, "err", err)
				mu.Lock()
				healthErrors[ai.Name+":"+ai.Version] = err.Error()
				mu.Unlock()
				select {
				case notifyChange <- struct{}{}:
				default:
				}
				continue
			}

			mu.Lock()
			running[a.SessionID] = re
			mu.Unlock()

			// Notify heartbeat of resource change (non-blocking).
			select {
			case notifyChange <- struct{}{}:
			default:
			}

			// Wait for engine to exit (relay closes after matchmaker finishes game)
			go func(sid string, engineKey string) {
				err := re.cmd.Wait()
				if err != nil {
					slog.Warn("engine exited with error", "session", sid, "err", err)
				} else {
					slog.Info("engine exited cleanly", "session", sid)
					// Clear any previous health errors on clean exit.
					mu.Lock()
					delete(healthErrors, engineKey)
					mu.Unlock()
				}
				re.cancel() // cleanup context
				mu.Lock()
				delete(running, sid)
				mu.Unlock()
				// Notify heartbeat of resource change (non-blocking).
				select {
				case notifyChange <- struct{}{}:
				default:
				}
			}(a.SessionID, ai.Name+":"+ai.Version)
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

// registerWithArena persists engine info to the arena database.
func registerWithArena(client *http.Client, cfg config, cfgMu *sync.RWMutex) error {
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
		cores := a.Cores
		if cores == 0 {
			cores = 1
		}
		mem := a.MemoryMB
		if mem == 0 {
			mem = 64
		}
		maxInst := a.MaxInstances
		if maxInst == 0 {
			maxInst = 1
		}
		ais = append(ais, aiReg{a.Name, a.Version, a.Created, a.ChangelogShort, a.ChangelogFull, a.BuildCmd, a.RunCmd, a.EngineID, a.EngineManifest, cores, mem, maxInst})
	}
	cfgMu.RUnlock()
	body["ais"] = ais
	resp, err := postJSON(client, cfg, "/api/coach/register", body)
	if err != nil {
		return fmt.Errorf("register POST failed: %w", err)
	}
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

// registerWithMatchmaker registers engines with the in-memory WantedList.
func registerWithMatchmaker(client *http.Client, cfg config, cfgMu *sync.RWMutex, healthErrors map[string]string) error {
	type engineReg struct {
		Name              string `json:"name"`
		Version           string `json:"version"`
		Cores             int    `json:"cores"`
		MemoryMB          int    `json:"memory_mb"`
		MaxInstances      int    `json:"max_instances"`
		Available         bool   `json:"available"`
		UnavailableReason string `json:"unavailable_reason,omitempty"`
	}
	cfgMu.RLock()
	var engines []engineReg
	for _, a := range cfg.AIs {
		key := a.Name + ":" + a.Version
		cores := a.Cores
		if cores == 0 { cores = 1 }
		mem := a.MemoryMB
		if mem == 0 { mem = 64 }
		maxInst := a.MaxInstances
		if maxInst == 0 { maxInst = 1 }
		available := true
		reason := ""
		if healthErrors != nil {
			if r, ok := healthErrors[key]; ok {
				available = false
				reason = r
			}
		}
		engines = append(engines, engineReg{a.Name, a.Version, cores, mem, maxInst, available, reason})
	}
	cfgMu.RUnlock()
	body := map[string]any{
		"coach_id":    cfg.CoachID,
		"cores_total": totalCores(cfg),
		"engines":     engines,
	}
	resp, err := postJSON(client, cfg, "/api/matchmaker/register", body)
	if err != nil {
		return fmt.Errorf("matchmaker register: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("matchmaker register failed: %s — %s", resp.Status, string(bodyBytes))
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
		"resources":     map[string]int{"cores_used": usedCores, "memory_mb_used": usedMem},
	}
	resp, err := postJSON(client, cfg, "/api/coach/heartbeat", body)
	if err != nil {
		return
	}
	resp.Body.Close()
}

func heartbeatLoop(ctx context.Context, client *http.Client, cfg config, mu *sync.Mutex, cfgMu *sync.RWMutex, running map[string]*runningEngine, reload func(), notifyChange <-chan struct{}) {
	// Send a heartbeat at least every 10s, and immediately when resources change.
	// Event-driven sends are batched: drain the channel for up to 1s before sending
	// to avoid thundering-herd POSTs when multiple engines start/stop together.
	const interval = 10 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastServerGen string

	send := func() {
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
			"resources":     map[string]int{"cores_used": usedCores, "memory_mb_used": usedMem},
		}
		resp, err := postJSON(client, cfg, "/api/coach/heartbeat", body)
		if err != nil {
			slog.Warn("heartbeat failed", "err", err)
			return
		}
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

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			send()
		case <-notifyChange:
			// Batch: drain any additional events within a 1s window.
			send()
			drainLoop:
			for {
				select {
				case <-notifyChange:
					// Another event arrived — drain and wait again.
				case <-time.After(1 * time.Second):
					break drainLoop
				}
			}
		}
	}
}

// ── Assignment polling ───────────────────────────────────────────────────

func pollAssignments(client *http.Client, cfg config) []assignment {
	req, _ := http.NewRequest("GET", cfg.ArenaURL+"/api/matchmaker/poll?coach="+cfg.CoachID, nil)
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var result struct {
		Assignments []assignment `json:"assignments"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Assignments
}

// ── Engine lifecycle ─────────────────────────────────────────────────────

func launchEngine(ctx context.Context, ai aiConfig, arenaURL, sessionID, logDir string, gameTimeSec float64) (*runningEngine, error) {
	parts := strings.Fields(ai.RunCmd)
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty run command")
	}

	engCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(engCtx, parts[0], parts[1:]...)
	cmd.Dir = filepath.Dir(parts[0])

	engineLogDir := filepath.Join(logDir, "engines")
	os.MkdirAll(engineLogDir, 0755)
	errLog, _ := os.Create(filepath.Join(engineLogDir, sessionID+".err"))
	stderrWriters := io.MultiWriter(os.Stderr)
	if errLog != nil {
		stderrWriters = io.MultiWriter(os.Stderr, errLog)
	}
	cmd.Stderr = stderrWriters

	// All engines use # arena-stats v1: JSON on stdout.
	var searchMu sync.Mutex
	var searchNodes int64
	var searchDepth int
	var searchScore int
	var adapterTimeMs int64
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()

	slog.Info("launching engine", "binary", parts[0], "args", strings.Join(parts[1:], " "), "dir", cmd.Dir, "session", sessionID)

	if err := cmd.Start(); err != nil {
		cancel()
		slog.Error("engine start failed", "binary", parts[0], "err", err)
		return nil, fmt.Errorf("start: %w", err)
	}

	// Pre-flight GTP health check — catch engines that crash on startup
	// before bridging to the relay.
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
			slog.Error("engine health check failed", "session", sessionID, "response", resp)
			return nil, fmt.Errorf("health check failed: %s", resp)
		}
		slog.Info("engine health check OK", "session", sessionID, "name", strings.TrimPrefix(resp, "= "))
	case <-time.After(3 * time.Second):
		cmd.Process.Kill()
		cancel()
		return nil, fmt.Errorf("health check timeout")
	}

	relayPath := "/api/relay/" + sessionID
	wsURL := strings.Replace(arenaURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	wsURL += relayPath

	wsCtx, wsCancel := context.WithTimeout(ctx, 30*time.Second)
	conn, _, err := websocket.Dial(wsCtx, wsURL, &websocket.DialOptions{})
	wsCancel()
	if err != nil {
		cmd.Process.Kill()
		cancel()
		return nil, fmt.Errorf("ws dial: %w", err)
	}

	// GTP-aware timing: track genmove wall-clock time and enforce
	// the time budget. The matchmaker no longer sends game_time GTP
	// commands — time enforcement is coach-side.
	var timingMu sync.Mutex
	var genmoveStart time.Time
	var totalThinkMs int64
	budgetMs := int64(gameTimeSec * 1000 * 105 / 100 * 2) // 2 games, 5% margin each
	engineTimedOut := false

	// stdout → WS: track genmove response timing and inject
	// measured wall-clock time into stats lines.
	var lastElapsedMs int64
	go func() {
		defer conn.Close(websocket.StatusNormalClosure, "done")
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			raw := scanner.Bytes()
			line := string(raw)

			// Parse engine JSON stats
			if strings.HasPrefix(line, "# arena-stats v1: ") {
				var ns struct {
					Nodes  int64 `json:"nodes"`
					Depth  int   `json:"depth"`
					Score  int   `json:"score"`
					TimeMs int64 `json:"time_ms"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "# arena-stats v1: ")), &ns); err == nil {
					searchMu.Lock()
					searchNodes, searchDepth, searchScore = ns.Nodes, ns.Depth, ns.Score
					adapterTimeMs = ns.TimeMs
					searchMu.Unlock()
				}
			}

			// Inject measured wall-clock time into stats lines
			if strings.HasPrefix(line, "#") {
				timingMu.Lock()
				ms := lastElapsedMs
				timingMu.Unlock()
				raw = []byte(fmt.Sprintf("# time_ms %d %s", ms, strings.TrimPrefix(line, "# ")))
			}

			// Check for genmove response (= ...) and track timing
			var injectLine string
			timingMu.Lock()
			if !genmoveStart.IsZero() && strings.HasPrefix(line, "= ") && len(strings.TrimPrefix(line, "= ")) > 0 {
				lastElapsedMs = time.Since(genmoveStart).Milliseconds()
				totalThinkMs += lastElapsedMs
				genmoveStart = time.Time{}

				if totalThinkMs > budgetMs {
					engineTimedOut = true
					timingMu.Unlock()
					slog.Warn("engine time budget exceeded", "session", sessionID, "total_ms", totalThinkMs, "budget_ms", budgetMs)
					conn.Write(context.Background(), websocket.MessageText, []byte("? timeout"))
					cmd.Process.Kill()
					return
				}
				// Inject timing + search-log data
				searchMu.Lock()
				sn, sd, ss, at := searchNodes, searchDepth, searchScore, adapterTimeMs
				searchNodes, searchDepth, searchScore, adapterTimeMs = 0, 0, 0, 0
				searchMu.Unlock()
				timeMs := lastElapsedMs
				if at > 0 {
					diff := lastElapsedMs - at
					if diff < 0 {
						diff = -diff
					}
					if diff > 100 {
						slog.Warn("adapter time differs from coach", "session", sessionID, "adapter_ms", at, "coach_ms", lastElapsedMs)
					} else {
						timeMs = at
					}
				}
				injectLine = fmt.Sprintf(`# time_ms %d {"nodes":%d,"depth":%d,"score":%d,"timeout":false}`, timeMs, sn, sd, ss)
			}
			timingMu.Unlock()

			if err := conn.Write(context.Background(), websocket.MessageText, raw); err != nil {
				break
			}
			if injectLine != "" {
				conn.Write(context.Background(), websocket.MessageText, []byte(injectLine))
			}
		}
	}()

	// WS → stdin: detect genmove commands to start the clock
	go func() {
		defer stdin.Close()
		for {
			_, msg, err := conn.Read(context.Background())
			if err != nil {
				break
			}
			cmdStr := string(msg)
			if strings.HasPrefix(cmdStr, "genmove") {
				timingMu.Lock()
				genmoveStart = time.Now()
				timingMu.Unlock()
			}
			io.WriteString(stdin, cmdStr+"\n")
		}
	}()

	re := &runningEngine{ai: ai, cmd: cmd, cancel: cancel, sessionID: sessionID}

	// Watchdog: if the engine doesn't respond at all within
	// 2x the per-game budget (for 2 games), kill it.
	if gameTimeSec > 0 {
		wdSec := int(gameTimeSec * 2 * 2) // 2 games * 2x margin
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
		// Try parsing as "30s" format
		fmt.Sscanf(tcJSON, "%fs", &tc.Seconds)
	}
	return tc.Seconds
}

func findAIByKey(cfg config, key string) *aiConfig {
	// key is "name:version"
	for i := range cfg.AIs {
		if cfg.AIs[i].Name+":"+cfg.AIs[i].Version == key {
			return &cfg.AIs[i]
		}
	}
	return nil
}

func totalCores(cfg config) int {
	if cfg.MaxCores > 0 {
		return cfg.MaxCores
	}
	return 1
}

func computeEngineIdentity(ai aiConfig) (string, string) {
	parts := strings.Fields(ai.RunCmd)
	if len(parts) == 0 {
		return "", ""
	}
	binPath := parts[0]
	var manifest strings.Builder
	fmt.Fprintf(&manifest, "Engine: %s %s\n", ai.Name, ai.Version)
	if ai.Created != "" {
		fmt.Fprintf(&manifest, "Date: %s\n", ai.Created)
	}
	if ai.ChangelogShort != "" {
		fmt.Fprintf(&manifest, "Changes: %s\n", ai.ChangelogShort)
	}
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
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if ext == ".brn" || ext == ".bin" || ext == ".safetensors" || ext == ".raw" || ext == ".dat" || ext == ".txt" {
				if seen[e.Name()] {
					continue
				}
				seen[e.Name()] = true
				path := filepath.Join(dir, e.Name())
				data, err := os.ReadFile(path)
				if err != nil {
					continue
				}
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
	suf := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(n)
	i := 0
	for i < len(suf)-1 && f >= 995 {
		f /= 1024
		i++
	}
	if f < 10 {
		return fmt.Sprintf("%.1f %s", f, suf[i])
	}
	return fmt.Sprintf("%.0f %s", f, suf[i])
}

func totalMem(cfg config) int {
	if cfg.MaxRAMMB > 0 {
		return cfg.MaxRAMMB
	}
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
