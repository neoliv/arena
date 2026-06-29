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
	"time"

	"github.com/neoliv/arena/internal/api"
	"github.com/neoliv/arena/internal/backup"
	"github.com/neoliv/arena/internal/coach"
	"github.com/neoliv/arena/internal/db"
	"github.com/neoliv/arena/internal/matchmaker"
	"github.com/neoliv/arena/internal/stats"
	"github.com/neoliv/arena/internal/version"
	"github.com/neoliv/arena/internal/web"
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
		newToken     = flag.String("new-token", "", "Generate a new API token for the given email and exit")
		recomputeElo = flag.Bool("recompute-elo", false, "Recompute Elo for all engines from game history and exit")
		showVer      = flag.Bool("version", false, "Print version and exit")
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
	if database.Rollback {
		web.SetRollbackBanner()
		slog.Warn("database restored from backup — recent games may be missing")
	}

	if err := database.Migrate(); err != nil {
		slog.Error("migration", "err", err)
		os.Exit(1)
	}

	if *newToken != "" {
		fmt.Print(database.PrintNewToken(*newToken, ""))
		return
	}
	if *recomputeElo {
		apiSrv := &api.Server{DB: database}
		rows, _ := database.Query("SELECT DISTINCT id FROM engines")
		if rows != nil {
			var ids []int
			for rows.Next() {
				var id int
				rows.Scan(&id)
				ids = append(ids, id)
			}
			rows.Close()
			for _, id := range ids {
				apiSrv.RecomputeElo(id)
			}
			fmt.Printf("Recomputed Elo for %d engines\n", len(ids))
		}
		return
	}

	slog.Info("arena-server starting", "addr", *addr, "db", *dbPath)
	slog.Info("database ready")

	bm := backup.New(*dbPath)
	bm.Run()
	slog.Info("backup manager started", "dir", filepath.Dir(*dbPath)+"/backup", "max", 63)

	validateToken := func(t string) bool {
		if t == "" {
			return false
		}
		if *token != "" && t == *token {
			return true
		}
		valid, _ := database.ValidateToken(t)
		return valid
	}

	mux := http.NewServeMux()
	apiServer := &api.Server{DB: database, Token: *token, ValidateToken: validateToken}
	apiServer.RegisterRoutes(mux)

	relay := coach.NewRelay()
	var serverGen [8]byte
	rand.Read(serverGen[:])
	coachHandler := coach.NewHandler(database, *token, relay, validateToken, hex.EncodeToString(serverGen[:]))
	coachErrors := coach.NewCoachErrorStore()
	coachHandler.ErrorStore = coachErrors

	// Coach endpoints (DB persistence)
	mux.HandleFunc("POST /api/coach/register", coachHandler.HandleRegister)
	mux.HandleFunc("POST /api/coach/heartbeat", coachHandler.HandleHeartbeat)
	mux.HandleFunc("POST /api/coach/engine-error", coachHandler.HandleEngineError)

	// Relay endpoint (WebSocket upgrade for game sessions)
	mux.HandleFunc("GET /api/relay/{session_id}", relay.HandleRelay)

	// Matchmaker (in-memory engine registry + pull-based assignment)
	mm := matchmaker.New(database, relay)
	mm.ErrorStore = coachErrors
	// Wire heartbeat → in-memory coach state (replaces old DB coach_ais.instances_running).
	coachHandler.OnHeartbeat = func(coachID string, sessionID string, aiUpdates map[string]int) bool {
		return mm.OnCoachHeartbeat(coachID, sessionID, aiUpdates)
	}
	if err := matchmaker.InitTrace("/var/log/arena/game_trace.log"); err != nil {
		slog.Warn("game trace disabled", "err", err)
	}
	mux.HandleFunc("GET /api/matchmaker/status", mm.HandleStatus)
	mux.HandleFunc("POST /api/matchmaker/register", mm.HandleRegister)
	mux.HandleFunc("GET /api/matchmaker/poll", mm.HandlePoll)
	mux.HandleFunc("POST /api/matchmaker/complete", mm.HandleComplete)

	// Per-player resource stats (coach reports real CPU/RAM every ~20s).
	resourceStore := coach.NewPlayerResourceStore()
	mux.HandleFunc("POST /api/coach/resources", resourceStore.HandleResources)

	sessions := web.NewSessionStore(database)
	limiter := web.NewRateLimiter()
	webHandler := &web.Handler{DB: database, Token: *token, Sessions: sessions, Limiter: limiter, EngineStatusFunc: mm.EngineStatus, CoachStatusFunc: mm.CoachStatus, ActiveAssignmentsFunc: mm.ActiveAssignments, ResourceStore: resourceStore}
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
	srv := &http.Server{Addr: *addr, Handler: handler, ReadTimeout: 15 * time.Second, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// handleShortFlags is duplicated across cmd/*/main.go.
// Canonical source: cmd/coach/main.go
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
