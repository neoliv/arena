// Package matchmaker — wanted list generation, engine registry, assignment.
package matchmaker

import (
	"log/slog"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/neoliv/arena/internal/db"
	"github.com/neoliv/arena/internal/elo"
)

// ── Engine registry (in-memory) ────────────────────────────────────────

// EngineEntry describes an engine registered by a coach.
type EngineEntry struct {
	Name              string `json:"name"`
	Version           string `json:"version"`
	CoachID           string `json:"coach_id"`
	Cores             int    `json:"cores"`
	MemoryMB          int    `json:"memory_mb"`
	MaxInstances      int    `json:"max_instances"`
	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

// CoachEntry holds a coach's registered engines and metadata.
type CoachEntry struct {
	ID         string
	CoresTotal int
	Engines    map[string]*EngineEntry // key: "name:version"
	LastSeen   time.Time
}

// ── Wanted pair ─────────────────────────────────────────────────────────

type wantedPair struct {
	ID               string
	BlackEngine      string // "name:version"
	WhiteEngine      string
	TimeControl      string // "30s"
	Priority         float64
	OpeningLine      string
	Status           string    // "pending", "playing"
	BlackConnected   bool      // coach dialed relay for this side
	WhiteConnected   bool      // coach dialed relay for this side
	BlackConnectedAt time.Time // when the coach dialed
	WhiteConnectedAt time.Time
	SessionID        string // base session ID for relay (both sides share this)
	gameTimeSec      float64 // parsed from TimeControl
}

// ── WantedList ──────────────────────────────────────────────────────────

type WantedList struct {
	mu       sync.RWMutex
	pairs    []*wantedPair
	coaches  map[string]*CoachEntry
	openings []string // from embedded book
	DB       *db.DB
	storeCh  chan<- GameResult

	// offers prevents re-offering the same pair+engine to the same coach
	// within a short window. Key: "coachID:engineKey" → offeredAt. TTL 5s.
	offers map[string]time.Time

	// declines tracks per-coach engine declines. Key: "coachID:engineKey".
	// TTL 20s. Populated when a connected side times out waiting for opponent.
	declines map[string]time.Time
}

func NewWantedList(database *db.DB, storeCh chan<- GameResult) *WantedList {
	return &WantedList{
		coaches:  make(map[string]*CoachEntry),
		offers:   make(map[string]time.Time),
		declines: make(map[string]time.Time),
		DB:       database,
		storeCh:  storeCh,
	}
}

// RegisterCoach adds/updates a coach and its engines.
func (w *WantedList) RegisterCoach(coachID string, coresTotal int, engines []EngineEntry) {
	w.mu.Lock()
	defer w.mu.Unlock()

	c, ok := w.coaches[coachID]
	if !ok {
		c = &CoachEntry{ID: coachID, CoresTotal: coresTotal, Engines: make(map[string]*EngineEntry)}
		w.coaches[coachID] = c
	}
	c.CoresTotal = coresTotal
	c.LastSeen = time.Now()

	// Re-registration implies a coach restart — clear all its declines.
	for k := range w.declines {
		if strings.HasPrefix(k, coachID+":") {
			delete(w.declines, k)
		}
	}

	for i := range engines {
		key := engines[i].Name + ":" + engines[i].Version
		if existing, ok := c.Engines[key]; ok {
			existing.Available = engines[i].Available
			existing.UnavailableReason = engines[i].UnavailableReason
			existing.MaxInstances = engines[i].MaxInstances
		} else {
			e := engines[i]
			e.CoachID = coachID
			c.Engines[key] = &e
		}
	}
}

// Heartbeat updates coach liveness without changing engine list.
func (w *WantedList) Heartbeat(coachID string, coresUsed int, memUsed int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if c, ok := w.coaches[coachID]; ok {
		c.LastSeen = time.Now()
	}
}

// RemoveCoach removes a coach and all its engines.
func (w *WantedList) RemoveCoach(coachID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.coaches, coachID)
}

// GetCoaches returns a snapshot for the dashboard.
func (w *WantedList) GetCoaches() []CoachEntry {
	w.mu.RLock()
	defer w.mu.RUnlock()
	var out []CoachEntry
	for _, c := range w.coaches {
		out = append(out, *c)
	}
	return out
}

// ── Tick: regenerate wanted list ────────────────────────────────────────

