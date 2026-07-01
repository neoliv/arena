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
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
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
	Version        string `yaml:"version,omitempty"` // probed from engine if absent
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
	// killReason records why the coach killed this engine.
	// "": still running (or engine exited on its own).
	// "timeout": engine exceeded time budget (engine fault).
	// "nopartner": watchdog fired — no game activity within budget (infra).
	// "shutdown": coach stopping or server restart (infra).
	killReason string
	killMu     sync.Mutex
}

// assignment is a single side assignment from the matchmaker.
// pairGroup holds both sides of a matchmaker pair.
// Session IDs are "pairID-b" / "pairID-w"; the base is the shared prefix.
type pairGroup struct {
	base  string
	black *assignment
	white *assignment
}

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
	showVer := flag.Bool("version", false, "Print version and exit")
	aisFilter := flag.String("ais", "", "Comma-separated list of AI names to load from coach.d/ (default: all)")
	debug := flag.Bool("debug", false, "Enable debug-level logging (shows capacity skips, etc.)")
	handleShortFlags("coach")
	flag.Parse()

	logLevel := slog.LevelDebug
	if !*debug {
		logLevel = slog.LevelInfo
	}
	logDir := filepath.Join(os.Getenv("HOME"), "dev", "agent", "arena", "log")
	os.MkdirAll(logDir, 0755)
	if lf, err := os.Create(filepath.Join(logDir, "coach.log")); err == nil {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, lf), &slog.HandlerOptions{Level: logLevel})))
	}
	slog.Info("coach starting", "pid", os.Getpid(), "log_dir", logDir)

	// Clean up old engine stderr logs (older than 7 days).
	engineLogDir := filepath.Join(logDir, "engines")
	if entries, err := os.ReadDir(engineLogDir); err == nil {
		cutoff := time.Now().Add(-7 * 24 * time.Hour)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".err") {
				if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
					os.Remove(filepath.Join(engineLogDir, e.Name()))
				}
			}
		}
	}

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
		slog.Info("using token", "source", tokenSource)
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
	if strings.Contains(cfg.ShareDir, " ") {
		slog.Error("share_dir contains spaces — this will break command-line parsing", "share_dir", cfg.ShareDir)
		os.Exit(1)
	}

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
				if ai.Name == "" {
					continue
				}
				if len(allowedAIs) > 0 && !allowedAIs[ai.Name] {
					continue
				}
				if ai.Binary != "" && !strings.HasPrefix(ai.Binary, "/") {
					ai.Binary = filepath.Join(engineDir, ai.Binary)
				}
				ai.RunCmd = strings.TrimSpace(ai.Binary + " " + ai.Args)
				// Substitute share_dir before probing — the engine may need
				// weights/config files from the share directory to start.
				ai.RunCmd = strings.Replace(ai.RunCmd, "%share_dir%", cfg.ShareDir, -1)

				// Probe version from the engine binary if YAML didn't specify one.
				// This makes the binary the single source of truth for version numbers.
				if ai.Version == "" {
					if v, err := probeVersion(ai); err == nil {
						ai.Version = v
						slog.Info("probed engine version", "name", ai.Name, "version", v)
					} else {
						slog.Warn("version probe failed", "name", ai.Name, "err", err)
						continue // skip — can't register without a version
					}
				}

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

	// ── Core usage sampler ───────────────────────────────────────────
	go coreSampler(ctx, cfg, &mu, running)

	// ── Assignment poll loop ──────────────────────────────────────────
	// Replaces the old task-based push model. The coach polls the
	// matchmaker for assignments, launches engines, and bridges them
	// to the relay. The matchmaker executes the game when both sides
	// connect.
	slog.Info("starting assignment poll loop")
	lastReReg := time.Now().Add(-5 * time.Minute)
	consecutiveFailures := 0
	for ctx.Err() == nil {
		if time.Since(lastReReg) > 5*time.Minute {
			loadAndRegister()
			lastReReg = time.Now()
		}

		assignments, pollOK := pollAssignments(client, cfg, 8) // up to 4 pairs × 2 sides
		if !pollOK {
			// Arena unreachable — exponential backoff.
			consecutiveFailures++
			delay := time.Duration(1<<min(consecutiveFailures-1, 6)) * time.Second
			if delay > 60*time.Second {
				delay = 60 * time.Second
			}
			slog.Warn("poll failed (arena unreachable?), backing off", "delay_s", delay.Seconds(), "failures", consecutiveFailures)
			select {
			case <-ctx.Done():
				break
			case <-time.After(delay):
			}
			continue
		}
		if consecutiveFailures > 0 {
			// Arena is back — re-register immediately (it may have restarted).
			slog.Info("arena reconnected after failures, re-registering", "failures", consecutiveFailures)
			loadAndRegister()
			lastReReg = time.Now()
		}
		consecutiveFailures = 0

		if len(assignments) == 0 {
			mu.Lock()
			idle := len(running)
			mu.Unlock()
			if idle == 0 {
				slog.Warn("poll returned no assignments and no engines running — matchmaker may be stale")
			}
		} else {
			slog.Info("poll received assignments", "count", len(assignments))
		}
		// Group assignments by pair base (session ID is "pairID-b" or "pairID-w").
		// Only launch complete pairs — never strand one side alone, which wastes
		// a core on an engine that will sit idle until reaped 15s later.
		//
		// TODO(multi-coach): when multiple coaches are active, a pair's two sides
		// may be assigned to different coaches. The current single-coach logic
		// requires both sides to be launchable locally. For multi-coach, the
		// matchmaker must track which coach claimed each side and only start the
		// game when both coaches have connected.
		pairs := groupAssignmentsByPair(assignments)
		for _, pair := range pairs {
			// Check if mate is already running — launch the missing side.
			var mateRunning bool
			mu.Lock()
			for sid := range running {
				if idx := strings.LastIndex(sid, "-"); idx >= 0 && sid[:idx] == pair.base {
					mateRunning = true
					break
				}
			}
			mu.Unlock()

			if mateRunning {
				var missing *assignment
				if pair.black != nil {
					mu.Lock()
					_, exists := running[pair.black.SessionID]
					mu.Unlock()
					if !exists {
						missing = pair.black
					}
				}
				if missing == nil && pair.white != nil {
					mu.Lock()
					_, exists := running[pair.white.SessionID]
					mu.Unlock()
					if !exists {
						missing = pair.white
					}
				}
				if missing != nil {
					launchOne(missing, &cfg, &cfgMu, &mu, running, healthErrors, notifyChange, ctx, logDir)
				}
				continue
			}

			// No mate running — only launch if BOTH sides are launchable.
			if pair.black == nil || pair.white == nil {
				continue
			}

			cfgMu.RLock()
			bAI := findAIByKey(cfg, pair.black.Engine)
			wAI := findAIByKey(cfg, pair.white.Engine)
			cfgMu.RUnlock()
			if bAI == nil || wAI == nil {
				continue
			}

			mu.Lock()
			if _, exists := running[pair.black.SessionID]; exists {
				mu.Unlock()
				continue
			}
			if _, exists := running[pair.white.SessionID]; exists {
				mu.Unlock()
				continue
			}
			usedCores, usedMem := 0, 0
			bInst, wInst := 0, 0
			for _, re := range running {
				usedCores += re.ai.Cores
				usedMem += re.ai.MemoryMB
				if re.ai.Name == bAI.Name && re.ai.Version == bAI.Version {
					bInst++
				}
				if re.ai.Name == wAI.Name && re.ai.Version == wAI.Version {
					wInst++
				}
			}
			coresNeeded := bAI.Cores + wAI.Cores
			memNeeded := bAI.MemoryMB + wAI.MemoryMB
			bOk := bAI.MaxInstances == 0 || bInst < bAI.MaxInstances
			wOk := wAI.MaxInstances == 0 || wInst < wAI.MaxInstances
			if !bOk || !wOk || usedCores+coresNeeded > totalCores(cfg) || usedMem+memNeeded > totalMem(cfg) {
				mu.Unlock()
				continue
			}
			mu.Unlock()

			// Both sides launchable — launch both.
			launchOne(pair.black, &cfg, &cfgMu, &mu, running, healthErrors, notifyChange, ctx, logDir)
			launchOne(pair.white, &cfg, &cfgMu, &mu, running, healthErrors, notifyChange, ctx, logDir)
		}

		select {
		case <-ctx.Done():
			break
		case <-time.After(5 * time.Second):
		}
	}

	mu.Lock()
	for _, re := range running {
		re.killMu.Lock()
		re.killReason = "shutdown"
		re.killMu.Unlock()
		re.cancel()
		re.cmd.Process.Kill()
	}
	mu.Unlock()
	slog.Info("coach shutting down")
}

