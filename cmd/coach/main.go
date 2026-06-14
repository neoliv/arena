// Coach runs on contributor machines. It manages AI lifecycles:
// registers available AIs with the Arena, polls for match assignments,
// launches engines as subprocesses, and bridges stdin/stdout to a WebSocket GTP relay.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
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
	ai       aiConfig
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	sessionID string
}

func main() {
	configPath := flag.String("config", "coach.yaml", "Path to coach config file")
	playersDir := flag.String("players", "players.d", "Directory containing player .yaml files")
	showVer    := flag.Bool("version", false, "Print version and exit")
	aisFilter  := flag.String("ais", "", "Comma-separated list of AI names to load from coach.d/ (default: all)")
	handleShortFlags("coach")
	flag.Parse()

	// Log to file alongside the binary (arena/coach.log)
	if exe, err := os.Executable(); err == nil {
		if lf, err := os.Create(filepath.Join(filepath.Dir(exe), "coach.log")); err == nil {
			slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, lf), &slog.HandlerOptions{Level: slog.LevelInfo})))
		}
	}
	slog.Info("coach starting", "pid", os.Getpid())

	if *showVer {
		fmt.Print(version.PrintVersion("coach"))
		return
	}
	// Build set of allowed AI names (empty = all)
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

	// Register
	loadAndRegister := func() {
		var ais []aiConfig
		entries, err := os.ReadDir(*playersDir)
		if err == nil {
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") { continue }
				aiData, err := os.ReadFile("coach.d/" + entry.Name())
				if err != nil { slog.Warn("read ai config", "file", entry.Name(), "err", err); continue }
				var ai aiConfig
				if err := yaml.Unmarshal(aiData, &ai); err != nil {
					slog.Warn("parse ai config", "file", entry.Name(), "err", err); continue
				}
				if ai.Name == "" || ai.Version == "" { continue }
				if len(allowedAIs) > 0 && !allowedAIs[ai.Name] { continue }
				// Compute engine_id: hash binary + companion data
				ai.EngineID, ai.EngineManifest = computeEngineIdentity(ai)
				ais = append(ais, ai)
			}
		}
		if len(ais) == 0 { slog.Error("no players found in engines" + "/*/players.d/*.yaml"); return }
		slog.Info("loaded AIs", "count", len(ais))
		cfg.AIs = ais
		slog.Info("registering with arena", "url", cfg.ArenaURL, "ais", len(cfg.AIs)); for _, a := range cfg.AIs { slog.Info("  player", "name", a.Name, "version", a.Version, "binary", a.Binary, "args", a.Args, "engine_id", a.EngineID[:min(16,len(a.EngineID))]) }; if err := register(client, cfg); err != nil {
			slog.Error("REGISTRATION FAILED", "err", err)
		} else {
			slog.Info("REGISTRATION SUCCEEDED", "ais", len(cfg.AIs))
		}
	}
	loadAndRegister()

	// Heartbeat + SIGHUP goroutines
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGHUP)
	defer cancel()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			s, err := signal.NotifyContext(context.Background(), syscall.SIGHUP)
			if err != nil { return }
			<-s.Done()
			slog.Info("SIGHUP received, reloading config")
			loadAndRegister()
		}
	}()

	var mu sync.Mutex
	running := make(map[string]*runningEngine) // key: sessionID

	go heartbeatLoop(ctx, client, cfg, &mu, running)

	// Task polling loop
	slog.Info("starting task poll loop")
	for ctx.Err() == nil {
		tasks := pollTasks(client, cfg)
		for _, t := range tasks {
			ai := findAI(cfg, t.EngineName, t.EngineVersion)
			if ai == nil {
				slog.Warn("task for unknown AI", "name", t.EngineName, "version", t.EngineVersion)
				continue
			}

			mu.Lock()
			// Resource check
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
				slog.Info("declining task (at capacity)", "ai", ai.Name, "instances", instCount)
				declineTask(client, cfg, t.AssignmentID, fmt.Sprintf("at capacity: %d/%d %s", instCount, ai.MaxInstances, ai.Name))
				continue
			}
			mu.Unlock()

			// Accept and launch
			acceptTask(client, cfg, t.AssignmentID)

			re, err := launchEngine(ctx, *ai, cfg.ArenaURL, t.RelayPath, t.SessionID)
			if err != nil {
				slog.Error("launch engine", "ai", ai.Name, "err", err)
				failTask(client, cfg, t.AssignmentID, "launch failed: "+err.Error())
				continue
			}

			mu.Lock()
			running[t.SessionID] = re
			mu.Unlock()

			readyTask(client, cfg, t.AssignmentID, t.SessionID)

			// Watch for completion in background
			go func(sid string, aid int) {
				re.cmd.Wait()
				mu.Lock()
				delete(running, sid)
				mu.Unlock()
				slog.Info("engine exited", "session", sid)
			}(t.SessionID, t.AssignmentID)
		}
		select {
		case <-ctx.Done():
			break
		case <-time.After(5 * time.Second):
		}
	}

	// Cleanup
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

