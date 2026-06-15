package db

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

type CoachRow struct {
	ID         int
	CoachID    string
	Label      string
	Version    string
	CoresTotal int
	MemoryMB   int
	LastSeen   string
}

type CoachAIRow struct {
	ID                  int
	CoachID             int
	EngineName          string
	EngineVersion       string
	CoresPerInstance    int
	MemoryMBPerInstance int
	MaxInstances        int
	InstancesRunning    int
	RunCmd              string
	BuildCmd            string
	Created             string
	ChangelogShort      string
	ChangelogFull       string
}

type AssignmentRow struct {
	ID            int
	Engine1ID     int
	Engine2ID     int
	Coach1AIID    int
	Coach2AIID    int
	TimeControl   string
	NumGames      int
	Session1ID    string
	Session2ID    string
	Status        string
	DeclineReason string
	RetryCount    int
	RetryAfter    string
}

func (db *DB) UpsertCoach(coachID, version, label string, cores, memoryMB int) (int, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(`INSERT INTO coaches (coach_id, version, label, cores_total, memory_mb_total, last_seen, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(coach_id) DO UPDATE SET
			version=excluded.version, label=excluded.label,
			cores_total=excluded.cores_total, memory_mb_total=excluded.memory_mb_total,
			last_seen=excluded.last_seen, updated_at=excluded.updated_at`,
		coachID, version, label, cores, memoryMB, now, now)
	if err != nil {
		return 0, fmt.Errorf("upsert coach: %w", err)
	}
	var id int
	err = db.QueryRow("SELECT id FROM coaches WHERE coach_id=?", coachID).Scan(&id)
	return id, err
}

func (db *DB) UpsertCoachAI(coachID int, name, version, created, changelogShort, changelogFull string, cores, memMB, maxInst int, runCmd, buildCmd, engineID, engineManifest string) (int, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	if created == "" { created = now }
	_, err := db.Exec(`INSERT INTO coach_ais (coach_id, engine_name, engine_version, cores_per_instance, memory_mb_per_instance, max_instances, run_cmd, build_cmd, is_available, created, changelog_short, changelog_full, engine_id, engine_manifest, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(coach_id, engine_name, engine_version) DO UPDATE SET
			cores_per_instance=excluded.cores_per_instance,
			memory_mb_per_instance=excluded.memory_mb_per_instance,
			max_instances=excluded.max_instances,
			run_cmd=excluded.run_cmd,
			build_cmd=excluded.build_cmd,
			created=excluded.created,
			changelog_short=excluded.changelog_short,
			changelog_full=excluded.changelog_full,
			is_available=1,
			engine_id=excluded.engine_id,
			engine_manifest=excluded.engine_manifest,
			updated_at=excluded.updated_at`, coachID, name, version, cores, memMB, maxInst, runCmd, buildCmd, created, changelogShort, changelogFull, engineID, engineManifest, now)
	if err != nil {
		return 0, fmt.Errorf("upsert coach ai: %w", err)
	}
	var id int
	err = db.QueryRow("SELECT id FROM coach_ais WHERE coach_id=? AND engine_name=? AND engine_version=?", coachID, name, version).Scan(&id)
	return id, err
}

func (db *DB) UpdateCoachHeartbeat(coachID int, aiUpdates map[string]int) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec("UPDATE coaches SET last_seen=?, updated_at=? WHERE id=?", now, now, coachID)
	if err != nil { return err }
	for key, running := range aiUpdates {
		parts := strings.SplitN(key, ":", 2)
		if len(parts) != 2 { continue }
		_, err := db.Exec("UPDATE coach_ais SET instances_running=?, is_available=1, updated_at=? WHERE coach_id=? AND engine_name=? AND engine_version=?",
			running, now, coachID, parts[0], parts[1])
		if err != nil { slog.Warn("update ai instances", "key", key, "err", err) }
	}
	return nil
}

