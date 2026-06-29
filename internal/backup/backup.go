// Package backup provides automatic zstd-compressed backups of the SQLite database
// with configurable retention. Pattern adapted from avh.
package backup

import (
	"fmt"
	"io"
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

type Manager struct {
	dbPath     string
	backupDir  string
	maxBackups int
	lastBackup time.Time
}

func New(dbPath string) *Manager {
	return &Manager{
		dbPath:     dbPath,
		backupDir:  filepath.Join(filepath.Dir(dbPath), "backup"),
		maxBackups: 16, // ~7 days at 12h interval
	}
}

func (m *Manager) Run() { go m.loop() }

func (m *Manager) loop() {
	for {
		time.Sleep(30 * time.Minute)
		m.maybeBackup()
	}
}

func (m *Manager) maybeBackup() {
	// Skip if we already backed up recently (survives restarts by checking filesystem).
	if latest := FindLatestBackup(m.dbPath); latest != "" {
		if info, err := os.Stat(latest); err == nil && time.Since(info.ModTime()) < 12*time.Hour {
			return
		}
	}
	if time.Since(m.lastBackup) < 12*time.Hour { return }
	if err := m.doBackup(); err != nil {
		fmt.Fprintf(os.Stderr, "backup: %v\n", err)
		return
	}
	m.lastBackup = time.Now()
	m.rotate()
}

func (m *Manager) doBackup() error {
	if err := os.MkdirAll(m.backupDir, 0755); err != nil { return err }
	tmpPath := filepath.Join(m.backupDir, "arena-tmp.db")
	name := filepath.Join(m.backupDir, "arena-"+time.Now().Format("2006-01-02T15-04")+".db.zst")
	// Use VACUUM INTO for a consistent snapshot (captures WAL).
	db, err := sql.Open("sqlite", m.dbPath+"?_journal_mode=WAL&_busy_timeout=30000")
	if err != nil { return err }
	defer db.Close()
	// Validate backup path to prevent SQL injection (VACUUM INTO does not support parameters).
	for _, c := range tmpPath {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '/' || c == '.' || c == '-' || c == '_') {
			return fmt.Errorf("invalid backup path character: %c", c)
		}
	}
	_, err = db.Exec(fmt.Sprintf("VACUUM INTO '%s'", tmpPath))
	if err != nil { return fmt.Errorf("vacuum: %w", err) }
	defer os.Remove(tmpPath)
	// Compress
	src, err := os.Open(tmpPath)
	if err != nil { return err }
	defer src.Close()
	dst, err := os.Create(name)
	if err != nil { return err }
	defer dst.Close()
	enc, err := zstd.NewWriter(dst)
	if err != nil { return err }
	defer enc.Close()
	if _, err := io.Copy(enc, src); err != nil { return err }
	enc.Close()
	srcInfo, _ := src.Stat()
	dstInfo, _ := dst.Stat()
	fmt.Printf("backup: %s (%d → %d octets, zstd)\n", filepath.Base(name), srcInfo.Size(), dstInfo.Size())
	return nil
}

func (m *Manager) rotate() {
	entries, _ := filepath.Glob(filepath.Join(m.backupDir, "arena-*.db.zst"))
	if len(entries) <= m.maxBackups { return }
	sort.Strings(entries)
	for _, path := range entries[:len(entries)-m.maxBackups] {
		os.Remove(path)
	}
}

func FindLatestBackup(dbPath string) string {
	dir := filepath.Join(filepath.Dir(dbPath), "backup")
	entries, _ := filepath.Glob(filepath.Join(dir, "arena-*.db.zst"))
	if len(entries) == 0 { return "" }
	sort.Strings(entries)
	return entries[len(entries)-1]
}

func RestoreBackup(dbPath, backupPath string) error {
	src, err := os.Open(backupPath)
	if err != nil { return fmt.Errorf("open backup %s: %w", backupPath, err) }
	defer src.Close()
	dec, err := zstd.NewReader(src)
	if err != nil { return fmt.Errorf("zstd reader: %w", err) }
	defer dec.Close()
	dst, err := os.Create(dbPath)
	if err != nil { return fmt.Errorf("create db %s: %w", dbPath, err) }
	defer dst.Close()
	if _, err := io.Copy(dst, dec); err != nil { return fmt.Errorf("restore: %w", err) }
	os.Remove(dbPath + "-wal")
	os.Remove(dbPath + "-shm")
	return nil
}

func (m *Manager) Size() int64 {
	entries, _ := filepath.Glob(filepath.Join(m.backupDir, "arena-*.db.zst"))
	var total int64
	for _, e := range entries {
		if info, _ := os.Stat(e); info != nil { total += info.Size() }
	}
	return total
}

func (m *Manager) Count() int {
	entries, _ := filepath.Glob(filepath.Join(m.backupDir, "arena-*.db.zst"))
	return len(entries)
}

func TrimSuffix(s, suffix string) string { return strings.TrimSuffix(s, suffix) }
