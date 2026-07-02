package matchmaker

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/neoliv/arena/internal/coach"
	"nhooyr.io/websocket"
)

// coachConn represents an active WebSocket connection from a coach.
type coachConn struct {
	conn    *websocket.Conn
	coachID string
	send    chan []byte
	mu      sync.Mutex
}

func (c *coachConn) sendJSON(msg coach.MMMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		slog.Warn("marshal mm msg", "err", err)
		return
	}
	select {
	case c.send <- data:
	default:
		slog.Warn("coach send buffer full", "coach", c.coachID)
	}
}

// HandleCoachWS handles a persistent coach WebSocket connection.
func (m *MatchMaker) HandleCoachWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"arena.arsac.org", "localhost"},
	})
	if err != nil {
		slog.Error("mm coach ws accept", "err", err)
		return
	}

	cc := &coachConn{
		conn: conn,
		send: make(chan []byte, 32),
	}

	go func() {
		for data := range cc.send {
			cc.mu.Lock()
			conn.Write(r.Context(), websocket.MessageText, data)
			cc.mu.Unlock()
		}
	}()

	ctx := r.Context()
	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			break
		}
		var cm coach.CoachMessage
		if err := json.Unmarshal(msg, &cm); err != nil {
			continue
		}
		m.handleCoachMessage(cc, cm)
	}

	slog.Info("coach ws disconnected", "coach", cc.coachID)
	close(cc.send)
	conn.Close(websocket.StatusNormalClosure, "done")

	m.coachConnsMu.Lock()
	delete(m.coachConns, cc.coachID)
	delete(m.coachAllocated, cc.coachID)
	// Clean up engine instances for this coach
	for k := range m.coachEngineInstances {
		if strings.HasPrefix(k, cc.coachID+":") {
			delete(m.coachEngineInstances, k)
		}
	}
	m.coachConnsMu.Unlock()

	if cc.coachID != "" {
		m.Wanted.RemoveCoach(cc.coachID)
		m.tryLaunch()
	}
}

func (m *MatchMaker) handleCoachMessage(cc *coachConn, msg coach.CoachMessage) {
	switch msg.Type {
	case "register":
		if msg.CoachID == "" {
			return
		}
		cc.coachID = msg.CoachID

		engines := make([]EngineEntry, len(msg.Engines))
		for i, e := range msg.Engines {
			engines[i] = EngineEntry{
				Name: e.Name, Version: e.Version, CoachID: msg.CoachID,
				Cores: e.Cores, MemoryMB: e.MemMB,
				MaxInstances: e.MaxInstances, Available: true,
			}
		}
		m.Wanted.RegisterCoach(msg.CoachID, msg.Cores, engines)

		m.coachConnsMu.Lock()
		m.coachConns[msg.CoachID] = cc
		m.coachAllocated[msg.CoachID] = 0
		m.coachConnsMu.Unlock()

		slog.Info("coach registered via ws", "coach", msg.CoachID, "engines", len(engines))
		go func() {
			m.Wanted.Tick()
			m.tryLaunch()
		}()

	case "heartbeat":
		if cc.coachID == "" {
			return
		}
		aiUpdates := make(map[string]int)
		for _, p := range msg.Players {
			aiUpdates[p.Name+":"+p.Version] = p.Instances
		}
		m.Wanted.Heartbeat(cc.coachID, 0, 0)
		m.Wanted.UpdateInstances(cc.coachID, aiUpdates)

	case "engine_exited":
		slog.Info("engine exited", "session", msg.Session, "ok", msg.OK)
		m.Wanted.ReleaseSide(msg.Session)
		m.coachConnsMu.Lock()
		if cc.coachID != "" {
			if m.coachAllocated[cc.coachID] > 0 { m.coachAllocated[cc.coachID]-- }
			// Decrement engine instance. We don't know which engine, so just
			// scan and decrement any positive count for this coach.
			for k := range m.coachEngineInstances {
				if strings.HasPrefix(k, cc.coachID+":") && m.coachEngineInstances[k] > 0 {
					m.coachEngineInstances[k]--
					break
				}
			}
		}
		m.coachConnsMu.Unlock()
		m.tryLaunch()

	case "engine_timeout":
		slog.Warn("engine timeout", "session", msg.Session)
		if m.ErrorStore != nil { m.ErrorStore.Report(msg.Session, "timeout") }
		m.Wanted.ReleaseSide(msg.Session)
		m.coachConnsMu.Lock()
		if cc.coachID != "" {
			if m.coachAllocated[cc.coachID] > 0 { m.coachAllocated[cc.coachID]-- }
			for k := range m.coachEngineInstances {
				if strings.HasPrefix(k, cc.coachID+":") && m.coachEngineInstances[k] > 0 {
					m.coachEngineInstances[k]--; break
				}
			}
		}
		m.coachConnsMu.Unlock()
		m.tryLaunch()

	case "engine_crash":
		slog.Warn("engine crash", "session", msg.Session)
		if m.ErrorStore != nil { m.ErrorStore.Report(msg.Session, "crash") }
		m.Wanted.ReleaseSide(msg.Session)
		m.coachConnsMu.Lock()
		if cc.coachID != "" {
			if m.coachAllocated[cc.coachID] > 0 { m.coachAllocated[cc.coachID]-- }
			for k := range m.coachEngineInstances {
				if strings.HasPrefix(k, cc.coachID+":") && m.coachEngineInstances[k] > 0 {
					m.coachEngineInstances[k]--; break
				}
			}
		}
		m.coachConnsMu.Unlock()
		m.tryLaunch()
	}
}