func (db *DB) GetOnlineCoaches(withinSec int) ([]CoachRow, error) {
	rows, err := db.Query("SELECT id, coach_id, COALESCE(label,''), COALESCE(version,''), cores_total, memory_mb_total, last_seen FROM coaches WHERE last_seen >= datetime('now',?||' seconds')", fmt.Sprintf("-%d", withinSec))
	if err != nil { return nil, err }
	defer rows.Close()
	var out []CoachRow
	for rows.Next() {
		var c CoachRow
		if err := rows.Scan(&c.ID, &c.CoachID, &c.Label, &c.Version, &c.CoresTotal, &c.MemoryMB, &c.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (db *DB) GetAvailableAIs(coachID int) ([]CoachAIRow, error) {
	rows, err := db.Query(`SELECT ca.id, ca.coach_id, ca.engine_name, ca.engine_version,
		ca.cores_per_instance, ca.memory_mb_per_instance, ca.max_instances, ca.instances_running,
		COALESCE(ca.run_cmd,''), COALESCE(ca.build_cmd,''),
		COALESCE(ca.created,''), COALESCE(ca.changelog_short,''), COALESCE(ca.changelog_full,'')
		FROM coach_ais ca JOIN coaches c ON c.id=ca.coach_id
		WHERE ca.is_available=1 AND ca.coach_id=? AND ca.instances_running < ca.max_instances
		ORDER BY ca.id`, coachID)
	if err != nil { return nil, err }
	defer rows.Close()
	var out []CoachAIRow
	for rows.Next() {
		var a CoachAIRow
		if err := rows.Scan(&a.ID, &a.CoachID, &a.EngineName, &a.EngineVersion,
			&a.CoresPerInstance, &a.MemoryMBPerInstance, &a.MaxInstances, &a.InstancesRunning,
			&a.RunCmd, &a.BuildCmd, &a.Created, &a.ChangelogShort, &a.ChangelogFull); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func (db *DB) GetPendingAssignments(coachAIID int) ([]AssignmentRow, error) {
	rows, err := db.Query(`SELECT id, engine1_id, engine2_id, coach1_ai_id, coach2_ai_id,
		COALESCE(time_control,'{}'), num_games, COALESCE(session1_id,''), COALESCE(session2_id,''),
		status, COALESCE(decline_reason,''), retry_count, COALESCE(retry_after,'')
		FROM match_assignments
		WHERE (coach1_ai_id=? OR coach2_ai_id=?) AND status IN ('pending','assigned')
		ORDER BY id LIMIT 5`, coachAIID, coachAIID)
	if err != nil { return nil, err }
	defer rows.Close()
	var out []AssignmentRow
	for rows.Next() {
		var a AssignmentRow
		if err := rows.Scan(&a.ID, &a.Engine1ID, &a.Engine2ID, &a.Coach1AIID, &a.Coach2AIID,
			&a.TimeControl, &a.NumGames, &a.Session1ID, &a.Session2ID,
			&a.Status, &a.DeclineReason, &a.RetryCount, &a.RetryAfter); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func (db *DB) CreateAssignment(e1ID, e2ID, c1AIID, c2AIID int, timeControl string, numGames int, session1, session2 string) (int, error) {
	res, err := db.Exec(`INSERT INTO match_assignments (engine1_id, engine2_id, coach1_ai_id, coach2_ai_id, time_control, num_games, session1_id, session2_id, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending')`, e1ID, e2ID, c1AIID, c2AIID, timeControl, numGames, session1, session2)
	if err != nil { return 0, fmt.Errorf("create assignment: %w", err) }
	id, _ := res.LastInsertId()
	return int(id), nil
}

func (db *DB) UpdateAssignmentStatus(id int, status, reason string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	extraCols := ""
	extraVals := ""
	if status == "in_progress" {
		extraCols = ", in_progress_at=?"
		extraVals = ", " + now
	}
	_, err := db.Exec("UPDATE match_assignments SET status=?, decline_reason=?"+extraCols+" WHERE id=?",
		status, reason+extraVals, id)
	return err
}

func (db *DB) RetryExpiredAssignments() error {
	_, err := db.Exec(`UPDATE match_assignments SET status='pending', retry_count=retry_count+1
		WHERE status='retry' AND retry_count < 5 AND retry_after <= datetime('now')`)
	if err != nil { return err }
	_, err = db.Exec(`UPDATE match_assignments SET status='failed', decline_reason='max retries exceeded'
		WHERE status='retry' AND retry_count >= 5`)
	return err
}

func (db *DB) FailStaleAssignments() error {
	_, err := db.Exec(`UPDATE match_assignments SET status='failed', decline_reason='coach offline'
		WHERE status IN ('assigned','accepted','in_progress')
		AND (coach1_ai_id IN (SELECT ca.id FROM coach_ais ca JOIN coaches c ON c.id=ca.coach_id WHERE c.last_seen < datetime('now','-90 seconds'))
		 OR coach2_ai_id IN (SELECT ca.id FROM coach_ais ca JOIN coaches c ON c.id=ca.coach_id WHERE c.last_seen < datetime('now','-90 seconds')))`)
	return err
}

func (db *DB) GetEngineID(name, version string) (int, error) {
	var id int
	err := db.QueryRow("SELECT id FROM engines WHERE name=? AND version=?", name, version).Scan(&id)
	return id, err
}

func (db *DB) GetEngineIDByName(name string) (int, error) {
	var id int
	err := db.QueryRow("SELECT id FROM engines WHERE name=? ORDER BY created_at DESC LIMIT 1", name).Scan(&id)
	return id, err
}

func (db *DB) GetAssignmentBySession(sessionID string) (*AssignmentRow, error) {
	var a AssignmentRow
	err := db.QueryRow(`SELECT id, engine1_id, engine2_id, coach1_ai_id, coach2_ai_id,
		COALESCE(time_control,'{}'), num_games, COALESCE(session1_id,''), COALESCE(session2_id,''),
		status, COALESCE(decline_reason,''), retry_count, COALESCE(retry_after,'')
		FROM match_assignments WHERE session1_id=? OR session2_id=?`, sessionID, sessionID).Scan(
		&a.ID, &a.Engine1ID, &a.Engine2ID, &a.Coach1AIID, &a.Coach2AIID,
		&a.TimeControl, &a.NumGames, &a.Session1ID, &a.Session2ID,
		&a.Status, &a.DeclineReason, &a.RetryCount, &a.RetryAfter)
	if err == sql.ErrNoRows { return nil, nil }
	if err != nil { return nil, err }
	return &a, nil
}
