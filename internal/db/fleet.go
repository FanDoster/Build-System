package db

import (
	"time"

	"github.com/FanDoster/Build-System/internal/models"
)

// Queries behind the agents page. All read-only, all against columns that
// already exist.
//
// Two rules apply throughout and are easy to forget:
//
// Timestamps are never compared in SQL. The builds table holds two text
// formats side by side — created_at from SQLite's datetime('now') and
// last_heartbeat_at from the Go driver — so a WHERE on either silently returns
// the wrong rows. Staleness is decided in Go, as FailStaleRunning already does.
//
// Aggregates are never taken over a DATETIME column. MAX/MIN lose the declared
// type and the driver hands back a string, which will not Scan into a
// *time.Time. Where a rollup is wanted, the rows are read and folded in Go.

// AgentNames lists every agent that has ever claimed a build, newest first.
// The registry knows who is here now; this knows who has ever been.
func (d *DB) AgentNames() ([]string, error) {
	rows, err := d.conn.Query(
		`SELECT agent, MAX(id) AS latest FROM builds
		 WHERE agent != '' GROUP BY agent ORDER BY latest DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		var latest int64
		if err := rows.Scan(&name, &latest); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// RunningBuildForAgent returns the build an agent is running, or nil.
//
// Deliberately derived rather than stored on an agent record: two independently
// written state fields with different lifetimes will disagree, and the agents
// page would claim a build is running that the build page shows as failed.
func (d *DB) RunningBuildForAgent(agent string) (*models.Build, error) {
	b, err := scanBuildSummary(d.conn.QueryRow(
		`SELECT `+buildSummaryCols+`
		 FROM builds b JOIN projects p ON p.id = b.project_id
		 WHERE b.agent = ? AND b.status = ?
		 ORDER BY b.id DESC LIMIT 1`, agent, models.StatusRunning))
	if err != nil {
		return nil, nil // no row is the ordinary case, not a failure
	}
	return b, nil
}

// RecentBuildsForAgent is an agent's last builds, newest first. Log-free.
func (d *DB) RecentBuildsForAgent(agent string, limit int) ([]models.Build, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := d.conn.Query(
		`SELECT `+buildSummaryCols+`
		 FROM builds b JOIN projects p ON p.id = b.project_id
		 WHERE b.agent = ?
		 ORDER BY b.id DESC LIMIT ?`, agent, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Build
	for rows.Next() {
		b, err := scanBuildSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// RemoteExecutor is one queue a project routes to, and what is waiting on it.
type RemoteExecutor struct {
	Name string
	// Projects using this executor.
	Projects []string
	// Pending is how many builds are waiting for an agent to claim them.
	Pending int
	// OldestPending is when the longest-waiting build was created. Zero when
	// nothing is waiting.
	OldestPending time.Time
}

// RemoteExecutors lists every executor a project is configured to use, with
// its queue depth.
//
// The list comes from projects, not from builds, on purpose: an executor that
// no agent has ever served is exactly the case worth showing, and it is
// invisible if you start from the builds that ran.
func (d *DB) RemoteExecutors() ([]RemoteExecutor, error) {
	rows, err := d.conn.Query(
		`SELECT executor, name FROM projects ORDER BY executor, name`)
	if err != nil {
		return nil, err
	}
	byName := map[string]*RemoteExecutor{}
	var order []string
	for rows.Next() {
		var executor, project string
		if err := rows.Scan(&executor, &project); err != nil {
			rows.Close()
			return nil, err
		}
		if !models.Remote(executor) {
			continue
		}
		e, ok := byName[executor]
		if !ok {
			e = &RemoteExecutor{Name: executor}
			byName[executor] = e
			order = append(order, executor)
		}
		e.Projects = append(e.Projects, project)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Pending builds, folded in Go rather than aggregated in SQL so the
	// timestamp keeps its type.
	pend, err := d.conn.Query(
		`SELECT p.executor, b.created_at FROM builds b
		 JOIN projects p ON p.id = b.project_id
		 WHERE b.status = ?`, models.StatusPending)
	if err != nil {
		return nil, err
	}
	defer pend.Close()
	for pend.Next() {
		var executor string
		var created time.Time
		if err := pend.Scan(&executor, &created); err != nil {
			return nil, err
		}
		e, ok := byName[executor]
		if !ok {
			continue // a local build; not this page's business
		}
		e.Pending++
		if e.OldestPending.IsZero() || created.Before(e.OldestPending) {
			e.OldestPending = created
		}
	}
	if err := pend.Err(); err != nil {
		return nil, err
	}

	out := make([]RemoteExecutor, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}
	return out, nil
}

// LogTailBytes returns the last n bytes of a build's log.
//
// Byte offsets, via the BLOB cast, for the same reason BuildLogLen uses it:
// length() counts characters on a TEXT value and a Unity log is full of
// non-ASCII. The tail is enough to find the current step and avoids pulling a
// log that can be tens of megabytes into memory to read one line.
func (d *DB) LogTailBytes(id int64, n int) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	total, err := d.BuildLogLen(id)
	if err != nil {
		return nil, err
	}
	from := total - n
	if from < 0 {
		from = 0
	}
	return d.BuildLogSlice(id, from, total-from)
}