// tryLaunch scans pending pairs and assigns them to connected coaches,
// respecting per-coach capacity limits.
func (m *MatchMaker) tryLaunch() {
	pairs := m.Wanted.PendingPairs()
	for _, p := range pairs {
		if p.Status != "pending" {
			continue
		}
		m.coachConnsMu.Lock()
		bCoach, bOk := m.findCoachForEngine(p.BlackEngine, p.BlackCoachID)
		wCoach, wOk := m.findCoachForEngine(p.WhiteEngine, p.WhiteCoachID)

		// Capacity + MaxInstances check.
		bCores := engineCores(m.Wanted, bCoach, p.BlackEngine)
		wCores := engineCores(m.Wanted, wCoach, p.WhiteEngine)
		bKey := bCoach + ":" + p.BlackEngine
		wKey := wCoach + ":" + p.WhiteEngine
		bMax := maxInstances(m.Wanted, bCoach, p.BlackEngine)
		wMax := maxInstances(m.Wanted, wCoach, p.WhiteEngine)
		bInstOk := bOk && (bMax == 0 || m.coachEngineInstances[bKey] < bMax)
		wInstOk := wOk && (wMax == 0 || m.coachEngineInstances[wKey] < wMax)
		bRoom := bOk && (m.coachAllocated[bCoach]+bCores <= coachCores(m.Wanted, bCoach))
		wRoom := wOk && (m.coachAllocated[wCoach]+wCores <= coachCores(m.Wanted, wCoach))
		if !bInstOk || !wInstOk || !bRoom || !wRoom {
			m.coachConnsMu.Unlock()
			continue
		}

		// Reserve cores and instances before unlocking
		if bOk {
			m.coachAllocated[bCoach] += bCores
			m.coachEngineInstances[bKey]++
		}
		if wOk {
			m.coachAllocated[wCoach] += wCores
			m.coachEngineInstances[wKey]++
		}
		m.coachConnsMu.Unlock()

		p.Status = "assigned"
		p.BlackCoachID = bCoach
		p.WhiteCoachID = wCoach
		if p.SessionID == "" {
			p.SessionID = p.ID
		}
		m.sendToCoach(bCoach, coach.MMMessage{
			Type: "launch", Session: p.SessionID + "-b",
			Engine: p.BlackEngine, Side: "black",
			TimeControl: coach.TimeControl{Seconds: parseTCSeconds(p.TimeControl)},
			Opening:     p.OpeningLine,
		})
		m.sendToCoach(wCoach, coach.MMMessage{
			Type: "launch", Session: p.SessionID + "-w",
			Engine: p.WhiteEngine, Side: "white",
			TimeControl: coach.TimeControl{Seconds: parseTCSeconds(p.TimeControl)},
			Opening:     p.OpeningLine,
		})
		slog.Info("launched pair", "pair", p.ID, "b", p.BlackEngine, "w", p.WhiteEngine,
			"b_coach", bCoach, "w_coach", wCoach)
	}
}

// engineCores returns the cores needed for a given engine on a coach.
func engineCores(w *WantedList, coachID, engineKey string) int {
	if coachID == "" {
		return 1
	}
	c, ok := w.GetCoach(coachID)
	if !ok {
		return 1
	}
	e, ok := c.Engines[engineKey]
	if !ok || e.Cores <= 0 {
		return 1
	}
	return e.Cores
}

// maxInstances returns MaxInstances for an engine on a coach (default 1).
func maxInstances(w *WantedList, coachID, engineKey string) int {
	if coachID == "" { return 1 }
	c, ok := w.GetCoach(coachID)
	if !ok { return 1 }
	e, ok := c.Engines[engineKey]
	if !ok || e.MaxInstances <= 0 { return 1 }
	return e.MaxInstances
}

// coachCores returns the total cores available on a coach.
func coachCores(w *WantedList, coachID string) int {
	if coachID == "" {
		return 999
	}
	c, ok := w.GetCoach(coachID)
	if !ok || c.CoresTotal <= 0 {
		return 8 // default
	}
	return c.CoresTotal
}

func (m *MatchMaker) findCoachForEngine(engineKey, preferred string) (string, bool) {
	if preferred != "" {
		if _, ok := m.coachConns[preferred]; ok {
			if m.Wanted.CoachHasEngine(preferred, engineKey) {
				return preferred, true
			}
		}
	}
	for coachID := range m.coachConns {
		if m.Wanted.CoachHasEngine(coachID, engineKey) {
			return coachID, true
		}
	}
	return "", false
}

func (m *MatchMaker) sendToCoach(coachID string, msg coach.MMMessage) {
	m.coachConnsMu.Lock()
	cc, ok := m.coachConns[coachID]
	m.coachConnsMu.Unlock()
	if !ok {
		return
	}
	cc.sendJSON(msg)
}

func parseTCSeconds(tcJSON string) float64 {
	var tc struct {
		Seconds float64 `json:"seconds"`
	}
	json.Unmarshal([]byte(tcJSON), &tc)
	if tc.Seconds <= 0 {
		return 30
	}
	return tc.Seconds
}