func (w *WantedList) Tick() {
	w.mu.Lock()
	defer w.mu.Unlock()

	// 1. Load Elo data from DB (read-only, fast)
	type engineElo struct {
		Name    string
		Version string
		TC      string
		Rating  float64
		Games   int
	}
	var elos []engineElo
	// Query engines directly — no JOIN on matches. A fresh DB has zero
	// matches, which would make this return zero rows → zero pairs → deadlock.
	rows, _ := w.DB.Query(`SELECT e.name, e.version,
		COALESCE((SELECT rating_after FROM elo_history WHERE engine_id=e.id ORDER BY created_at DESC LIMIT 1), 1500.0),
		COALESCE((SELECT COUNT(*) FROM elo_history WHERE engine_id=e.id), 0)
		FROM engines e ORDER BY e.name`)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var el engineElo
			rows.Scan(&el.Name, &el.Version, &el.Rating, &el.Games)
			el.TC = "30s" // default time control
			elos = append(elos, el)
		}
	}

	// 2. Build priority-sorted wanted pairs
	var newPairs []*wantedPair
	seen := make(map[string]bool)
	for _, a := range elos {
		for _, b := range elos {
			if a.Name == b.Name && a.Version == b.Version {
				continue
			}
			if a.TC != b.TC {
				continue
			}
			key := a.Name + ":" + a.Version + "|" + b.Name + ":" + b.Version + "|" + a.TC
			rkey := b.Name + ":" + b.Version + "|" + a.Name + ":" + a.Version + "|" + a.TC
			if seen[key] || seen[rkey] {
				continue
			}
			seen[key] = true

			// Check if both engines are available on any coach
			akey := a.Name + ":" + a.Version
			bkey := b.Name + ":" + b.Version
			if !w.engineAvailable(akey) || !w.engineAvailable(bkey) {
				continue
			}

			ciA := elo.ConfidenceInterval(a.Rating, a.Games)
			ciB := elo.ConfidenceInterval(b.Rating, b.Games)
			priority := math.Sqrt(ciA*ciA+ciB*ciB) * w.recencyFactor(akey, bkey)

			// Stable ID: hash of the engine pair so the same pairing keeps
			// the same ID across ticks. This prevents in-flight assignments
			// from being orphaned when Tick regenerates the pair list.
			newPairs = append(newPairs, &wantedPair{
				ID:          akey + "|" + bkey,
				BlackEngine: akey,
				WhiteEngine: bkey,
				TimeControl: a.TC,
				Priority:    priority,
				Status:      "pending",
			})
		}
	}

	// Sort by priority descending
	sort.Slice(newPairs, func(i, j int) bool { return newPairs[i].Priority > newPairs[j].Priority })

	// 3. Assign openings
	w.loadOpenings()
	for i, p := range newPairs {
		p.OpeningLine = w.openings[rand.Intn(len(w.openings))]
		_ = i
	}

	// Preserve playing/connected/assigned pairs from the old list.
	// Pairs with SessionID set have assignments out — must survive
	// Tick so ClaimSide can match the connections.
	oldPlaying := make(map[string]*wantedPair)
	for _, p := range w.pairs {
		if p.Status == "playing" || p.BlackConnected || p.WhiteConnected || p.SessionID != "" {
			oldPlaying[p.ID] = p
		}
	}
	for _, p := range newPairs {
		if old, ok := oldPlaying[p.ID]; ok {
			p.Status = old.Status
			p.BlackConnected = old.BlackConnected
			p.WhiteConnected = old.WhiteConnected
			p.BlackConnectedAt = old.BlackConnectedAt
			p.WhiteConnectedAt = old.WhiteConnectedAt
			p.SessionID = old.SessionID
		}
	}

	w.pairs = newPairs
	slog.Info("matchmaker tick", "pairs", len(newPairs))
}

func (w *WantedList) engineAvailable(key string) bool {
	for _, c := range w.coaches {
		if e, ok := c.Engines[key]; ok && e.Available {
			return true
		}
	}
	return false
}

func (w *WantedList) recencyFactor(a, b string) float64 { return 1.0 } // simplified

func (w *WantedList) loadOpenings() {
	if len(w.openings) > 0 {
		return
	}
	for _, line := range strings.Split(embeddedOpeningsBook, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			w.openings = append(w.openings, line)
		}
	}
	if len(w.openings) == 0 {
		w.openings = append(w.openings, "")
	}
}

// ── Assignment: poll + assign ───────────────────────────────────────────

// Assignment is a single side assignment returned to a coach.
type Assignment struct {
	SessionID   string `json:"session_id"`
	Engine      string `json:"engine"`
	Side        string `json:"side"`
	TimeControl string `json:"time_control"`
	Opening     string `json:"opening"`
}

