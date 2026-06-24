// Package db provides SQLite connectivity and schema for the arena.
package db

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/neoliv/arena/internal/backup"
	_ "modernc.org/sqlite"
)

// DB wraps the SQL connection pool.
type DB struct {
	*sql.DB
	Rollback bool // true if DB was restored from backup
}

// Open creates or opens the SQLite database at the given path.
// If the database is corrupted, it tries to restore from the latest backup.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	dsn := path + "?_journal_mode=WAL&_busy_timeout=30000&_foreign_keys=on"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	conn.SetMaxOpenConns(4)
	conn.SetMaxIdleConns(2)
	if err := conn.Ping(); err != nil {
		conn.Close()
		// Try restoring from latest backup.
		backupDir := filepath.Join(filepath.Dir(path), "backup")
		entries, _ := filepath.Glob(filepath.Join(backupDir, "arena-*.db.zst"))
		if len(entries) > 0 {
			sort.Strings(entries)
			latest := entries[len(entries)-1]
			fmt.Fprintf(os.Stderr, "db: ping failed (%v), restoring from %s\n", err, filepath.Base(latest))
			if err := backup.RestoreBackup(path, latest); err != nil {
				return nil, fmt.Errorf("restore backup: %w", err)
			}
			// Retry open.
			conn2, err := sql.Open("sqlite", dsn)
			if err != nil {
				return nil, fmt.Errorf("open after restore: %w", err)
			}
			conn2.SetMaxOpenConns(4)
			conn2.SetMaxIdleConns(2)
			if err := conn2.Ping(); err != nil {
				conn2.Close()
				return nil, fmt.Errorf("ping after restore: %w", err)
			}
			return &DB{conn2, true}, nil
		}
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &DB{conn, false}, nil
}

// Migrate creates the schema.
func (db *DB) Migrate() error {
	slog.Info("running database migrations")
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	// Add columns that may not exist in older databases
	for _, stmt := range []string{
		"ALTER TABLE coaches ADD COLUMN version TEXT DEFAULT ''",
		"ALTER TABLE coaches ADD COLUMN session_id TEXT DEFAULT ''",
		"ALTER TABLE api_tokens ADD COLUMN nickname TEXT DEFAULT ''",
		"ALTER TABLE coach_ais ADD COLUMN created TEXT DEFAULT ''",
		"ALTER TABLE coach_ais ADD COLUMN changelog_short TEXT DEFAULT ''",
		"ALTER TABLE coach_ais ADD COLUMN changelog_full TEXT DEFAULT ''",
		"ALTER TABLE coach_ais ADD COLUMN engine_id TEXT DEFAULT ''",
		"ALTER TABLE coach_ais ADD COLUMN engine_manifest TEXT DEFAULT ''",
		"ALTER TABLE engines ADD COLUMN created TEXT DEFAULT ''",
		"ALTER TABLE engines ADD COLUMN changelog_short TEXT DEFAULT ''",
		"ALTER TABLE engines ADD COLUMN changelog_full TEXT DEFAULT ''",
		"ALTER TABLE engines ADD COLUMN engine_id TEXT DEFAULT ''",
		"ALTER TABLE engines ADD COLUMN engine_manifest TEXT DEFAULT ''",
			"ALTER TABLE games ADD COLUMN disconnect INTEGER DEFAULT 0",
			"CREATE TABLE IF NOT EXISTS game_moves (id INTEGER PRIMARY KEY AUTOINCREMENT, game_id INTEGER REFERENCES games(id), move_num INTEGER NOT NULL, side TEXT NOT NULL, move TEXT NOT NULL DEFAULT '', nodes INTEGER DEFAULT 0, depth INTEGER DEFAULT 0, time_ms REAL DEFAULT 0, score INTEGER DEFAULT 0)",
			"CREATE INDEX IF NOT EXISTS idx_gm_game ON game_moves(game_id)",
	} {
		db.Exec(stmt) // ignore errors — column may already exist
	}
	return nil
}

