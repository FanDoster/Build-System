package db

import (
	"database/sql"
	"fmt"

	"github.com/FanDoster/Build-System/internal/models"
)

func (d *DB) AgentNotificationPreferences(name string) (models.AgentNotificationPreferences, error) {
	p := models.DefaultAgentNotificationPreferences()
	err := d.conn.QueryRow(`SELECT enabled,availability,health,builds FROM agent_notification_preferences WHERE name=?`, name).Scan(&p.Enabled, &p.Availability, &p.Health, &p.Builds)
	if err == sql.ErrNoRows {
		err = nil
	}
	return p, err
}

func (d *DB) ListAgentNotificationPreferences() (map[string]models.AgentNotificationPreferences, error) {
	rows, err := d.conn.Query(`SELECT name,enabled,availability,health,builds FROM agent_notification_preferences`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]models.AgentNotificationPreferences{}
	for rows.Next() {
		var name string
		var p models.AgentNotificationPreferences
		if err := rows.Scan(&name, &p.Enabled, &p.Availability, &p.Health, &p.Builds); err != nil {
			return nil, err
		}
		out[name] = p
	}
	return out, rows.Err()
}

// UpdateAgentNotificationPreferences merges a partial operator change under the
// same transaction that permanently suppresses matching unsent intents. The
// independent table is never written by sightings, self-reports or forgetting.
func (d *DB) UpdateAgentNotificationPreferences(name string, patch map[string]bool) (models.AgentNotificationPreferences, error) {
	p := models.DefaultAgentNotificationPreferences()
	if !models.ValidAgentName(name) {
		return p, fmt.Errorf("invalid agent name")
	}
	tx, err := d.conn.Begin()
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	err = tx.QueryRow(`SELECT enabled,availability,health,builds FROM agent_notification_preferences WHERE name=?`, name).Scan(&p.Enabled, &p.Availability, &p.Health, &p.Builds)
	if err != nil && err != sql.ErrNoRows {
		return p, err
	}
	// Retire late intents under the OLD preferences before lifting a mute.
	// Future producers may not prefilter capture, and the worker may not have
	// reached its final gate yet. Neither an unmute nor a category change may
	// make an intent queued during the muted interval eligible for delivery.
	suppress := func(p models.AgentNotificationPreferences) error {
		_, err := tx.Exec(`UPDATE notification_outbox SET state='muted' WHERE state='pending' AND agent_name=? AND (
 ?=0 OR (event_kind='availability' AND ?=0) OR (event_kind='health' AND ?=0) OR (event_kind='build' AND ?=0)
 OR (event_kind IN ('agent','') AND (?=0 OR ?=0)))`, name, p.Enabled, p.Availability, p.Health, p.Builds, p.Availability, p.Health)
		return err
	}
	if err = suppress(p); err != nil {
		return p, err
	}
	for key, value := range patch {
		switch key {
		case "enabled":
			p.Enabled = value
		case "availability":
			p.Availability = value
		case "health":
			p.Health = value
		case "builds":
			p.Builds = value
		default:
			return p, fmt.Errorf("unknown preference")
		}
	}
	_, err = tx.Exec(`INSERT INTO agent_notification_preferences(name,enabled,availability,health,builds) VALUES(?,?,?,?,?)
 ON CONFLICT(name) DO UPDATE SET enabled=excluded.enabled,availability=excluded.availability,health=excluded.health,builds=excluded.builds`, name, p.Enabled, p.Availability, p.Health, p.Builds)
	if err != nil {
		return p, err
	}
	if err = suppress(p); err != nil {
		return p, err
	}
	return p, tx.Commit()
}

// Unknown future event kinds still require the master switch. Legacy combined
// agent messages require BOTH categories because their payload is immutable.
const agentNotificationAllowedSQL = `NOT EXISTS (SELECT 1 FROM agent_notification_preferences a
 WHERE a.name=notification_outbox.agent_name AND notification_outbox.agent_name<>'' AND
 (a.enabled=0 OR (notification_outbox.event_kind='availability' AND a.availability=0)
 OR (notification_outbox.event_kind='health' AND a.health=0)
 OR (notification_outbox.event_kind='build' AND a.builds=0)
 OR (notification_outbox.event_kind IN ('agent','') AND (a.availability=0 OR a.health=0))))`
