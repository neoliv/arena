// arena-server is the central results database + web dashboard for the Othello Arena.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"

	"github.com/neoliv/arena/internal/api"
	"github.com/neoliv/arena/internal/backup"
	"github.com/neoliv/arena/internal/coach"
	"github.com/neoliv/arena/internal/db"
	"github.com/neoliv/arena/internal/matchmaker"
	"github.com/neoliv/arena/internal/stats"
	"github.com/neoliv/arena/internal/version"
	"github.com/neoliv/arena/internal/web"
	"gopkg.in/yaml.v3"
)

type statusRecorder struct {
	http.ResponseWriter
	status   int
	bytesOut int64
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytesOut += int64(n)
	return n, err
}

// Hijack supports WebSocket upgrades by delegating to the underlying writer.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := r.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("Hijack not supported")
}

func main() {
	var (
		dbPath   = flag.String("db", envDefault("ARENA_DB", "/opt/arena/arena.db"), "SQLite database path")
		addr     = flag.String("addr", envDefault("LISTEN_ADDR", ":8500"), "HTTP listen address")
		token    = flag.String("token", envDefault("ARENA_TOKEN", ""), "Master API token (also checks DB tokens)")
		newToken = flag.String("new-token", "", "Generate a new API token for the given email and exit")
		showVer  = flag.Bool("version", false, "Print version and exit")
	)
	handleShortFlags("arena-server")
	flag.Parse()

	if *showVer {
		fmt.Print(version.PrintVersion("arena-server"))
		return
	}

	database, err := db.Open(*dbPath)
	if err != nil {
		slog.Error("database open", "err", err)
		os.Exit(1)
	}
	defer database.Close()

	if err := database.Migrate(); err != nil {
		slog.Error("migration", "err", err)
		os.Exit(1)
	}

	if *newToken != "" {
		fmt.Print(database.PrintNewToken(*newToken, ""))
		return
	}

	slog.Info("arena-server starting", "addr", *addr, "db", *dbPath)
	slog.Info("database ready")

	bm := backup.New(*dbPath)
	bm.Run()
	slog.Info("backup manager started", "dir", filepath.Dir(*dbPath)+"/backup", "max", 63)

	validateToken := func(t string) bool {
		if t == "" { return false }
		if *token != "" && t == *token { return true }
		valid, _ := database.ValidateToken(t)
		return valid
	}

	mux := http.NewServeMux()
	apiServer := &api.Server{DB: database, Token: *token, ValidateToken: validateToken}
	apiServer.RegisterRoutes(mux)

	relay := coach.NewRelay()
	var serverGen [8]byte
	rand.Read(serverGen[:])
	coachHandler := &coach.Handler{DB: database, Token: *token, Relay: relay, ValidateToken: validateToken, ServerGen: hex.EncodeToString(serverGen[:])}
	mux.HandleFunc("POST /api/coach/register", coachHandler.HandleRegister)
	mux.HandleFunc("POST /api/coach/heartbeat", coachHandler.HandleHeartbeat)
	mux.HandleFunc("GET /api/coach/tasks", coachHandler.HandleTasks)
	mux.HandleFunc("POST /api/coach/tasks/{id}/status", coachHandler.HandleTaskStatus)
	mux.HandleFunc("GET /api/relay/{session_id}", relay.HandleRelay)

	mmCfg := matchmaker.DefaultConfig()
	if data, err := os.ReadFile("matchmaker.yaml"); err == nil {
		if err := yaml.Unmarshal(data, &mmCfg); err != nil {
			slog.Warn("parse matchmaker.yaml, using defaults", "err", err)
		}
	}
	if mmCfg.Token == "" { mmCfg.Token = *token }
	if mmCfg.ArenaURL == "" { mmCfg.ArenaURL = "https://arena.arsac.org" }

	mm := matchmaker.New(database, relay, mmCfg)
	coachHandler.SetMatchMaker(mm.OnBothReady)
	mux.HandleFunc("GET /api/matchmaker/status", mm.HandleStatus)
	go mm.Run()

	sessions := web.NewSessionStore(database)
	limiter := web.NewRateLimiter()
	webHandler := &web.Handler{DB: database, Token: *token, Sessions: sessions, Limiter: limiter}
	webHandler.RegisterRoutes(mux)

handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				buf := make([]byte, 4096)
				n := runtime.Stack(buf, false)
				slog.Error("panic in handler", "path", r.URL.Path, "panic", rec, "stack", string(buf[:n]))
				http.Error(w, "internal server error", 500)
			}
		}()
		rw := &statusRecorder{ResponseWriter: w, status: 200}
		mux.ServeHTTP(rw, r)
		stats.Global.Record(int(r.ContentLength), int(rw.bytesOut))
		if rw.status == 404 || rw.status == 0 {
		}
	})

	slog.Info("listening", "addr", *addr)
	if err := http.ListenAndServe(*addr, handler); err != nil {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" { return v }
	return def
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
