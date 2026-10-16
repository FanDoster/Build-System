package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// NotificationQueueLimit bounds disk use even if the relay is unavailable for
// weeks. Old completed entries are removed first, then the oldest queued event.
// Build observations survive eviction, so an expired event is never replayed.
const NotificationQueueLimit = 1000

func (d *DB) migrateNotifications() error {
	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`
 INSERT OR IGNORE INTO app_settings(key,value) VALUES ('notification_instance_id',lower(hex(randomblob(16))));
 CREATE TABLE IF NOT EXISTS notification_build_seen (
   build_id INTEGER PRIMARY KEY REFERENCES builds(id) ON DELETE CASCADE,
   status TEXT NOT NULL,
   terminal INTEGER NOT NULL DEFAULT 0
 );
 CREATE TABLE IF NOT EXISTS notification_agent_seen (
   project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
   name TEXT NOT NULL,
   fingerprint TEXT NOT NULL,
   revision INTEGER NOT NULL DEFAULT 1,
   PRIMARY KEY(project_id, name)
 );
 CREATE TABLE IF NOT EXISTS agent_notification_preferences (
 name TEXT PRIMARY KEY,
 enabled INTEGER NOT NULL DEFAULT 1,
 availability INTEGER NOT NULL DEFAULT 1,
 health INTEGER NOT NULL DEFAULT 1,
 builds INTEGER NOT NULL DEFAULT 1
 );
 CREATE TABLE IF NOT EXISTS notification_agent_kind_seen (
 project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 name TEXT NOT NULL,
 kind TEXT NOT NULL,
 fingerprint TEXT NOT NULL,
 revision INTEGER NOT NULL DEFAULT 1,
 PRIMARY KEY(project_id,name,kind)
 );
 CREATE TABLE IF NOT EXISTS notification_outbox (
   id INTEGER PRIMARY KEY AUTOINCREMENT,
   event_key TEXT NOT NULL UNIQUE,
   project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
   build_id INTEGER REFERENCES builds(id) ON DELETE CASCADE,
   event_json TEXT NOT NULL DEFAULT '',
   payload TEXT NOT NULL DEFAULT '',
   agent_name TEXT NOT NULL DEFAULT '',
   event_kind TEXT NOT NULL DEFAULT '',
   state TEXT NOT NULL DEFAULT 'pending',
   relay_id TEXT NOT NULL DEFAULT '',
   attempts INTEGER NOT NULL DEFAULT 0,
   created_at INTEGER NOT NULL,
   next_attempt_at INTEGER NOT NULL
 );
 CREATE INDEX IF NOT EXISTS idx_notification_due ON notification_outbox(state, next_attempt_at, id);
 CREATE TRIGGER IF NOT EXISTS notification_project_mute AFTER UPDATE OF telegram_notifications ON projects
 WHEN NEW.telegram_notifications=0 AND OLD.telegram_notifications<>0 BEGIN
   UPDATE notification_outbox SET state='muted' WHERE project_id=NEW.id AND state='pending';
 END;
 `)
	if err != nil {
		return err
	}
	// ALTER inside this transaction: a restart sees either the complete new
	// capture contract or the old one, never a trigger referring to missing fields.
	for _, column := range []string{"agent_name", "event_kind"} {
		rows, e := tx.Query(`SELECT name FROM pragma_table_info('notification_outbox')`)
		if e != nil {
			return e
		}
		found := false
		for rows.Next() {
			var name string
			if e = rows.Scan(&name); e != nil {
				rows.Close()
				return e
			}
			if name == column {
				found = true
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		if !found {
			if _, e = tx.Exec(`ALTER TABLE notification_outbox ADD COLUMN ` + column + ` TEXT NOT NULL DEFAULT ''`); e != nil {
				return e
			}
		}
	}
	if _, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_notification_agent_kind ON notification_outbox(agent_name,event_kind,state)`); err != nil {
		return err
	}
	if err = backfillNotificationAgents(tx); err != nil {
		return err
	}
	var seeded int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM app_settings WHERE key='notification_build_seed_v1'`).Scan(&seeded); err != nil {
		return err
	}
	if seeded == 0 {
		// This is the only historical scan. No log bytes are selected. Existing
		// terminal builds are observed, never notified; existing active jobs still
		// produce their future outcome, including startup recovery failures.
		if _, err = tx.Exec(`INSERT OR IGNORE INTO notification_build_seen(build_id,status,terminal)
     SELECT id,status,status IN ('success','failed','canceled') FROM builds;
     INSERT INTO app_settings(key,value) VALUES ('notification_build_seed_v1','1');`); err != nil {
			return err
		}
	}
	// A database trigger, not a callback in each runner/API/janitor path, makes
	// status observation atomic with every terminal write, including new paths.
	for _, op := range []string{"INSERT", "UPDATE OF status"} {
		suffix := "insert"
		if op != "INSERT" {
			suffix = "update"
		}
		// IF NOT EXISTS would silently retain the old capture logic on upgrade.
		// Refresh both triggers atomically; no history is rewritten or re-alerted.
		if _, err = tx.Exec(`DROP TRIGGER IF EXISTS notification_build_` + suffix); err != nil {
			return err
		}
		_, err = tx.Exec(`CREATE TRIGGER notification_build_` + suffix + `
   AFTER ` + op + ` ON builds BEGIN
     INSERT OR IGNORE INTO notification_outbox(event_key,project_id,build_id,event_json,agent_name,event_kind,created_at,next_attempt_at)
       SELECT (SELECT value FROM app_settings WHERE key='notification_instance_id') || ':build:' || NEW.id, NEW.project_id, NEW.id,
         json_object('id',NEW.id,'number',NEW.project_build_number,'project_id',NEW.project_id,
           'project',p.name,'branch',p.branch,
           'status',NEW.status,'commit',NEW.commit_sha,
           'executor',p.executor,'agent',NEW.agent,
           'started',NEW.started_at,'finished',NEW.finished_at),
         NEW.agent,'build',unixepoch(),unixepoch()
       FROM projects p WHERE p.id=NEW.project_id AND p.telegram_notifications=1
         AND NOT EXISTS (SELECT 1 FROM agent_notification_preferences a WHERE NEW.agent<>'' AND a.name=NEW.agent AND (a.enabled=0 OR a.builds=0))
         AND COALESCE((SELECT value FROM app_settings WHERE key='telegram_capture_enabled'),'1')='1'
         AND NEW.status IN ('success','failed','canceled')
         AND NOT EXISTS (SELECT 1 FROM notification_build_seen WHERE build_id=NEW.id AND terminal=1)
         AND length(CAST(json_object('id',NEW.id,'number',NEW.project_build_number,'project_id',NEW.project_id,
           'project',p.name,'branch',p.branch,'status',NEW.status,'commit',NEW.commit_sha,
           'executor',p.executor,'agent',NEW.agent,'started',NEW.started_at,'finished',NEW.finished_at) AS BLOB))<=16384;
     INSERT INTO notification_build_seen(build_id,status,terminal)
       VALUES(NEW.id,NEW.status,NEW.status IN ('success','failed','canceled'))
       ON CONFLICT(build_id) DO UPDATE SET status=excluded.status,
         terminal=MAX(notification_build_seen.terminal,excluded.terminal);
     DELETE FROM notification_outbox WHERE id IN (
       SELECT id FROM notification_outbox ORDER BY
         CASE WHEN state IN ('pending','submitted') THEN 1 ELSE 0 END,id
         LIMIT MAX(0,(SELECT COUNT(*) FROM notification_outbox)-1000)
     );
   END;`)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Notification is a durable relay submission. Payload is set exactly once,
// before the first request, and EventKey is unchanged through all retries.
type Notification struct {
	ID, ProjectID                                int64
	EventKey, EventJSON, Payload, State, RelayID string
	AgentName, EventKind                         string
	Attempts                                     int
	CreatedAt, NextAttemptAt                     int64
}

func (d *DB) DueNotifications(now time.Time, limit int) ([]Notification, error) {
	if limit < 1 || limit > 25 {
		limit = 25
	}
	rows, err := d.conn.Query(`SELECT id,project_id,event_key,event_json,payload,state,relay_id,attempts,created_at,next_attempt_at,agent_name,event_kind
   FROM notification_outbox WHERE state IN ('pending','submitted') AND next_attempt_at<=? ORDER BY next_attempt_at,id LIMIT ?`, now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Notification
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.ProjectID, &n.EventKey, &n.EventJSON, &n.Payload, &n.State, &n.RelayID, &n.Attempts, &n.CreatedAt, &n.NextAttemptAt, &n.AgentName, &n.EventKind); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (d *DB) SetNotificationPayload(id int64, payload string) error {
	if len(payload) > 16<<10 || !json.Valid([]byte(payload)) {
		return fmt.Errorf("invalid notification payload")
	}
	_, err := d.conn.Exec(`UPDATE notification_outbox SET payload=? WHERE id=? AND payload=''`, payload, id)
	return err
}

// UpdateNotification records acceptance separately from delivery. No error
// response bodies are stored: they may contain credentials or destination IDs.
func (d *DB) UpdateNotification(id int64, state, relayID string, attempts int, next time.Time) error {
	switch state {
	case "pending", "submitted", "delivered", "failed", "muted", "expired", "suppressed":
	default:
		return fmt.Errorf("invalid notification state")
	}
	if len(relayID) > 128 {
		return fmt.Errorf("invalid relay id")
	}
	_, err := d.conn.Exec(`UPDATE notification_outbox SET state=?,relay_id=?,attempts=?,next_attempt_at=? WHERE id=? AND state IN ('pending','submitted')`, state, relayID, attempts, next.Unix(), id)
	return err
}

func (d *DB) PruneNotifications(now time.Time) error {
	// Integer Unix times have one representation; unlike builds' mixed DATETIME
	// values these can safely be compared in SQLite.
	_, err := d.conn.Exec(`UPDATE notification_outbox SET state='expired' WHERE state='pending' AND created_at<?;
 DELETE FROM notification_outbox WHERE created_at<?;
   DELETE FROM notification_outbox WHERE id IN (SELECT id FROM notification_outbox
     ORDER BY CASE WHEN state IN ('pending','submitted') THEN 1 ELSE 0 END,id
     LIMIT MAX(0,(SELECT COUNT(*) FROM notification_outbox)-1000));`, now.Add(-24*time.Hour).Unix(), now.Add(-7*24*time.Hour).Unix())
	return err
}

// ObserveNotificationAgent atomically advances a semantic observation and
// enqueues only transitions. Initial healthy state and muted changes are still
// remembered, so restart/unmute never replays an already observed condition.
func (d *DB) ObserveNotificationAgent(projectID int64, name, fingerprint, payload string, unhealthy bool, now time.Time) error {
	return d.observeNotificationAgent(projectID, name, "agent", fingerprint, payload, unhealthy, now)
}

// ObserveNotificationAgentKind splits availability from health, so neither
// category can trigger or expose the other's details. Baselines always advance.
func (d *DB) ObserveNotificationAgentKind(projectID int64, name, kind, fingerprint, payload string, unhealthy bool, now time.Time) error {
	if kind != "availability" && kind != "health" {
		return fmt.Errorf("invalid agent event kind")
	}
	return d.observeNotificationAgent(projectID, name, kind, fingerprint, payload, unhealthy, now)
}

func (d *DB) observeNotificationAgent(projectID int64, name, kind, fingerprint, payload string, unhealthy bool, now time.Time) error {
	if len(name) > 256 || len(fingerprint) > 16<<10 || len(payload) > 16<<10 || !json.Valid([]byte(payload)) {
		return fmt.Errorf("invalid agent notification")
	}
	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var previous string
	var revision int
	query := `SELECT fingerprint,revision FROM notification_agent_kind_seen WHERE project_id=? AND name=? AND kind=?`
	args := []any{projectID, name, kind}
	if kind == "agent" {
		query = `SELECT fingerprint,revision FROM notification_agent_seen WHERE project_id=? AND name=?`
		args = args[:2]
	}
	err = tx.QueryRow(query, args...).Scan(&previous, &revision)
	first := err == sql.ErrNoRows
	if err != nil && !first {
		return err
	}
	if !first && previous == fingerprint {
		return nil
	}
	// The legacy digest cannot be split safely. Seed each new category silently
	// when an old baseline exists, rather than repeating an old unhealthy alert.
	legacyBaseline := false
	if first && kind != "agent" {
		if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM notification_agent_seen WHERE project_id=? AND name=?)`, projectID, name).Scan(&legacyBaseline); err != nil {
			return err
		}
	}
	revision++
	if kind == "agent" {
		_, err = tx.Exec(`INSERT INTO notification_agent_seen(project_id,name,fingerprint,revision) VALUES(?,?,?,?) ON CONFLICT(project_id,name) DO UPDATE SET fingerprint=excluded.fingerprint,revision=excluded.revision`, projectID, name, fingerprint, revision)
	} else {
		_, err = tx.Exec(`INSERT INTO notification_agent_kind_seen(project_id,name,kind,fingerprint,revision) VALUES(?,?,?,?,?) ON CONFLICT(project_id,name,kind) DO UPDATE SET fingerprint=excluded.fingerprint,revision=excluded.revision`, projectID, name, kind, fingerprint, revision)
	}
	if err != nil {
		return err
	}
	if (!first || unhealthy) && !legacyBaseline {
		nameHash := sha256.Sum256([]byte(name))
		key := fmt.Sprintf("agent:%d:%x:%d", projectID, nameHash[:16], revision)
		if kind != "agent" {
			key = fmt.Sprintf("agent-kind:%d:%x:%s:%d", projectID, nameHash[:16], kind, revision)
		}
		_, err = tx.Exec(`INSERT INTO notification_outbox(event_key,project_id,payload,agent_name,event_kind,created_at,next_attempt_at)
 SELECT (SELECT value FROM app_settings WHERE key='notification_instance_id') || ':' || ?,id,?,?,?,?,? FROM projects
 WHERE id=? AND telegram_notifications=1 AND COALESCE((SELECT value FROM app_settings WHERE key='telegram_capture_enabled'),'1')='1'
 AND NOT EXISTS (SELECT 1 FROM agent_notification_preferences a WHERE a.name=? AND (a.enabled=0 OR (?='availability' AND a.availability=0) OR (?='health' AND a.health=0) OR (?='agent' AND (a.availability=0 OR a.health=0))))`, key, payload, name, kind, now.Unix(), now.Unix(), projectID, name, kind, kind, kind)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM notification_outbox WHERE id IN (SELECT id FROM notification_outbox ORDER BY CASE WHEN state IN ('pending','submitted') THEN 1 ELSE 0 END,id LIMIT MAX(0,(SELECT COUNT(*) FROM notification_outbox)-1000))`)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetTelegramCapture distinguishes explicit disablement from transport outage.
// Disabled configuration observes builds but cannot accumulate a setup backlog.
// Already submitted receipts are preserved: acceptance cannot be recalled.
func (d *DB) SetTelegramCapture(enabled bool) error {
	value := "0"
	if enabled {
		value = "1"
	}
	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO app_settings(key,value) VALUES('telegram_capture_enabled',?)
   ON CONFLICT(key) DO UPDATE SET value=excluded.value WHERE value<>excluded.value`, value)
	if err != nil {
		return err
	}
	if !enabled {
		if _, err = tx.Exec(`UPDATE notification_outbox SET state='suppressed' WHERE state='pending'`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) GetNotification(id int64) (*Notification, error) {
	var n Notification
	err := d.conn.QueryRow(`SELECT id,project_id,event_key,event_json,payload,state,relay_id,attempts,created_at,next_attempt_at,agent_name,event_kind
   FROM notification_outbox WHERE id=?`, id).Scan(&n.ID, &n.ProjectID, &n.EventKey, &n.EventJSON, &n.Payload, &n.State, &n.RelayID, &n.Attempts, &n.CreatedAt, &n.NextAttemptAt, &n.AgentName, &n.EventKind)
	return &n, err
}

// NotificationCanSubmit atomically retires disallowed unsent intents and checks
// current preferences after network metadata reads. Merely returning false would
// leave a late/future producer's pending event eligible after a later unmute.
// Accepted receipts and an already-started POST are not recalled by this gate.
func (d *DB) NotificationCanSubmit(id int64) (bool, error) {
	tx, err := d.conn.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`UPDATE notification_outbox SET state='muted'
 WHERE id=? AND state='pending' AND (
 NOT EXISTS (SELECT 1 FROM projects p WHERE p.id=notification_outbox.project_id AND p.telegram_notifications=1)
 OR COALESCE((SELECT value FROM app_settings WHERE key='telegram_capture_enabled'),'1')<>'1'
 OR NOT (`+agentNotificationAllowedSQL+`))`, id)
	if err != nil {
		return false, err
	}
	var okay bool
	err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM notification_outbox JOIN projects p ON p.id=notification_outbox.project_id
   WHERE notification_outbox.id=? AND notification_outbox.state='pending' AND p.telegram_notifications=1
     AND COALESCE((SELECT value FROM app_settings WHERE key='telegram_capture_enabled'),'1')='1' AND `+agentNotificationAllowedSQL+`)`, id).Scan(&okay)
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return okay, nil
}

// RecordNotificationAcceptance is only for a validated relay receipt from an
// HTTP POST that already started. A concurrent mute cannot recall that send;
// keep tracking it without allowing an unsent muted intent to be retried.
func (d *DB) RecordNotificationAcceptance(id int64, state, relayID string, attempts int, next time.Time) error {
	switch state {
	case "submitted", "delivered", "failed":
	default:
		return fmt.Errorf("invalid acceptance state")
	}
	if len(relayID) != 32 {
		return fmt.Errorf("invalid acceptance id")
	}
	_, err := d.conn.Exec(`UPDATE notification_outbox SET state=?,relay_id=?,attempts=?,next_attempt_at=?
   WHERE id=? AND relay_id='' AND state IN ('pending','muted','suppressed')`, state, relayID, attempts, next.Unix(), id)
	return err
}

// backfillNotificationAgents examines only the bounded outbox, never build logs
// or historical builds. Legacy agent names are correlated through hashed keys,
// not sanitized display payloads (which are not reversible identities).
func backfillNotificationAgents(tx *sql.Tx) error {
	var done int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM app_settings WHERE key='notification_agent_metadata_v2'`).Scan(&done); err != nil {
		return err
	}
	if done != 0 {
		return nil
	}
	rows, err := tx.Query(`SELECT id,project_id,event_key,event_json,build_id IS NOT NULL FROM notification_outbox WHERE state='pending' AND event_kind='' ORDER BY id LIMIT 1000`)
	if err != nil {
		return err
	}
	type legacy struct {
		id, project int64
		key, event  string
		build       bool
	}
	var entries []legacy
	for rows.Next() {
		var n legacy
		if err = rows.Scan(&n.id, &n.project, &n.key, &n.event, &n.build); err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, n := range entries {
		name, kind := "", "agent"
		if n.build {
			kind = "build"
			var event struct {
				Agent string `json:"agent"`
			}
			if json.Unmarshal([]byte(n.event), &event) == nil {
				name = event.Agent
			}
		} else {
			seen, err := tx.Query(`SELECT name FROM notification_agent_seen WHERE project_id=?`, n.project)
			if err != nil {
				return err
			}
			for seen.Next() {
				var candidate string
				if err = seen.Scan(&candidate); err != nil {
					seen.Close()
					return err
				}
				sum := sha256.Sum256([]byte(candidate))
				if strings.Contains(n.key, fmt.Sprintf(":agent:%d:%x:", n.project, sum[:16])) {
					name = candidate
					break
				}
			}
			err = seen.Err()
			seen.Close()
			if err != nil {
				return err
			}
		}
		if _, err = tx.Exec(`UPDATE notification_outbox SET agent_name=?,event_kind=? WHERE id=?`, name, kind, n.id); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`INSERT INTO app_settings(key,value) VALUES('notification_agent_metadata_v2','1')`)
	return err
}
