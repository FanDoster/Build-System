package db

import (
	"database/sql"
	"encoding/json"
	"time"
	"unicode/utf8"

	"github.com/FanDoster/Build-System/internal/models"
)

// The agents table: what the server remembers about a build machine between
// restarts.
//
// This inverts what the agent protocol was originally built on. "An idle agent
// writes nothing to the database" was a deliberate property — an agent that has
// been long-polling healthily for an hour was, in the builds table, identical
// to one unplugged an hour ago, and that was accepted because the alternative
// was a row written twice a minute per agent forever. Two things changed that.
// Pause has to be durable, or a redeploy silently resumes every paused machine
// at the worst possible moment. And last-seen has to outlive the process, or
// the first minutes after every deploy show a fleet of dead machines.
//
// The write is kept honest by throttling it: at most one row touch per agent
// per AgentSightingInterval, not one per poll. See agents.Registry.ShouldPersist.
//
// Three rules run through everything below.
//
// Timestamps are never compared in SQL. The driver stores time.Time in Go's
// own format while SQLite's datetime() writes another, so datetime(paused_until)
// returns NULL for every row this file writes and a WHERE on it silently
// matches nothing. Pause expiry is decided in Go, by models.AgentPaused.
//
// Aggregates are never taken over a DATETIME column. MAX/MIN/COALESCE lose the
// declared type and the driver hands back a string that will not scan into a
// *time.Time.
//
// The sighting upsert never touches pause. It lists its columns explicitly for
// that reason: INSERT OR REPLACE would rewrite the whole row and blank
// paused_until, so an agent would un-pause itself on its next poll — a pause
// that quietly stops working is worse than no pause at all.

// AgentSightingInterval is the minimum gap between last_seen_at writes for one
// agent. Seconds, not minutes: GitLab throttled theirs to an hour and made
// their own UI wrong by up to an hour. Ten seconds is far below the 90s
// staleness tolerance, so the page is never wrong in a way anyone can see, and
// an agent in a one-second retry loop still writes only six times a minute.
const AgentSightingInterval = 10 * time.Second

// MaxAgentRows caps how many agents the table will hold.
//
// The name is a primary key chosen by whoever holds the agent token, and there
// is no registration step, so without a cap one credential can write unbounded
// rows by varying the name. Past the cap, new names are not stored — but their
// claims are still served, because a full table must never stop CI.
const MaxAgentRows = 500

// AgentRow is one persisted machine.
type AgentRow struct {
	Name        string
	Executors   []string
	FirstSeenAt *time.Time
	LastSeenAt  *time.Time
	LastScheme  string
	PausedUntil *time.Time
	PauseNote   string
}

// Paused reports whether this row's pause is still in force at now.
func (a *AgentRow) Paused(now time.Time) bool {
	return models.AgentPaused(a.PausedUntil, now)
}

const agentCols = `name, executors, first_seen_at, last_seen_at, last_scheme, paused_until, pause_note`

func scanAgentRow(s scanner) (*AgentRow, error) {
	var a AgentRow
	var executors string
	if err := s.Scan(&a.Name, &executors, &a.FirstSeenAt, &a.LastSeenAt,
		&a.LastScheme, &a.PausedUntil, &a.PauseNote); err != nil {
		return nil, err
	}
	// A malformed list is left empty rather than failing the read: this column
	// is display and coverage information, and losing it must not take down the
	// page that exists to explain why nothing is building.
	if executors != "" {
		_ = json.Unmarshal([]byte(executors), &a.Executors)
	}
	return &a, nil
}