// declineKey builds the lookup key for the decline/offer maps.
func declineKey(coachID, engineKey string) string { return coachID + ":" + engineKey }

// PollAssignments returns up to N pending pairs the coach can play.
// Does NOT mark anything as assigned — the relay connection is the claim.
// Uses an offers map (5s TTL) to avoid returning the same pair+engine
// to the same coach on every poll.
//
// Two-pass: first collect complete pairs (both sides free) to maximize
// concurrent games, then fill remaining slots with single-sided assignments.
// This prevents wasting core slots on lone connections when not all engine
// pairs are simultaneously available.
//
// Tracks per-engine instance counts against MaxInstances so the arena never
// offers more copies of an engine than the coach can launch.
func (w *WantedList) PollAssignments(coachID string, n int) []Assignment {
	w.mu.Lock()
	defer w.mu.Unlock()

	c, ok := w.coaches[coachID]
	if !ok {
		return nil
	}

	now := time.Now()
	type candidate struct {
		p          *wantedPair
		bKey, wKey string
		bothSides  bool
	}
	var complete []candidate // both sides available
	var single   []candidate // only one side available

	for _, p := range w.pairs {
		if p.Status != "pending" {
			continue
		}

		bKey := p.BlackEngine
		wKey := p.WhiteEngine

		hasB := c.Engines[bKey] != nil
		hasW := c.Engines[wKey] != nil
		bdk := declineKey(coachID, bKey)
		wdk := declineKey(coachID, wKey)
		_, bDecl := w.declines[bdk]
		_, wDecl := w.declines[wdk]
		tb, bOff := w.offers[bdk]
		tw, wOff := w.offers[wdk]
		bOk := hasB && !p.BlackConnected && !bDecl && (!bOff || now.Sub(tb) >= 2*time.Second)
		wOk := hasW && !p.WhiteConnected && !wDecl && (!wOff || now.Sub(tw) >= 2*time.Second)

		if bOk && wOk {
			complete = append(complete, candidate{p: p, bKey: bKey, wKey: wKey, bothSides: true})
		} else if bOk {
			single = append(single, candidate{p: p, bKey: bKey, wKey: wKey, bothSides: false})
		} else if wOk {
			single = append(single, candidate{p: p, bKey: bKey, wKey: wKey, bothSides: false})
		}
	}

	// maxInst returns MaxInstances for an engine, defaulting to 1.
	maxInst := func(key string) int {
		if e, ok := c.Engines[key]; ok && e.MaxInstances > 0 {
			return e.MaxInstances
		}
		return 1
	}

	used := make(map[string]int) // per-engine output count
	var out []Assignment

	// Pass 1: complete pairs first (each uses 2 slots).
	for _, cand := range complete {
		if len(out)+1 >= n {
			break
		}
		bKey, wKey := cand.bKey, cand.wKey
		if used[bKey] >= maxInst(bKey) || used[wKey] >= maxInst(wKey) {
			continue // engine already at max instances in this poll batch
		}
		p := cand.p
		used[bKey]++
		used[wKey]++
		bdk := declineKey(coachID, bKey)
		wdk := declineKey(coachID, wKey)
		w.offers[bdk] = now
		w.offers[wdk] = now
		if p.SessionID == "" { p.SessionID = p.ID }
		out = append(out,
			Assignment{SessionID: p.SessionID + "-b", Engine: bKey, Side: "black", TimeControl: p.TimeControl, Opening: p.OpeningLine},
			Assignment{SessionID: p.SessionID + "-w", Engine: wKey, Side: "white", TimeControl: p.TimeControl, Opening: p.OpeningLine},
		)
	}
	// Pass 2: single-sided assignments to fill remaining slots.
	for _, cand := range single {
		if len(out) >= n {
			break
		}
		p, bKey, wKey := cand.p, cand.bKey, cand.wKey
		hasB := c.Engines[bKey] != nil
		bdk := declineKey(coachID, bKey)
		wdk := declineKey(coachID, wKey)
		_, bDecl := w.declines[bdk]
		_, wDecl := w.declines[wdk]
		tb, bOff := w.offers[bdk]
		tw, wOff := w.offers[wdk]
		bOk := hasB && !p.BlackConnected && !bDecl && (!bOff || now.Sub(tb) >= 2*time.Second)
		wOk := c.Engines[wKey] != nil && !p.WhiteConnected && !wDecl && (!wOff || now.Sub(tw) >= 2*time.Second)
		if bOk && !wOk && used[bKey] < maxInst(bKey) {
			used[bKey]++
			w.offers[bdk] = now
			if p.SessionID == "" { p.SessionID = p.ID }
			out = append(out, Assignment{SessionID: p.SessionID + "-b", Engine: bKey, Side: "black", TimeControl: p.TimeControl, Opening: p.OpeningLine})
		} else if wOk && !bOk && used[wKey] < maxInst(wKey) {
			used[wKey]++
			w.offers[wdk] = now
			if p.SessionID == "" { p.SessionID = p.ID }
			out = append(out, Assignment{SessionID: p.SessionID + "-w", Engine: wKey, Side: "white", TimeControl: p.TimeControl, Opening: p.OpeningLine})
		}
	}
		// Diagnostic: when returning empty, log breakdown.
	if len(out) == 0 && len(w.pairs) > 0 {
		var noEngine, connected, declined, offered, atMax int
		now2 := time.Now()
		for _, p := range w.pairs {
			if p.Status != "pending" { continue }
			bKey, wKey := p.BlackEngine, p.WhiteEngine
			if c.Engines[bKey] == nil && c.Engines[wKey] == nil { noEngine++; continue }
			if p.BlackConnected || p.WhiteConnected { connected++; continue }
			bdk, wdk := declineKey(coachID, bKey), declineKey(coachID, wKey)
			_, bDecl := w.declines[bdk]
			_, wDecl := w.declines[wdk]
			if bDecl || wDecl { declined++; continue }
			tb, bOff := w.offers[bdk]
			tw, wOff := w.offers[wdk]
			bStale := !bOff || now2.Sub(tb) >= 2*time.Second
			wStale := !wOff || now2.Sub(tw) >= 2*time.Second
			if !bStale || !wStale { offered++; continue }
			if used[bKey] >= maxInst(bKey) || used[wKey] >= maxInst(wKey) { atMax++; continue }
		}
		slog.Warn("PollAssignments empty breakdown", "coach", coachID, "pairs", len(w.pairs),
			"no_engine", noEngine, "connected", connected, "declined", declined, "offered", offered, "at_max", atMax)
	}
	return out
}