func register(client *http.Client, cfg config) error {
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
	var ais []aiReg
	for _, a := range cfg.AIs {
		cores := a.Cores; if cores == 0 { cores = 1 }
		mem := a.MemoryMB; if mem == 0 { mem = 64 }
		maxInst := a.MaxInstances; if maxInst == 0 { maxInst = 1 }
		ais = append(ais, aiReg{a.Name, a.Version, a.Created, a.ChangelogShort, a.ChangelogFull, a.BuildCmd, a.RunCmd, a.EngineID, a.EngineManifest, cores, mem, maxInst})
	}
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

func heartbeatLoop(ctx context.Context, client *http.Client, cfg config, mu *sync.Mutex, running map[string]*runningEngine) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done(): return
		case <-ticker.C:
		}
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
		for _, ai := range cfg.AIs {
			key := ai.Name + ":" + ai.Version
			count := counts[key]
			ais = append(ais, map[string]any{
				"name": ai.Name, "version": ai.Version,
				"current_matches": count, "max_concurrency": ai.MaxInstances,
			})
		}
		mu.Unlock()
		body := map[string]any{
			"coach_id": cfg.CoachID, "token": cfg.Token,
			"ais_available": ais,
			"resources": map[string]int{"cores_used": usedCores, "memory_mb_used": usedMem},
		}
		resp, err := postJSON(client, cfg, "/api/coach/heartbeat", body)
		if err != nil { slog.Warn("heartbeat failed", "err", err); continue }
		resp.Body.Close()
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

func launchEngine(ctx context.Context, ai aiConfig, arenaURL, relayPath, sessionID string) (*runningEngine, error) {
	parts := strings.Fields(ai.RunCmd)
	if len(parts) == 0 { return nil, fmt.Errorf("empty run command") }

	engCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(engCtx, parts[0], parts[1:]...)
	cmd.Stderr = os.Stderr
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start: %w", err)
	}

	// Connect to WebSocket relay
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

	// Bridge: engine stdout -> WebSocket, WebSocket -> engine stdin
	go func() {
		defer conn.Close(websocket.StatusNormalClosure, "done")
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if err := conn.Write(context.Background(), websocket.MessageText, scanner.Bytes()); err != nil {
				break
			}
		}
	}()
	go func() {
		defer stdin.Close()
		for {
			_, msg, err := conn.Read(context.Background())
			if err != nil { break }
			io.WriteString(stdin, string(msg)+"\n")
		}
	}()

	return &runningEngine{ai: ai, cmd: cmd, cancel: cancel, sessionID: sessionID}, nil
}

// ── Helpers ──────────────────────────────────────────────────────────────

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
	return 1 // conservative default
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
	// Hash the binary
	if data, err := os.ReadFile(binPath); err == nil {
		info, _ := os.Stat(binPath)
		h := sha256.Sum256(data)
		hasher.Write(h[:])
		fmt.Fprintf(&manifest, "Binary: %s\n  Size: %s\n  Modified: %s\n  SHA256: %s\n\n",
			binPath, niceSize(info.Size()), info.ModTime().Format("2006-01-02 15:04"), hex.EncodeToString(h[:])[:16])
	} else {
		fmt.Fprintf(&manifest, "Binary: %s (not found)\n\n", binPath)
	}

	// Hash companion data (look in same dir as binary, and ../data, ../Lib, ../Database)
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
	return 256 // conservative default
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
