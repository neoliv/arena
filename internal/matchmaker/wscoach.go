package matchmaker

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

	"github.com/neoliv/arena/internal/coach"
	"nhooyr.io/websocket"
)

type coachConn struct {
	conn    *websocket.Conn
	coachID string
	send    chan []byte
	mu      sync.Mutex
}

func (c *coachConn) sendJSON(msg coach.MMMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	select {
	case c.send <- data:
	default:
		slog.Warn("coach send buffer full", "coach", c.coachID)
	}
}

func (m *MatchMaker) HandleCoachWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"arena.arsac.org", "localhost"},
	})
	if err != nil {
		slog.Error("mm coach ws accept", "err", err)
		return
	}

	cc := &coachConn{conn: conn, send: make(chan []byte, 32)}
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
	delete(m.coachHeartbeats, cc.coachID)
	delete(m.coachNextID, cc.coachID)
	delete(m.coachAckedID, cc.coachID)
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
		// Coach sends its last AckID on reconnect. MM continues from there.
		m.coachNextID[msg.CoachID] = msg.AckID + 1
		m.coachAckedID[msg.CoachID] = msg.AckID
		m.coachConnsMu.Unlock()

		slog.Info("coach registered via ws", "coach", msg.CoachID, "engines", len(engines),
			"next_id", msg.AckID+1)
		go func() {
			m.Wanted.Tick()
			m.tryLaunch()
		}()

	case "heartbeat":
		if cc.coachID == "" {
			return
		}
		m.coachConnsMu.Lock()
		m.coachHeartbeats[cc.coachID] = msg
		m.coachAckedID[cc.coachID] = msg.AckID
		m.coachConnsMu.Unlock()

		aiUpdates := make(map[string]int)
		for _, p := range msg.Players {
			aiUpdates[p.Name+":"+p.Version] = p.Instances
		}
		m.Wanted.Heartbeat(cc.coachID, 0, 0)
		m.Wanted.UpdateInstances(cc.coachID, aiUpdates)
		// Only trigger launch if the heartbeat confirms we have room.
		// During warmup, hbRunning=0 but inflight covers in-flight launches.
		hbRunning := 0
		for _, p := range msg.Players { hbRunning += p.Instances }
		next := m.coachNextID[cc.coachID]
		inflight := next - (msg.AckID + 1)
		if inflight < 0 { inflight = 0 }
		if hbRunning+inflight < 8 {
			m.tryLaunch()
		}

	case "engine_exited":
		slog.Info("engine exited", "session", msg.Session, "ok", msg.OK)
		m.Wanted.ReleaseSide(msg.Session)
		// Engine exiting always frees capacity — adjust hbRunning
		// optimistically so tryLaunch sees the freed slot immediately.
		m.adjustHeartbeatForExit(cc.coachID, msg.Engine)
		m.tryLaunch()

	case "engine_timeout":
		slog.Warn("engine timeout", "session", msg.Session)
		if m.ErrorStore != nil {
			m.ErrorStore.Report(msg.Session, "timeout")
		}
		m.Wanted.ReleaseSide(msg.Session)
		m.adjustHeartbeatForExit(cc.coachID, msg.Engine)
		m.tryLaunch()

	case "engine_crash":
		slog.Warn("engine crash", "session", msg.Session)
		if m.ErrorStore != nil {
			m.ErrorStore.Report(msg.Session, "crash")
		}
		m.Wanted.ReleaseSide(msg.Session)
		m.adjustHeartbeatForExit(cc.coachID, msg.Engine)
		m.tryLaunch()
	}
}

func (m *MatchMaker) tryLaunch() {
	pairs := m.Wanted.PendingPairs()
	if len(pairs) == 0 {
		return
	}
	var skipped, launched, noRoom, noCoach int
	for _, p := range pairs {
		if p.Status != "pending" {
			continue
		}
		m.coachConnsMu.Lock()
		bCoach, bOk := m.findCoachForEngine(p.BlackEngine, p.BlackCoachID)
		wCoach, wOk := m.findCoachForEngine(p.WhiteEngine, p.WhiteCoachID)
		bRoom := bOk && m.coachHasRoom(bCoach, p.BlackEngine)
		wRoom := wOk && m.coachHasRoom(wCoach, p.WhiteEngine)
		if !bOk || !wOk {
			noCoach++
			m.coachConnsMu.Unlock()
			continue
		}
		if !bRoom || !wRoom {
			noRoom++
			m.coachConnsMu.Unlock()
			continue
		}
		bID := m.coachNextID[bCoach]
		m.coachNextID[bCoach]++
		wID := m.coachNextID[wCoach]
		m.coachNextID[wCoach]++
		m.coachConnsMu.Unlock()

		p.Status = "assigned"
		p.BlackCoachID = bCoach
		p.WhiteCoachID = wCoach
		if p.SessionID == "" {
			p.SessionID = p.ID
		}
		m.sendToCoach(bCoach, coach.MMMessage{
			ID: bID, Type: "launch", Session: p.SessionID + "-b",
			Engine: p.BlackEngine, Side: "black",
			TimeControl: coach.TimeControl{Seconds: parseTCSeconds(p.TimeControl)},
			Opening:     p.OpeningLine,
		})
		m.sendToCoach(wCoach, coach.MMMessage{
			ID: wID, Type: "launch", Session: p.SessionID + "-w",
			Engine: p.WhiteEngine, Side: "white",
			TimeControl: coach.TimeControl{Seconds: parseTCSeconds(p.TimeControl)},
			Opening:     p.OpeningLine,
		})
		launched++
		slog.Info("launched pair", "pair", p.ID, "b", p.BlackEngine, "w", p.WhiteEngine)
	}
	if skipped > 0 || launched > 0 {
		slog.Info("tryLaunch done", "pending", len(pairs), "launched", launched,
			"skipped", skipped, "no_coach", noCoach, "no_room", noRoom)
	} else {
		skipped = len(pairs) - launched
	}
}

func (m *MatchMaker) coachHasRoom(coachID, engineKey string) bool {
	hb, _ := m.coachHeartbeats[coachID]
	acked := m.coachAckedID[coachID]
	next := m.coachNextID[coachID]

	hbRunning := 0
	for _, p := range hb.Players {
		hbRunning += p.Instances
	}
	inflight := next - (acked + 1)
	if inflight < 0 {
		inflight = 0
	}
	coresTotal := 8
	if c, ok := m.Wanted.GetCoach(coachID); ok {
		coresTotal = c.CoresTotal
	}
	// hbRunning + inflight must leave room for a full pair (2 engines).
	// The +1 gives one slot of buffer — an exit frees 1 slot but we can
	// still launch a pair (2 engines) without being blocked by inflight.
	return hbRunning+inflight+1 < coresTotal
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
	var tc struct{ Seconds float64 `json:"seconds"` }
	json.Unmarshal([]byte(tcJSON), &tc)
	if tc.Seconds <= 0 {
		return 30
	}
	return tc.Seconds
}

// adjustHeartbeatForExit decrements the instance count for an engine
// in the cached heartbeat so tryLaunch sees freed capacity immediately.
func (m *MatchMaker) adjustHeartbeatForExit(coachID, engineKey string) {
	m.coachConnsMu.Lock()
	defer m.coachConnsMu.Unlock()
	hb, ok := m.coachHeartbeats[coachID]
	if !ok || engineKey == "" {
		return
	}
	for i, p := range hb.Players {
		if p.Name+":"+p.Version == engineKey && p.Instances > 0 {
			hb.Players[i].Instances--
			m.coachHeartbeats[coachID] = hb
			return
		}
	}
}