const schema = `
CREATE TABLE IF NOT EXISTS engines (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT NOT NULL,
    version       TEXT NOT NULL,
    git_commit    TEXT,
    git_repo      TEXT,
    protocol      TEXT DEFAULT 'gtp',
    submitted_by  TEXT,
    created         TEXT DEFAULT '',
	    changelog_short TEXT DEFAULT '',
	    changelog_full  TEXT DEFAULT '',
	    created_at      TEXT DEFAULT (datetime('now')),
    UNIQUE(name, version)
);

CREATE TABLE IF NOT EXISTS matches (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    engine1_id    INTEGER REFERENCES engines(id),
    engine2_id    INTEGER REFERENCES engines(id),
    time_control  TEXT NOT NULL DEFAULT '{}',
    opening_book  TEXT,
    runner_id     TEXT,
    total_games   INTEGER NOT NULL DEFAULT 0,
    wins_1        INTEGER DEFAULT 0,
    wins_2        INTEGER DEFAULT 0,
    draws         INTEGER DEFAULT 0,
    created_at    TEXT DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS games (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    match_id      INTEGER REFERENCES matches(id),
    game_number   INTEGER NOT NULL,
    black_id      INTEGER REFERENCES engines(id),
    white_id      INTEGER REFERENCES engines(id),
    result        TEXT NOT NULL,
    final_score   INTEGER,
    opening_line  TEXT,
    pgn           TEXT NOT NULL DEFAULT '',
    black_time_s  REAL,
    white_time_s  REAL,
    black_nodes   INTEGER,
    white_nodes   INTEGER,
    black_depth   INTEGER,
    white_depth   INTEGER,
    created_at    TEXT DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS elo_history (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    engine_id     INTEGER REFERENCES engines(id),
    opponent_id   INTEGER REFERENCES engines(id),
    match_id      INTEGER REFERENCES matches(id),
    rating_before REAL NOT NULL,
    rating_after  REAL NOT NULL,
    games          INTEGER NOT NULL DEFAULT 0,
    wins           INTEGER DEFAULT 0,
    losses         INTEGER DEFAULT 0,
    draws          INTEGER DEFAULT 0,
    created_at    TEXT DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS bisections (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    engine_name   TEXT NOT NULL,
    good_commit   TEXT NOT NULL,
    bad_commit    TEXT NOT NULL,
    ref_engine_id INTEGER REFERENCES engines(id),
    games_per_step INTEGER DEFAULT 100,
    status        TEXT DEFAULT 'pending',
    current_good  TEXT,
    current_bad   TEXT,
    created_at    TEXT DEFAULT (datetime('now')),
    finished_at   TEXT
);

CREATE TABLE IF NOT EXISTS speed_stats (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    engine_id     INTEGER REFERENCES engines(id),
    match_id      INTEGER REFERENCES matches(id),
    ply           INTEGER NOT NULL,
    total_nodes   INTEGER NOT NULL DEFAULT 0,
    total_time_s  REAL NOT NULL DEFAULT 0.0,
    total_depth   INTEGER NOT NULL DEFAULT 0,
    timeouts      INTEGER NOT NULL DEFAULT 0,
    total_nps     INTEGER NOT NULL DEFAULT 0,
    total_branch  INTEGER NOT NULL DEFAULT 0,
    total_empties INTEGER NOT NULL DEFAULT 0,
    sample_count  INTEGER NOT NULL DEFAULT 1,
    created_at    TEXT DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_speed_engine ON speed_stats(engine_id);
CREATE INDEX IF NOT EXISTS idx_speed_ply ON speed_stats(ply);

CREATE TABLE IF NOT EXISTS bisect_steps (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    bisection_id  INTEGER REFERENCES bisections(id),
    commit_hash   TEXT NOT NULL,
    elo_result    REAL,
    verdict       TEXT,
    games_played  INTEGER,
    created_at    TEXT DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_games_match ON games(match_id);
CREATE INDEX IF NOT EXISTS idx_games_black ON games(black_id);
CREATE INDEX IF NOT EXISTS idx_games_white ON games(white_id);
CREATE INDEX IF NOT EXISTS idx_elo_engine ON elo_history(engine_id);

CREATE TABLE IF NOT EXISTS game_moves (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    game_id    INTEGER REFERENCES games(id),
    move_num   INTEGER NOT NULL,
    side       TEXT NOT NULL,
    move       TEXT NOT NULL DEFAULT '',
    nodes      INTEGER DEFAULT 0,
    depth      INTEGER DEFAULT 0,
    time_ms    REAL DEFAULT 0,
    score      INTEGER DEFAULT 0,
    nps        INTEGER DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_gm_game ON game_moves(game_id);
CREATE INDEX IF NOT EXISTS idx_elo_created ON elo_history(created_at);

CREATE TABLE IF NOT EXISTS coaches (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    coach_id      TEXT NOT NULL UNIQUE,
    version       TEXT DEFAULT '',
    label         TEXT DEFAULT '',
    cores_total   INTEGER NOT NULL DEFAULT 0,
    memory_mb_total INTEGER NOT NULL DEFAULT 0,
    last_seen     TEXT,
    created_at    TEXT DEFAULT (datetime('now')),
    updated_at    TEXT DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS coach_ais (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    coach_id            INTEGER NOT NULL REFERENCES coaches(id),
    engine_name         TEXT NOT NULL,
    engine_version      TEXT NOT NULL,
    cores_per_instance  INTEGER DEFAULT 1,
    memory_mb_per_instance INTEGER DEFAULT 64,
    max_instances       INTEGER DEFAULT 1,
    instances_running   INTEGER DEFAULT 0,
    run_cmd             TEXT DEFAULT '',
    build_cmd           TEXT DEFAULT '',
    engine_id           TEXT DEFAULT '',
    engine_manifest     TEXT DEFAULT '',
    is_available        INTEGER DEFAULT 0,
    created_at          TEXT DEFAULT (datetime('now')),
    updated_at          TEXT DEFAULT (datetime('now')),
    created           TEXT DEFAULT '',
    changelog_short   TEXT DEFAULT '',
    changelog_full    TEXT DEFAULT '',
    UNIQUE(coach_id, engine_name, engine_version)
);

CREATE TABLE IF NOT EXISTS match_assignments (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    engine1_id    INTEGER NOT NULL REFERENCES engines(id),
    engine2_id    INTEGER NOT NULL REFERENCES engines(id),
    coach1_ai_id  INTEGER NOT NULL REFERENCES coach_ais(id),
    coach2_ai_id  INTEGER NOT NULL REFERENCES coach_ais(id),
    time_control  TEXT NOT NULL DEFAULT '{"type":"total","seconds":60}',
    num_games     INTEGER NOT NULL DEFAULT 10,
    session1_id   TEXT DEFAULT '',
    session2_id   TEXT DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'pending',
    decline_reason TEXT DEFAULT '',
    retry_count   INTEGER DEFAULT 0,
    retry_after   TEXT,
    created_at    TEXT DEFAULT (datetime('now')),
    assigned_at   TEXT,
    ready_at      TEXT,
    in_progress_at TEXT,
    completed_at  TEXT
);

CREATE INDEX IF NOT EXISTS idx_coach_ais_available ON coach_ais(is_available);
CREATE INDEX IF NOT EXISTS idx_assignments_status ON match_assignments(status);
CREATE INDEX IF NOT EXISTS idx_assignments_coach1 ON match_assignments(coach1_ai_id);
CREATE INDEX IF NOT EXISTS idx_assignments_coach2 ON match_assignments(coach2_ai_id);

CREATE TABLE IF NOT EXISTS api_tokens (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    token       TEXT NOT NULL UNIQUE,
    email       TEXT NOT NULL DEFAULT '',
    nickname    TEXT DEFAULT '',
    comment     TEXT DEFAULT '',
    created_at  TEXT DEFAULT (datetime('now')),
    last_used   TEXT DEFAULT '',
    use_count   INTEGER DEFAULT 0,
    active      INTEGER DEFAULT 1
);

CREATE TABLE IF NOT EXISTS web_sessions (
    id          TEXT PRIMARY KEY,
    token       TEXT NOT NULL,
    email       TEXT NOT NULL DEFAULT '',
    created_at  TEXT DEFAULT (datetime('now'))
);
`
