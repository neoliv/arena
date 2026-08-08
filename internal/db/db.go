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
		"ALTER TABLE api_tokens ADD COLUMN nickname TEXT DEFAULT ''",
		"ALTER TABLE engines ADD COLUMN created TEXT DEFAULT ''",
		"ALTER TABLE engines ADD COLUMN changelog_short TEXT DEFAULT ''",
		"ALTER TABLE engines ADD COLUMN changelog_full TEXT DEFAULT ''",
		"ALTER TABLE engines ADD COLUMN engine_id TEXT DEFAULT ''",
		"ALTER TABLE engines ADD COLUMN engine_manifest TEXT DEFAULT ''",
		// Fix created_at: old schema used TEXT datetime('now'), new uses unixepoch().
		"UPDATE games SET created_at = COALESCE(unixepoch(created_at), 0) WHERE typeof(created_at) = 'text'",
		"ALTER TABLE games ADD COLUMN disconnect INTEGER DEFAULT 0",
		"ALTER TABLE games ADD COLUMN error_code INTEGER DEFAULT 0",
		"ALTER TABLE games ADD COLUMN game_time_sec REAL DEFAULT 0",
		"ALTER TABLE game_moves ADD COLUMN flags TEXT DEFAULT ''",
		"CREATE TABLE IF NOT EXISTS game_moves (id INTEGER PRIMARY KEY AUTOINCREMENT, game_id INTEGER REFERENCES games(id), move_num INTEGER NOT NULL, side TEXT NOT NULL, move TEXT NOT NULL DEFAULT '', nodes INTEGER DEFAULT 0, depth INTEGER DEFAULT 0, time_ms REAL DEFAULT 0, score INTEGER DEFAULT 0, flags TEXT DEFAULT '')",
		// idx_games_created on games(created_at) is ASC by default; SQLite reads
		// indices backwards so DESC queries are also served by this index.
		"CREATE INDEX IF NOT EXISTS idx_games_created ON games(created_at)",
		// settings table — key/value store for arena settings (game budget, ...)
		"CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '')",
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
	    created_at      INTEGER DEFAULT (unixepoch()),
    UNIQUE(name, version)
);

CREATE TABLE IF NOT EXISTS games (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    match_id      INTEGER DEFAULT 0,
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
    disconnect    INTEGER DEFAULT 0,
    error_code    INTEGER DEFAULT 0,
    game_time_sec REAL DEFAULT 0,
    created_at    INTEGER DEFAULT (unixepoch())
);

CREATE TABLE IF NOT EXISTS elo_history (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    engine_id     INTEGER REFERENCES engines(id),
    opponent_id   INTEGER REFERENCES engines(id),
    match_id      INTEGER DEFAULT 0,
    rating_before REAL NOT NULL,
    rating_after  REAL NOT NULL,
    games          INTEGER NOT NULL DEFAULT 0,
    wins           INTEGER DEFAULT 0,
    losses         INTEGER DEFAULT 0,
    draws          INTEGER DEFAULT 0,
    created_at    INTEGER DEFAULT (unixepoch())
);

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
    score      INTEGER DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_gm_game ON game_moves(game_id);
CREATE INDEX IF NOT EXISTS idx_elo_created ON elo_history(created_at);

CREATE TABLE IF NOT EXISTS api_tokens (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    token       TEXT NOT NULL UNIQUE,
    email       TEXT NOT NULL DEFAULT '',
    nickname    TEXT DEFAULT '',
    comment     TEXT DEFAULT '',
    created_at  INTEGER DEFAULT (unixepoch()),
    last_used   TEXT DEFAULT '',
    use_count   INTEGER DEFAULT 0,
    active      INTEGER DEFAULT 1
);

CREATE TABLE IF NOT EXISTS web_sessions (
    id          TEXT PRIMARY KEY,
    token       TEXT NOT NULL,
    email       TEXT NOT NULL DEFAULT '',
    created_at  INTEGER DEFAULT (unixepoch())
);

CREATE TABLE IF NOT EXISTS settings (
    key         TEXT PRIMARY KEY,
    value       TEXT NOT NULL DEFAULT ''
);
`