// RecordAgentSighting notes that an agent asked for work.
//
// Called from the claim path for every poll that passes the throttle,
// INCLUDING the ones that find nothing — an empty poll is the only trace an
// idle agent leaves anywhere.
//
// first_seen_at is written once and never again. The excluded.first_seen_at
// spelling would be wrong here: it would reset the moment an agent restarted,
// throwing away the one fact this table keeps that nothing else can.
func (d *DB) RecordAgentSighting(name string, executors []string, scheme string, now time.Time) error {
	if !models.ValidAgentName(name) {
		return nil // not an error: a bad name must never fail a claim
	}
	list, err := json.Marshal(executors)
	if err != nil {
		list = []byte("[]")
	}
	now = now.UTC()

	// The cap is checked only for names that are not already present, so a
	// known agent keeps working no matter how full the table is.
	//
	// first_seen_at is filled only when it is still NULL. A row can exist
	// before the machine has ever connected — an operator can pause an agent
	// that has not arrived yet — and in that case the row's creation is not a
	// sighting. This poll is the first one, so it is what first-seen means.
	res, err := d.conn.Exec(
		`UPDATE agents SET executors = ?, last_seen_at = ?, last_scheme = ?,
		     first_seen_at = COALESCE(first_seen_at, ?)
		 WHERE name = ?`,
		string(list), now, scheme, now, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}

	var count int
	if err := d.conn.QueryRow(`SELECT COUNT(*) FROM agents`).Scan(&count); err != nil {
		return err
	}
	if count >= MaxAgentRows {
		return nil // full: serve the claim, remember nothing
	}
	_, err = d.conn.Exec(
		`INSERT INTO agents (name, executors, first_seen_at, last_seen_at, last_scheme)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		     executors    = excluded.executors,
		     last_seen_at = excluded.last_seen_at,
		     last_scheme  = excluded.last_scheme`,
		name, string(list), now, now, scheme)
	return err
}

// AgentPausedUntil returns an agent's pause expiry, or nil if it has none.
//
// Read on the claim path, so it selects one column by primary key. A missing
// row is not an error — an agent the server has never stored is not paused.
func (d *DB) AgentPausedUntil(name string) (*time.Time, error) {
	var until *time.Time
	err := d.conn.QueryRow(`SELECT paused_until FROM agents WHERE name = ?`, name).Scan(&until)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return until, nil
}

// PauseAgent stops an agent being given new builds until until.
//
// The expiry is required and bounded by the caller; this writes what it is
// given. A row is created if the agent has never been seen, so a machine can be
// paused before it first connects — which is the case where pausing matters
// most, because there is nothing else to stop it taking work the moment it
// arrives.
//
// Creating that row does NOT stamp first_seen_at. A pause is not a sighting,
// and a first-seen recording a contact that never happened would never be
// corrected: the sighting path fills that column only while it is NULL.
func (d *DB) PauseAgent(name string, until time.Time, note string) error {
	note = truncateRunes(note, models.MaxPauseNoteLen)
	_, err := d.conn.Exec(
		`INSERT INTO agents (name, paused_until, pause_note)
		 VALUES (?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		     paused_until = excluded.paused_until,
		     pause_note   = excluded.pause_note`,
		name, until.UTC(), note)
	return err
}

// truncateRunes cuts a string to at most n bytes without splitting a rune.
//
// A plain s[:n] cuts mid-sequence for any note containing a non-ASCII
// character, and the invalid bytes survive into SQLite and back out onto the
// page. The note is display text an operator typed; mangling its last character
// because it happened to be an accent is a poor way to enforce a length.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// ResumeAgent clears a pause. Resuming an agent that is not paused, or that has
// no row, is not an error — the caller wants it running, and it is.
func (d *DB) ResumeAgent(name string) error {
	_, err := d.conn.Exec(
		`UPDATE agents SET paused_until = NULL, pause_note = '' WHERE name = ?`, name)
	return err
}

// ForgetAgent removes an agent's row.
//
// Shipped in the same change as the table on purpose: a name is self-asserted,
// so one typo in a config file creates a row that nothing would ever remove.
// It deletes the record, not the history — builds keep their agent column, so
// the machine reappears in the fleet listing from its build history until those
// builds age out.
func (d *DB) ForgetAgent(name string) error {
	_, err := d.conn.Exec(`DELETE FROM agents WHERE name = ?`, name)
	return err
}

// ListAgentRows returns every persisted agent, oldest name first for a stable
// order.
func (d *DB) ListAgentRows() ([]AgentRow, error) {
	rows, err := d.conn.Query(`SELECT ` + agentCols + ` FROM agents ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentRow
	for rows.Next() {
		a, err := scanAgentRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// GetAgentRow returns one persisted agent, or nil.
func (d *DB) GetAgentRow(name string) (*AgentRow, error) {
	a, err := scanAgentRow(d.conn.QueryRow(`SELECT `+agentCols+` FROM agents WHERE name = ?`, name))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}
