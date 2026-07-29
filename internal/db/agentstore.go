package db

import (
	"database/sql"
	"encoding/json"
	"time"

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

	// What the agent says about itself. Everything below is reported by the
	// machine being described and is therefore untrusted — bounded on arrival,
	// escaped on the way out.
	Version     string
	OSArch      string
	StartedAt   *time.Time // the AGENT's start, for uptime — not the server's
	DiskFreeGB  int
	DiskFloorGB int
	// Status is the last full self-report, already parsed. Nil when the agent
	// has never sent one, which is a different thing from having sent a healthy
	// one and must not look the same on the page.
	Status       *models.AgentStatus
	LastStatusAt *time.Time
}

// SelfReport is the small block an agent attaches to its claim poll.
//
// Separate from the fuller status on purpose: this rides a request that fires
// twice a minute forever, so it stays a handful of scalars. A zero field means
// "not reported" and never overwrites what is already stored — an older agent
// that sends nothing must not blank what a newer one said.
type SelfReport struct {
	Version   string
	OSArch    string
	StartedAt *time.Time
	// Pointers, so that "did not report" and "reported zero" are different
	// things. They have to be: a disk that has genuinely filled reports 0 GB
	// free, and treating that as "no reading" would keep the last healthy
	// number on the page at the exact moment it stops being true.
	DiskFreeGB  *int
	DiskFloorGB *int
}

// Paused reports whether this row's pause is still in force at now.
func (a *AgentRow) Paused(now time.Time) bool {
	return models.AgentPaused(a.PausedUntil, now)
}

const agentCols = `name, executors, first_seen_at, last_seen_at, last_scheme, paused_until, pause_note,
	version, os_arch, started_at, disk_free_gb, disk_floor_gb, status_json, last_status_at`