// ClaimSide is called via the relay OnConnect callback when a coach dials
// a relay session. It marks the side as connected. Returns (true, pair) if
// both sides are now connected and the match should start.
func (w *WantedList) ClaimSide(sessionID string) (startMatch bool, pair *wantedPair) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Parse pair ID and side from session: "g1-b" → pair "g1", side "b"
	idx := strings.LastIndex(sessionID, "-")
	if idx < 0 {
		return false, nil
	}
	pairID := sessionID[:idx]
	side := sessionID[idx+1:]

	for _, p := range w.pairs {
		if p.SessionID != pairID && p.ID != pairID {
			continue
		}
		if p.Status != "pending" {
			return false, nil
		}
		now := time.Now()
		if side == "b" && !p.BlackConnected {
			p.BlackConnected = true
			p.BlackConnectedAt = now
		} else if side == "w" && !p.WhiteConnected {
			p.WhiteConnected = true
			p.WhiteConnectedAt = now
		} else {
			return false, nil // already connected or unknown side
		}
		if p.BlackConnected && p.WhiteConnected {
			p.Status = "playing"
			return true, p
		}
		return false, nil
	}
	return false, nil
}

// ReleaseSide resets a side's connection (called on timeout waiting for opponent).
func (w *WantedList) ReleaseSide(sessionID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	idx := strings.LastIndex(sessionID, "-")
	if idx < 0 {
		return
	}
	pairID := sessionID[:idx]
	side := sessionID[idx+1:]
	for _, p := range w.pairs {
		if (p.SessionID == pairID || p.ID == pairID) && p.Status == "pending" {
			if side == "b" {
				p.BlackConnected = false
			} else {
				p.WhiteConnected = false
			}
			return
		}
	}
}

// ResetPair resets a pair to pending so it can be reassigned after a failure.
func (w *WantedList) ResetPair(pairID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range w.pairs {
		if p.ID == pairID {
			p.Status = "pending"
			p.BlackConnected = false
			p.WhiteConnected = false
			return
		}
	}
}

// ReapStale expires old offers and declines.
func (w *WantedList) ReapStale(_ time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()

	for k, t := range w.offers {
		if now.Sub(t) > 5*time.Second {
			delete(w.offers, k)
		}
	}
	for k, t := range w.declines {
		if now.Sub(t) > 20*time.Second {
			delete(w.declines, k)
		}
	}
}
