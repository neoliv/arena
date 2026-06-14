package db

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// GenerateToken creates a cryptographically random token string.
func GenerateToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// InsertToken stores a new API token in the database.
func (db *DB) InsertToken(token, email, comment string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec("INSERT INTO api_tokens (token, email, comment, created_at) VALUES (?, ?, ?, ?)",
		token, email, comment, now)
	return err
}

// ValidateToken checks if a token is valid and records usage.
// Returns true and the email if valid, false if not.
func (db *DB) ValidateToken(token string) (bool, string) {
	if token == "" {
		return false, ""
	}
	var email string
	var active int
	err := db.QueryRow("SELECT email, active FROM api_tokens WHERE token=?", token).Scan(&email, &active)
	if err != nil || active == 0 {
		return false, ""
	}
	// Record usage asynchronously (best-effort)
	now := time.Now().UTC().Format(time.RFC3339)
	db.Exec("UPDATE api_tokens SET last_used=?, use_count=use_count+1 WHERE token=?", now, token)
	return true, email
}

// ListTokens returns all tokens with their metadata (hides the token value for security).
func (db *DB) ListTokens() ([]map[string]any, error) {
	rows, err := db.Query("SELECT id, SUBSTR(token,1,8)||'...', email, COALESCE(comment,''), created_at, COALESCE(last_used,''), use_count FROM api_tokens ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, count int
		var token, email, comment, created, lastUsed string
		rows.Scan(&id, &token, &email, &comment, &created, &lastUsed, &count)
		out = append(out, map[string]any{
			"id": id, "token_prefix": token, "email": email, "comment": comment,
			"created_at": created, "last_used": lastUsed, "use_count": count,
		})
	}
	return out, nil
}

// HasTokens returns true if at least one token exists in the database.
func (db *DB) HasTokens() (bool, error) {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM api_tokens").Scan(&count)
	return count > 0, err
}

// PrintNewToken generates a token, stores it, and returns a human-readable message.
func (db *DB) PrintNewToken(email, comment string) string {
	token := GenerateToken()
	if err := db.InsertToken(token, email, comment); err != nil {
		return fmt.Sprintf("Error storing token: %v", err)
	}
	return fmt.Sprintf(`
╔══════════════════════════════════════════════════════════════╗
║  New API token generated for: %-30s ║
║  Comment: %-50s ║
║                                                            ║
║  Token: %-48s ║
║                                                            ║
║  Use this token in your coach configuration:               ║
║                                                            ║
║    Option 1 (recommended) — environment variable:          ║
║      export ARENA_TOKEN=%s ║
║                                                            ║
║    Option 2 — coach.yaml (not recommended, may leak):      ║
║      token: "%s" ║
║                                                            ║
║  The coach reads ARENA_TOKEN from the environment          ║
║  automatically if token is not set in coach.yaml.          ║
╚══════════════════════════════════════════════════════════════╝
`, email, comment, token, token, token)
}