func scanAgentRow(s scanner) (*AgentRow, error) {
	var a AgentRow
	var executors, status string
	if err := s.Scan(&a.Name, &executors, &a.FirstSeenAt, &a.LastSeenAt,
		&a.LastScheme, &a.PausedUntil, &a.PauseNote,
		&a.Version, &a.OSArch, &a.StartedAt, &a.DiskFreeGB, &a.DiskFloorGB,
		&status, &a.LastStatusAt); err != nil {
		return nil, err
	}
	// Neither JSON column can fail the read. Both are display information
	// written by a machine we do not control, and losing the whole page —
	// including the coverage panel — because one agent stored something
	// unparseable would take the page down exactly when it is wanted.
	if executors != "" {
		_ = json.Unmarshal([]byte(executors), &a.Executors)
	}
	if status != "" {
		var st models.AgentStatus
		if json.Unmarshal([]byte(status), &st) == nil {
			a.Status = &st
		}
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
//
// The self block is folded into the same write. Every one of its fields is
// held to "reported, or leave what is there": an agent too old to send the
// block, or one that could not measure its disk this time round, must not blank
// what a newer or healthier poll already stored. NULLIF turns the zero value
// into NULL and COALESCE then keeps the column — so "not reported" and
// "reported as zero" stay distinguishable, which matters because 0 GB free is a
// real and alarming reading.
func (d *DB) RecordAgentSighting(name string, executors []string, scheme string, self SelfReport, now time.Time) error {
	if !models.ValidAgentName(name) {
		return nil // not an error: a bad name must never fail a claim
	}
	list, err := json.Marshal(executors)
	if err != nil {
		list = []byte("[]")
	}
	now = now.UTC()
	version := models.ClampText(self.Version, models.MaxAgentVersionLen)
	osArch := models.ClampText(self.OSArch, models.MaxOSArchLen)
	var startedAt interface{}
	if self.StartedAt != nil && !self.StartedAt.IsZero() {
		startedAt = self.StartedAt.UTC()
	}
	// A negative reading is a broken measurement, not a disk, and is dropped so
	// the page keeps the last number that made sense. Zero is NOT dropped: it
	// is what a full disk reports.
	free := nonNegative(self.DiskFreeGB)
	floor := nonNegative(self.DiskFloorGB)

	// The cap is checked only for names that are not already present, so a
	// known agent keeps working no matter how full the table is.
	//
	// first_seen_at is filled only when it is still NULL. A row can exist
	// before the machine has ever connected — an operator can pause an agent
	// that has not arrived yet — and in that case the row's creation is not a
	// sighting. This poll is the first one, so it is what first-seen means.
	res, err := d.conn.Exec(
		`UPDATE agents SET executors = ?, last_seen_at = ?, last_scheme = ?,
		     first_seen_at = COALESCE(first_seen_at, ?),
		     version       = COALESCE(NULLIF(?, ''), version),
		     os_arch       = COALESCE(NULLIF(?, ''), os_arch),
		     started_at    = COALESCE(?, started_at),
		     disk_free_gb  = COALESCE(?, disk_free_gb),
		     disk_floor_gb = COALESCE(?, disk_floor_gb)
		 WHERE name = ?`,
		string(list), now, scheme, now,
		version, osArch, startedAt, free, floor, name)
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
	// The INSERT carries the block too. Without it a brand-new agent's very
	// first claim would store none of this and the page would stay blank until
	// the next throttle window — the row is created here, not by the UPDATE
	// above, which affected nothing.
	_, err = d.conn.Exec(
		`INSERT INTO agents (name, executors, first_seen_at, last_seen_at, last_scheme,
		                     version, os_arch, started_at, disk_free_gb, disk_floor_gb)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, COALESCE(?, 0), COALESCE(?, 0))
		 ON CONFLICT(name) DO UPDATE SET
		     executors     = excluded.executors,
		     last_seen_at  = excluded.last_seen_at,
		     last_scheme   = excluded.last_scheme,
		     version       = COALESCE(NULLIF(excluded.version, ''), agents.version),
		     os_arch       = COALESCE(NULLIF(excluded.os_arch, ''), agents.os_arch),
		     started_at    = COALESCE(excluded.started_at, agents.started_at),
		     disk_free_gb  = COALESCE(excluded.disk_free_gb, agents.disk_free_gb),
		     disk_floor_gb = COALESCE(excluded.disk_floor_gb, agents.disk_floor_gb)`,
		name, string(list), now, now, scheme,
		version, osArch, startedAt, free, floor)
	return err
}

// nonNegative passes a reported figure through, dropping only a negative one —
// that is a broken measurement rather than a disk. nil means the agent said
// nothing, and the stored value is left alone.
func nonNegative(v *int) interface{} {
	if v == nil || *v < 0 {
		return nil
	}
	return *v
}

// RecordAgentStatus stores an agent's full self-report.
//
// Its own endpoint rather than the claim poll, because the claim carries only
// what is cheap enough to send twice a minute forever. Its own row update
// rather than part of the sighting, because an agent reports this on its own
// timer — including while it is building, when it does not poll at all, which
// is exactly when an operator wants to know the disk is filling up.
//
// A row is created if the agent is not known yet: a machine that reports a
// problem before its first successful claim is the most interesting kind.
func (d *DB) RecordAgentStatus(name string, status *models.AgentStatus, now time.Time) error {
	if !models.ValidAgentName(name) || status == nil {
		return nil
	}
	status.Clamp()
	blob, err := json.Marshal(status)
	if err != nil {
		return err
	}
	// Clamped and still oversized. Give ground in the order an operator would:
	// the inventory first, then checks from the end. Never all of it — a report
	// dropped whole leaves the page silent about a machine that was trying to
	// say something, which is the opposite of what this endpoint is for.
	if len(blob) > models.MaxStatusBytes {
		trimmed := &models.AgentStatus{Checks: status.Checks}
		for {
			if blob, err = json.Marshal(trimmed); err != nil {
				return err
			}
			if len(blob) <= models.MaxStatusBytes || len(trimmed.Checks) <= 1 {
				break
			}
			trimmed.Checks = trimmed.Checks[:len(trimmed.Checks)-1]
		}
		if len(blob) > models.MaxStatusBytes {
			return nil // a single check longer than the cap; nothing to salvage
		}
	}
	now = now.UTC()

	res, err := d.conn.Exec(
		`UPDATE agents SET status_json = ?, last_status_at = ? WHERE name = ?`,
		string(blob), now, name)
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
		return nil
	}
	_, err = d.conn.Exec(
		`INSERT INTO agents (name, status_json, last_status_at) VALUES (?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		     status_json    = excluded.status_json,
		     last_status_at = excluded.last_status_at`,
		name, string(blob), now)
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
	note = models.ClampText(note, models.MaxPauseNoteLen)
	_, err := d.conn.Exec(
		`INSERT INTO agents (name, paused_until, pause_note)
		 VALUES (?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		     paused_until = excluded.paused_until,
		     pause_note   = excluded.pause_note`,
		name, until.UTC(), note)
	return err
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
