package web

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/neoliv/arena/internal/db"
)

// RateLimiter tracks login attempts per IP with a sliding window.
type RateLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}

// NewRateLimiter creates a rate limiter allowing maxAttempts per window.
func NewRateLimiter() *RateLimiter {
	rl := &RateLimiter{attempts: make(map[string][]time.Time)}
	go func() {
		for range time.Tick(5 * time.Minute) {
			rl.mu.Lock()
			cutoff := time.Now().Add(-5 * time.Minute)
			for ip, times := range rl.attempts {
				var recent []time.Time
				for _, t := range times {
					if t.After(cutoff) { recent = append(recent, t) }
				}
				if len(recent) == 0 { delete(rl.attempts, ip) } else { rl.attempts[ip] = recent }
			}
			rl.mu.Unlock()
		}
	}()
	return rl
}

// Allow returns true if the request is allowed, false if rate limited.
func (rl *RateLimiter) Allow(ip string, maxAttempts int, window time.Duration) (bool, time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-window)
	var recent []time.Time
	for _, t := range rl.attempts[ip] {
		if t.After(cutoff) { recent = append(recent, t) }
	}
	rl.attempts[ip] = recent
	if len(recent) >= maxAttempts {
		wait := recent[0].Add(window).Sub(now)
		return false, wait
	}
	rl.attempts[ip] = append(recent, now)
	return true, 0
}

// Session holds an authenticated web session.
type Session struct {
	Token     string
	Email     string
	CreatedAt time.Time
}

// SessionStore manages web sessions with DB persistence.
// Memory cache provides fast lookups; DB ensures survival across restarts.
type SessionStore struct {
	mu       sync.Mutex
	cache    map[string]*Session
	db       *db.DB
}

// NewSessionStore creates a new session store backed by the database.
func NewSessionStore(database *db.DB) *SessionStore {
	ss := &SessionStore{cache: make(map[string]*Session), db: database}
	// Clean expired sessions from DB periodically
	go func() {
		for range time.Tick(1 * time.Hour) {
			cutoff := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
			ss.db.Exec("DELETE FROM web_sessions WHERE created_at < ?", cutoff)
		}
	}()
	return ss
}

// Create stores a new session and returns the session ID.
func (ss *SessionStore) Create(token, email string) string {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	b := make([]byte, 32)
	rand.Read(b)
	sid := hex.EncodeToString(b)
	now := time.Now().UTC().Format(time.RFC3339)
	ss.db.Exec("INSERT INTO web_sessions (id, token, email, created_at) VALUES (?, ?, ?, ?)", sid, token, email, now)
	ss.cache[sid] = &Session{Token: token, Email: email, CreatedAt: time.Now()}
	return sid
}

// Validate checks a session ID and returns the session if valid.
func (ss *SessionStore) Validate(sid string) *Session {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	// Check cache first
	if s, ok := ss.cache[sid]; ok {
		if time.Since(s.CreatedAt) < 24*time.Hour { return s }
		delete(ss.cache, sid)
		return nil
	}
	// Fall back to DB (survives server restarts)
	var token, email, createdStr string
	err := ss.db.QueryRow("SELECT token, email, created_at FROM web_sessions WHERE id=?", sid).Scan(&token, &email, &createdStr)
	if err != nil { return nil }
	created, _ := time.Parse(time.RFC3339, createdStr)
	if time.Since(created) > 24*time.Hour {
		ss.db.Exec("DELETE FROM web_sessions WHERE id=?", sid)
		return nil
	}
	s := &Session{Token: token, Email: email, CreatedAt: created}
	ss.cache[sid] = s // populate cache
	return s
}

// Destroy removes a session from both cache and DB.
func (ss *SessionStore) Destroy(sid string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	delete(ss.cache, sid)
	ss.db.Exec("DELETE FROM web_sessions WHERE id=?", sid)
}

// Login page handler.
func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		ip := r.RemoteAddr
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			ip = strings.Split(fwd, ",")[0]
		}
		if ok, wait := h.Limiter.Allow(ip, 5, time.Minute); !ok {
			w.Header().Set("Retry-After", fmt.Sprintf("%.0f", wait.Seconds()))
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintf(w, "Too many attempts. Try again in %.0f seconds.", wait.Seconds())
			return
		}
		r.ParseForm()
		token := strings.TrimSpace(r.FormValue("token"))
		if token == "" {
			h.renderLogin(w, "Please enter a token.")
			return
		}
		valid, email := h.DB.ValidateToken(token)
		if !valid {
			h.renderLogin(w, "Invalid token.")
			return
		}
		sid := h.Sessions.Create(token, email)
		http.SetCookie(w, &http.Cookie{
			Name:     "arena_session",
			Value:    sid,
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   86400, // 24 hours
		})
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	h.renderLogin(w, "")
}

func (h *Handler) renderLogin(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, `<!DOCTYPE html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Othello Arena — Login</title><style>`+sharedCSS+`body{display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0;background:var(--bg)}.box{background:var(--bg2);padding:2em;border-radius:8px;border:1px solid var(--border);max-width:400px;width:100%}h1{text-align:center;margin:0 0 1em}input{width:100%;padding:.6em;border:1px solid var(--border);border-radius:4px;background:var(--bg);color:var(--fg);font:inherit;box-sizing:border-box}button{width:100%;padding:.6em;margin-top:1em;background:var(--accent);color:#fff;border:none;border-radius:4px;font:inherit;cursor:pointer}.error{color:#e55;text-align:center;margin-bottom:1em}</style></head><body><div class="box"><h1>Othello Arena</h1>`)
	if msg != "" { fmt.Fprintf(w, `<p class="error">%s</p>`, msg) }
	io.WriteString(w, `<form method="post" autocomplete="off"><input type="text" name="token" placeholder="Enter your API token" autocomplete="off" autofocus><button type="submit">Sign in</button></form></div></body></html>`)
}

// HandleLogout destroys the session and redirects to login.
func (h *Handler) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("arena_session"); err == nil {
		h.Sessions.Destroy(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "arena_session", Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// RequireLogin is middleware that redirects to /login if no valid session exists.
// When no auth is configured, it allows all requests through.
func (h *Handler) RequireLogin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// If auth is disabled (no tokens configured at all), allow through
		hasTokens, _ := h.DB.HasTokens()
		if h.Token == "" && !hasTokens {
			next(w, r)
			return
		}
		// Check for valid session cookie
		if c, err := r.Cookie("arena_session"); err == nil {
			if s := h.Sessions.Validate(c.Value); s != nil {
				next(w, r)
				return
			}
		}
		// Also accept Authorization header (API token directly)
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			t := strings.TrimPrefix(auth, "Bearer ")
			if valid, _ := h.DB.ValidateToken(t); valid {
				next(w, r)
				return
			}
		}
		// Redirect to login
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}