// ── HTTP helpers ─────────────────────────────────────────────────────────

// groupAssignmentsByPair groups assignments by pair base.
// Session IDs are "pairID-b" or "pairID-w"; the base is everything before
// the last "-". Each pair gets one entry with black and/or white set.
func groupAssignmentsByPair(assignments []assignment) []pairGroup {
	groups := make(map[string]*pairGroup)
	var order []string
	for i := range assignments {
		a := &assignments[i]
		idx := strings.LastIndex(a.SessionID, "-")
		if idx < 0 {
			continue
		}
		base := a.SessionID[:idx]
		pg, ok := groups[base]
		if !ok {
			pg = &pairGroup{base: base}
			groups[base] = pg
			order = append(order, base)
		}
		if a.Side == "black" {
			pg.black = a
		} else {
			pg.white = a
		}
	}
	result := make([]pairGroup, len(order))
	for i, base := range order {
		result[i] = *groups[base]
	}
	return result
}

// launchOne launches a single engine for an assignment. Extracted from the
// main loop so it can be called for both sides of a pair.
func launchOne(a *assignment, cfg *config, cfgMu *sync.RWMutex, mu *sync.Mutex, running map[string]*runningEngine, healthErrors map[string]string, notifyChange chan<- struct{}, ctx context.Context, logDir string) {
	cfgMu.RLock()
	ai := findAIByKey(*cfg, a.Engine)
	cfgMu.RUnlock()
	if ai == nil {
		slog.Warn("assignment for unknown AI", "engine", a.Engine)
		return
	}

	mu.Lock()
	if _, exists := running[a.SessionID]; exists {
		mu.Unlock()
		return
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
	if (ai.MaxInstances > 0 && instCount >= ai.MaxInstances) || usedCores+ai.Cores > totalCores(*cfg) || usedMem+ai.MemoryMB > totalMem(*cfg) {
		mu.Unlock()
		return
	}
	mu.Unlock()

	aiCopy := *ai
	gameSecs := parseGameTime(a.TimeControl)
	if gameSecs > 0 {
		aiCopy.RunCmd = strings.Replace(aiCopy.RunCmd, "%game_time%", fmt.Sprintf("%.0f", gameSecs), -1)
		aiCopy.RunCmd = strings.Replace(aiCopy.RunCmd, "%share_dir%", cfg.ShareDir, -1)
	}

	re, err := launchEngine(ctx, aiCopy, cfg.ArenaURL, cfg.Token, a.SessionID, logDir, gameSecs)
	if err != nil {
		slog.Error("launch engine", "ai", ai.Name, "session", a.SessionID, "err", err)
		mu.Lock()
		healthErrors[ai.Name+":"+ai.Version] = err.Error()
		mu.Unlock()
		select {
		case notifyChange <- struct{}{}:
		default:
		}
		return
	}

	mu.Lock()
	running[a.SessionID] = re
	mu.Unlock()

	select {
	case notifyChange <- struct{}{}:
	default:
	}

	// Wait for engine to exit (relay closes after matchmaker finishes game)
	go func(sid string, engineKey string) {
		err := re.cmd.Wait()
		if err != nil {
			re.killMu.Lock()
			kr := re.killReason
			re.killMu.Unlock()
			switch kr {
			case "timeout":
				slog.Warn("engine TIME BUDGET EXCEEDED — game scored as loss", "session", sid, "err", err)
			case "nopartner", "shutdown":
				slog.Warn("engine INFRASTRUCTURE KILL (no partner) — no penalty", "session", sid, "err", err)
				mu.Lock()
				delete(healthErrors, engineKey)
				mu.Unlock()
			default:
				slog.Warn("engine exited with error", "session", sid, "err", err)
			}
		} else {
			slog.Info("engine exited cleanly", "session", sid)
			mu.Lock()
			delete(healthErrors, engineKey)
			mu.Unlock()
		}
		re.cancel()
		mu.Lock()
		delete(running, sid)
		mu.Unlock()
		select {
		case notifyChange <- struct{}{}:
		default:
		}
	}(a.SessionID, ai.Name+":"+ai.Version)
}

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

// reportEngineError sends an engine error classification to the arena.
// Fire-and-forget with a short timeout — if it fails, the matchmaker
// will treat the failure as infrastructure (engine blameless), which is
// the safe default.
func reportEngineError(arenaURL, token, sessionID, errorType string) {
	body := map[string]string{
		"session_id": sessionID,
		"error_type": errorType,
	}
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(body)
	req, _ := http.NewRequest("POST", arenaURL+"/api/coach/engine-error", &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("failed to report engine error to arena", "session", sessionID, "error_type", errorType, "err", err)
		return
	}
	resp.Body.Close()
	slog.Info("reported engine error to arena", "session", sessionID, "error_type", errorType)
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
			if lastServerGen == "" {
				// First heartbeat — just record the generation, don't reload.
				lastServerGen = hb.ServerGen
			} else {
				slog.Info("server restart detected — killing orphaned engines and re-registering",
					"server_gen", hb.ServerGen[:min(8, len(hb.ServerGen))])
				lastServerGen = hb.ServerGen
				// Server restart means all relay sessions are gone.
				// Kill running engines immediately to free cores.
				killAllRunning(mu, running)
				reload()
			}
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

func pollAssignments(client *http.Client, cfg config, n int) ([]assignment, bool) {
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/matchmaker/poll?coach=%s&n=%d", cfg.ArenaURL, cfg.CoachID, n), nil)
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	var result struct {
		Assignments []assignment `json:"assignments"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Assignments, true
}

// ── Engine lifecycle ─────────────────────────────────────────────────────

func launchEngine(ctx context.Context, ai aiConfig, arenaURL, token, sessionID, logDir string, gameTimeSec float64) (*runningEngine, error) {
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
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			healthCh <- scanner.Text()
		} else {
			healthCh <- ""
		}
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

	// Probe engine version via GTP. If YAML has no version, use the engine's
	// own version string (e.g. "0.1.1" from CARGO_PKG_VERSION). This makes
	// the binary the single source of truth for version numbers.
	if ai.Version == "" {
		stdin.Write([]byte("version\n"))
		verCh := make(chan string, 1)
		go func() {
			scanner := bufio.NewScanner(stdout)
			if scanner.Scan() {
				verCh <- scanner.Text()
			} else {
				verCh <- ""
			}
		}()
		select {
		case resp := <-verCh:
			if strings.HasPrefix(resp, "= ") {
				ai.Version = strings.TrimPrefix(resp, "= ")
			}
		case <-time.After(2 * time.Second):
		}
	}
	if ai.Version == "" {
		ai.Version = "unknown"
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

	re := &runningEngine{ai: ai, cmd: cmd, cancel: cancel, sessionID: sessionID}

	// GTP-aware timing: per-genmove deadline. The MM owns per-game
	// time enforcement. The coach is the safety backstop — it kills
	// the engine if a single genmove exceeds budget + margin without
	// responding (hung engine).
	var timingMu sync.Mutex
	var genmoveStart time.Time
	genmoveDeadline := make(chan struct{}, 1) // closed when deadline expires
	gameStarted := false                      // disarmed by clear_board

	// gameOver is closed by the WS→stdin goroutine when the MM closes
	// the relay (normal game end). The stdout goroutine checks this to
	// distinguish "engine crashed" from "game finished".
	gameOver := make(chan struct{})

	// Per-genmove deadline goroutine: kills the engine if budget + margin
	// expires with no response. Reset on each genmove command.
	go func() {
		margin := 5 * time.Second
		if gameTimeSec < 5 {
			margin = time.Duration(gameTimeSec) * time.Second
		}
		budget := time.Duration(gameTimeSec) * time.Second
		var timer *time.Timer
		for {
			select {
			case <-engCtx.Done():
				if timer != nil {
					timer.Stop()
				}
				return
			case <-genmoveDeadline:
				// New genmove: (re)start the timer.
				if timer != nil {
					timer.Stop()
				}
				timer = time.AfterFunc(budget+margin, func() {
					re.killMu.Lock()
					re.killReason = "timeout"
					re.killMu.Unlock()
					slog.Warn("engine HUNG — no response within budget + margin",
						"session", sessionID, "budget_s", budget.Seconds(), "margin_s", margin.Seconds())
					reportEngineError(arenaURL, token, sessionID, "timeout")
					// Inject "= ? timeout" as the genmove response
					// so the MM sees a proper timeout (not a crash).
					conn.Write(context.Background(), websocket.MessageText,
						[]byte("= ? timeout"))
					cmd.Process.Kill()
				})
			}
		}
	}()

	// stdout → WS: track genmove response timing and inject
	// measured wall-clock time into stats lines.
	var lastElapsedMs int64
	go func() {
		defer conn.Close(websocket.StatusNormalClosure, "done")
		writeFailed := false // true if conn.Write fails → network issue, not engine crash
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
				} else {
					searchMu.Lock()
					searchNodes, searchDepth, searchScore, adapterTimeMs = 0, 0, 0, 0
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
				genmoveStart = time.Time{}
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
				writeFailed = true
				break
			}
			if injectLine != "" {
				conn.Write(context.Background(), websocket.MessageText, []byte(injectLine))
			}
		}
		// Crash detection: only when stdout closed (scanner.Scan→false).
		// Network failures (conn.Write error→writeFailed=true) are NOT
		// engine crashes — the engine is still running, connection broke.
		if writeFailed {
			return
		}
		// Scanner exited (EOF on stdout). Determine why.
		// - gameOver closed: MM finished the game, relay closed → normal.
		// - killReason set: coach killed engine (timeout/watchdog) → already reported.
		// - otherwise: engine exited on its own → crash. Report BEFORE conn.Close().
		select {
		case <-gameOver:
			// Normal end — MM finished the game.
		default:
			re.killMu.Lock()
			kr := re.killReason
			re.killMu.Unlock()
			if kr == "" {
				slog.Warn("engine exited unexpectedly (crash)", "session", sessionID)
				reportEngineError(arenaURL, token, sessionID, "crash")
			}
		}
	}()

	// WS → stdin: detect genmove commands to start the clock.
	// When the MM closes the relay, conn.Read() fails → close gameOver
	// to signal "normal end" to the stdout goroutine.
	go func() {
		defer stdin.Close()
		defer close(gameOver)
		for {
			_, msg, err := conn.Read(context.Background())
			if err != nil {
				break
			}
			cmdStr := string(msg)
			if strings.HasPrefix(cmdStr, "clear_board") {
				timingMu.Lock()
				gameStarted = true
				timingMu.Unlock()
			}
			if strings.HasPrefix(cmdStr, "genmove") {
				timingMu.Lock()
				genmoveStart = time.Now()
				timingMu.Unlock()
				// Signal the deadline goroutine to (re)start the timer.
				select {
				case genmoveDeadline <- struct{}{}:
				default:
				}
			}
			if _, err := io.WriteString(stdin, cmdStr+"\n"); err != nil {
				break
			}
		}
	}()

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
			started := gameStarted
			timingMu.Unlock()
			if !started {
				re.killMu.Lock()
				re.killReason = "nopartner"
				re.killMu.Unlock()
			}
			re.killMu.Lock()
			kr := re.killReason
			re.killMu.Unlock()
			if kr == "nopartner" {
				slog.Warn("engine INFRASTRUCTURE KILL (no partner found)", "session", sessionID, "seconds", wdSec)
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

// O7: mtime-based cache for computeEngineIdentity: avoids re-hashing
// large binary/data files when nothing has changed on disk.
var (
	identityCacheMu sync.Mutex
	identityCache   = make(map[string]struct {
		engineID string
		manifest string
		mtimers  map[string]time.Time
	})
)

func computeEngineIdentity(ai aiConfig) (string, string) {
	parts := strings.Fields(ai.RunCmd)
	if len(parts) == 0 {
		return "", ""
	}
	binPath := parts[0]

	// Check mtime-based cache: if the binary path is known and all
	// previously tracked file mtimes match, return cached identity.
	identityCacheMu.Lock()
	if cached, ok := identityCache[binPath]; ok {
		match := true
		for path, mtime := range cached.mtimers {
			info, err := os.Stat(path)
			if err != nil || !info.ModTime().Equal(mtime) {
				match = false
				break
			}
		}
		if match {
			identityCacheMu.Unlock()
			return cached.engineID, cached.manifest
		}
	}
	identityCacheMu.Unlock()

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
	mtimers := make(map[string]time.Time)
	if data, err := os.ReadFile(binPath); err == nil {
		info, _ := os.Stat(binPath)
		h := sha256.Sum256(data)
		hasher.Write(h[:])
		mtimers[binPath] = info.ModTime()
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
				mtimers[path] = info.ModTime()
				fmt.Fprintf(&manifest, "Data: %s\n  Size: %s\n  Modified: %s\n  SHA256: %s\n\n",
					path, niceSize(info.Size()), info.ModTime().Format("2006-01-02 15:04"), hex.EncodeToString(h[:])[:16])
			}
		}
	}

	engineID := hex.EncodeToString(hasher.Sum(nil))[:16]
	manifest.WriteString(fmt.Sprintf("Engine ID: %s\n", engineID))

	// Cache the result before returning.
	identityCacheMu.Lock()
	identityCache[binPath] = struct {
		engineID string
		manifest string
		mtimers  map[string]time.Time
	}{
		engineID: engineID,
		manifest: manifest.String(),
		mtimers:  mtimers,
	}
	identityCacheMu.Unlock()

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

// ── Core usage sampler + resource reporter ──────────────────────────
// Samples actual CPU time (via /proc/[pid]/stat) and Pss memory (via
// /proc/[pid]/smaps_rollup) for every running engine process including
// children. A core is counted as "used" only if the engine is computing.
//
// Three cadences:
//   - 1s: sample CPU/Pss, accumulate per-player stats
//   - 20s: POST per-player stats to arena, reset interval accumulators
//   - 10min: log detailed summary (existing behavior)

func coreSampler(ctx context.Context, cfg config, mu *sync.Mutex, running map[string]*runningEngine) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	sendTicker := time.NewTicker(20 * time.Second)
	defer sendTicker.Stop()
	reportTicker := time.NewTicker(10 * time.Minute)
	defer reportTicker.Stop()
	totalCores := float64(totalCores(cfg))
	var sumUtil, sumIdle float64
	var samples int64

	// Per-PID last-seen CPU tick count (monotonic, from /proc/[pid]/stat).
	prevCPU := make(map[int]uint64)

	// Interval (20s) and cumulative (since start) per-player accumulators.
	type playerAgg struct {
		cpu, rss  metricAgg
		instances int // max instances seen in this window
	}
	intervalAcc := make(map[string]*playerAgg)
	cumulativeAcc := make(map[string]*playerAgg)

	httpClient := &http.Client{Timeout: 10 * time.Second}

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			mu.Lock()
			type entry struct {
				pid       int
				playerKey string
			}
			var entries []entry
			for _, re := range running {
				if re.cmd != nil && re.cmd.Process != nil {
					entries = append(entries, entry{re.cmd.Process.Pid, re.ai.Name + ":" + re.ai.Version})
				}
			}
			mu.Unlock()

			var usedCPU float64
			instCounts := make(map[string]int)
			for _, e := range entries {
				ticks := readProcessCPUTicks(e.pid)
				pssKB := readProcessPss(e.pid)
				delta := float64(0)
				if prev, ok := prevCPU[e.pid]; ok && ticks >= prev {
					delta = float64(ticks-prev) / 100.0
				}
				prevCPU[e.pid] = ticks
				usedCPU += delta
				cpuPct := delta
				idle := cpuPct < 0.01
				if idle {
					sumIdle++
				}
				rssMB := float64(pssKB) / 1024.0

				// Accumulate into interval + cumulative
				for _, accMap := range []map[string]*playerAgg{intervalAcc, cumulativeAcc} {
					pa := accMap[e.playerKey]
					if pa == nil {
						pa = &playerAgg{}
						pa.cpu.min, pa.rss.min = 1e9, 1e9
						accMap[e.playerKey] = pa
					}
					addMetric(&pa.cpu, cpuPct)
					addMetric(&pa.rss, rssMB)
				}
				instCounts[e.playerKey]++
			}
			// Update per-player instance counts in interval accumulator
			for k, n := range instCounts {
				if intervalAcc[k] != nil && n > intervalAcc[k].instances {
					intervalAcc[k].instances = n
				}
				if cumulativeAcc[k] != nil && n > cumulativeAcc[k].instances {
					cumulativeAcc[k].instances = n
				}
			}
			// Clean up stale PIDs
			for pid := range prevCPU {
				found := false
				for _, e := range entries {
					if e.pid == pid {
						found = true
						break
					}
				}
				if !found {
					delete(prevCPU, pid)
				}
			}

			if usedCPU > totalCores {
				usedCPU = totalCores
			}
			sumUtil += usedCPU / totalCores
			samples++

		case <-sendTicker.C:
			// Build payload from interval accumulators
			var players []map[string]interface{}
			for key, pa := range intervalAcc {
				name, ver := splitPlayerKey(key)
				// Look up declared resource allocation from engine config
				memMB := 64 // default
				mu.Lock()
				for _, re := range running {
					rKey := re.ai.Name + ":" + re.ai.Version
					if rKey == key {
						if re.ai.MemoryMB > 0 {
							memMB = re.ai.MemoryMB
						}
						break
					}
				}
				mu.Unlock()
				players = append(players, map[string]interface{}{
					"name": name, "version": ver,
					"instances": pa.instances,
					"memory_mb": memMB,
					"interval": map[string]interface{}{
						"cpu_pct": metricSummary(&pa.cpu),
						"rss_mb":  metricSummary(&pa.rss),
					},
					"cumulative": map[string]interface{}{
						"cpu_pct": metricSummary(&cumulativeAcc[key].cpu),
						"rss_mb":  metricSummary(&cumulativeAcc[key].rss),
					},
				})
			}
			// Reset interval accumulators
			intervalAcc = make(map[string]*playerAgg)

			go func(pl []map[string]interface{}) {
				body := map[string]interface{}{
					"coach_id": cfg.CoachID,
					"players":  pl,
				}
				var buf bytes.Buffer
				json.NewEncoder(&buf).Encode(body)
				req, _ := http.NewRequest("POST", cfg.ArenaURL+"/api/coach/resources", &buf)
				req.Header.Set("Content-Type", "application/json")
				if cfg.Token != "" {
					req.Header.Set("Authorization", "Bearer "+cfg.Token)
				}
				resp, err := httpClient.Do(req)
				if err != nil {
					slog.Warn("resource stats POST failed", "err", err, "players", len(pl))
					return
				}
				resp.Body.Close()
				// Log a one-line summary of what was reported
				var parts []string
				for _, p := range pl {
					ival := p["interval"].(map[string]interface{})
					cpu := ival["cpu_pct"].(map[string]float64)
					rss := ival["rss_mb"].(map[string]float64)
					parts = append(parts, fmt.Sprintf("%s:%s cpu=%.0f%% rss=%.0fMB", p["name"], p["version"], cpu["avg"]*100, rss["avg"]))
				}
				slog.Info("resource stats reported", "players", len(pl), "summary", strings.Join(parts, ", "))
			}(players)

		case <-reportTicker.C:
			if samples == 0 {
				continue
			}
			avgUtil := sumUtil / float64(samples)
			avgIdle := sumIdle / float64(samples)
			slog.Info("core usage (real CPU)", "avg_utilization", fmt.Sprintf("%.1f%%", avgUtil*100),
				"avg_idle_engines", fmt.Sprintf("%.1f", avgIdle),
				"cores_total", int(totalCores), "samples", samples, "interval", "10m")
			for key, pa := range cumulativeAcc {
				n := float64(pa.cpu.count)
				if n == 0 {
					continue
				}
				cpuAvg := pa.cpu.sum / n
				rssAvg := pa.rss.sum / n
				cpuStd := math.Sqrt(pa.cpu.sumSq/n - cpuAvg*cpuAvg)
				rssStd := math.Sqrt(pa.rss.sumSq/n - rssAvg*rssAvg)
				slog.Info("player resource stats", "player", key,
					"cpu_pct", fmt.Sprintf("min=%.0f max=%.0f avg=%.0f std=%.0f", pa.cpu.min*100, pa.cpu.max*100, cpuAvg*100, cpuStd*100),
					"rss_mb", fmt.Sprintf("min=%.0f max=%.0f avg=%.0f std=%.0f", pa.rss.min, pa.rss.max, rssAvg, rssStd),
					"samples", pa.cpu.count)
			}
			sumUtil, sumIdle, samples = 0, 0, 0
		}
	}
}

func addMetric(m *metricAgg, v float64) {
	m.sum += v
	m.sumSq += v * v
	m.count++
	if v < m.min {
		m.min = v
	}
	if v > m.max {
		m.max = v
	}
}

type metricAgg struct {
	sum, sumSq, min, max float64
	count                int
}

func metricSummary(m *metricAgg) map[string]float64 {
	if m.count == 0 {
		return map[string]float64{"min": 0, "max": 0, "avg": 0, "std": 0}
	}
	avg := m.sum / float64(m.count)
	variance := m.sumSq/float64(m.count) - avg*avg
	if variance < 0 {
		variance = 0
	}
	return map[string]float64{"min": m.min, "max": m.max, "avg": avg, "std": math.Sqrt(variance)}
}

func splitPlayerKey(key string) (name, version string) {
	idx := strings.LastIndex(key, ":")
	if idx < 0 {
		return key, "unknown"
	}
	return key[:idx], key[idx+1:]
}

// readProcessCPUTicks reads total CPU ticks (utime+stime+cutime+cstime)
// from /proc/[pid]/stat for the process including its children.
func readProcessCPUTicks(pid int) uint64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	s := string(data)
	closeParen := strings.LastIndexByte(s, ')')
	if closeParen < 0 {
		return 0
	}
	parts := strings.Fields(s[closeParen+2:])
	if len(parts) < 15 {
		return 0
	}
	utime, _ := strconv.ParseUint(parts[11], 10, 64)
	stime, _ := strconv.ParseUint(parts[12], 10, 64)
	cutime, _ := strconv.ParseUint(parts[13], 10, 64)
	cstime, _ := strconv.ParseUint(parts[14], 10, 64)
	return utime + stime + cutime + cstime
}

// readProcessPss reads Proportional Set Size in KiB from /proc/[pid]/smaps_rollup.
// Pss divides shared pages (e.g. mmapped NNUE weights) evenly among all
// processes sharing them — 4 neursi instances sharing 610MB weights each
// report ~152MB Pss instead of 610MB RSS.
func readProcessPss(pid int) uint64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Pss:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				v, _ := strconv.ParseUint(f[1], 10, 64)
				return v
			}
		}
	}
	return 0
}

// killAllRunning kills all engine subprocesses and clears the running map.
// Used when the arena server restarts — all relay sessions are gone and
// running engines are orphaned (consuming cores doing nothing).
func killAllRunning(mu *sync.Mutex, running map[string]*runningEngine) {
	mu.Lock()
	defer mu.Unlock()
	n := len(running)
	for sid, re := range running {
		re.killMu.Lock()
		re.killReason = "shutdown"
		re.killMu.Unlock()
		re.cmd.Process.Kill()
		delete(running, sid)
	}
	slog.Info("killed orphaned engines after server restart", "count", n)
}

// handleShortFlags is the canonical source; duplicated across cmd/*/main.go
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

// probeVersion launches the engine, sends GTP "version", and returns the
// response. Used at registration time to get the real version from the binary
// when the YAML doesn't specify one.
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
	defer cmd.Wait()

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
		if strings.HasPrefix(resp, "= ") {
			return strings.TrimSpace(strings.TrimPrefix(resp, "= ")), nil
		}
		return "", fmt.Errorf("bad GTP response: %s", resp)
	case <-ctx.Done():
		cmd.Process.Kill()
		return "", fmt.Errorf("timeout")
	}
}
