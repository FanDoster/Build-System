package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/FanDoster/Build-System/internal/models"
)

type DB struct {
	conn *sql.DB
}

func Open(path string) (*DB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}

	// PRAGMA syntax is driver-specific: modernc.org/sqlite reads `_pragma=name(value)`
	// and SILENTLY IGNORES unknown query params (the mattn/go-sqlite3 spelling
	// `_journal_mode=WAL&_foreign_keys=on` left this DB on journal=delete with
	// foreign keys OFF, so ON DELETE CASCADE never fired). Verify with
	// TestPragmasApplied if this string is ever touched.
	conn, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	conn.SetMaxOpenConns(1) // SQLite is single-writer
	conn.SetConnMaxLifetime(0)

	db := &DB{conn: conn}
	if err := db.migrate(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

func (d *DB) Close() error {
	return d.conn.Close()
}

func (d *DB) migrate() error {
	_, err := d.conn.Exec(`
		CREATE TABLE IF NOT EXISTS projects (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			repo_url TEXT NOT NULL,
			branch TEXT NOT NULL DEFAULT 'main',
			dockerfile_path TEXT NOT NULL DEFAULT 'Dockerfile',
			image_name TEXT NOT NULL,
			deploy_compose_path TEXT DEFAULT '',
			deploy_service_name TEXT DEFAULT '',
			webhook_secret TEXT NOT NULL DEFAULT '',
			clone_token TEXT NOT NULL DEFAULT '',
			no_cache INTEGER NOT NULL DEFAULT 0,
			poll_enabled INTEGER NOT NULL DEFAULT 0,
			poll_interval_secs INTEGER NOT NULL DEFAULT 60,
			last_polled_sha TEXT NOT NULL DEFAULT '',
			last_polled_at DATETIME,
			last_poll_error TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT (datetime('now')),
			updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);

		CREATE TABLE IF NOT EXISTS builds (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			status TEXT NOT NULL DEFAULT 'pending',
			commit_sha TEXT NOT NULL DEFAULT '',
			commit_message TEXT NOT NULL DEFAULT '',
			log TEXT NOT NULL DEFAULT '',
			started_at DATETIME,
			finished_at DATETIME,
			created_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);

		CREATE INDEX IF NOT EXISTS idx_builds_project ON builds(project_id, created_at DESC);

		CREATE TABLE IF NOT EXISTS app_settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
	`)
	if err != nil {
		return err
	}
	// Additive column migrations for DBs created before the column existed.
	// CREATE TABLE IF NOT EXISTS never alters an existing table, so new
	// columns must be added explicitly and idempotently.
	for _, c := range []struct{ table, column, decl string }{
		{"projects", "no_cache", "INTEGER NOT NULL DEFAULT 0"},
		{"projects", "poll_enabled", "INTEGER NOT NULL DEFAULT 0"},
		{"projects", "poll_interval_secs", "INTEGER NOT NULL DEFAULT 60"},
		{"projects", "last_polled_sha", "TEXT NOT NULL DEFAULT ''"},
		{"projects", "last_polled_at", "DATETIME"},
		{"projects", "last_poll_error", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := d.addColumnIfMissing(c.table, c.column, c.decl); err != nil {
			return err
		}
	}
	return nil
}

// addColumnIfMissing runs ALTER TABLE ADD COLUMN only when the column is
// absent, so migrate() stays idempotent across restarts.
func (d *DB) addColumnIfMissing(table, column, decl string) error {
	rows, err := d.conn.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return nil // already present
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = d.conn.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl))
	return err
}

// --- App settings (key/value, e.g. the persisted session-signing secret) ---

// GetSetting returns "" (no error) when the key does not exist.
func (d *DB) GetSetting(key string) (string, error) {
	var v string
	err := d.conn.QueryRow(`SELECT value FROM app_settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (d *DB) SetSetting(key, value string) error {
	_, err := d.conn.Exec(
		`INSERT INTO app_settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value,
	)
	return err
}

// --- Projects ---

// projectCols is the full read column list, kept in one place so the several
// project SELECTs and scanProject can never drift apart.
const projectCols = `id, name, repo_url, branch, dockerfile_path, image_name,
	deploy_compose_path, deploy_service_name, webhook_secret, clone_token, no_cache,
	poll_enabled, poll_interval_secs, last_polled_sha, last_polled_at, last_poll_error,
	created_at, updated_at`

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanProject(s scanner) (*models.Project, error) {
	p := &models.Project{}
	err := s.Scan(&p.ID, &p.Name, &p.RepoURL, &p.Branch, &p.DockerfilePath, &p.ImageName,
		&p.DeployComposePath, &p.DeployServiceName, &p.WebhookSecret, &p.CloneToken, &p.NoCache,
		&p.PollEnabled, &p.PollIntervalSecs, &p.LastPolledSHA, &p.LastPolledAt, &p.LastPollError,
		&p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (d *DB) CreateProject(p *models.Project) error {
	p.CreatedAt = time.Now().UTC()
	p.UpdatedAt = p.CreatedAt
	if p.PollIntervalSecs == 0 {
		p.PollIntervalSecs = models.DefaultPollIntervalSecs
	}
	res, err := d.conn.Exec(
		`INSERT INTO projects (name, repo_url, branch, dockerfile_path, image_name, deploy_compose_path, deploy_service_name, webhook_secret, clone_token, no_cache, poll_enabled, poll_interval_secs, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.RepoURL, p.Branch, p.DockerfilePath, p.ImageName,
		p.DeployComposePath, p.DeployServiceName, p.WebhookSecret, p.CloneToken, p.NoCache,
		p.PollEnabled, p.PollIntervalSecs,
		p.CreatedAt, p.UpdatedAt,
	)
	if err != nil {
		return err
	}
	p.ID, _ = res.LastInsertId()
	return nil
}

func (d *DB) GetProject(id int64) (*models.Project, error) {
	return scanProject(d.conn.QueryRow(
		`SELECT `+projectCols+` FROM projects WHERE id = ?`, id))
}

func (d *DB) GetProjectByName(name string) (*models.Project, error) {
	return scanProject(d.conn.QueryRow(
		`SELECT `+projectCols+` FROM projects WHERE name = ?`, name))
}

// ListProjects returns every project with secrets cleared — callers that need
// the webhook secret or clone token must re-fetch the row with GetProject.
func (d *DB) ListProjects() ([]models.Project, error) {
	rows, err := d.conn.Query(`SELECT ` + projectCols + ` FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []models.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		p.Sanitize()
		projects = append(projects, *p)
	}
	return projects, rows.Err()
}

// UpdateProject writes the user-editable columns. Poller bookkeeping
// (last_polled_*) is deliberately NOT written here: a settings save must not
// clobber state the poller owns, and the poller uses UpdatePollState.
func (d *DB) UpdateProject(p *models.Project) error {
	p.UpdatedAt = time.Now().UTC()
	_, err := d.conn.Exec(
		`UPDATE projects SET name=?, repo_url=?, branch=?, dockerfile_path=?, image_name=?,
		 deploy_compose_path=?, deploy_service_name=?, webhook_secret=?, clone_token=?, no_cache=?,
		 poll_enabled=?, poll_interval_secs=?, updated_at=?
		 WHERE id=?`,
		p.Name, p.RepoURL, p.Branch, p.DockerfilePath, p.ImageName,
		p.DeployComposePath, p.DeployServiceName, p.WebhookSecret, p.CloneToken, p.NoCache,
		p.PollEnabled, p.PollIntervalSecs,
		p.UpdatedAt, p.ID,
	)
	return err
}

// UpdatePollState records the result of one poll. A successful poll stores the
// observed tip and clears the error; a failed poll keeps the last known tip
// (so recovery doesn't fire a spurious build) and surfaces the message.
func (d *DB) UpdatePollState(id int64, sha string, pollErr string) error {
	now := time.Now().UTC()
	if pollErr != "" {
		_, err := d.conn.Exec(
			`UPDATE projects SET last_polled_at=?, last_poll_error=? WHERE id=?`,
			now, pollErr, id)
		return err
	}
	_, err := d.conn.Exec(
		`UPDATE projects SET last_polled_at=?, last_polled_sha=?, last_poll_error='' WHERE id=?`,
		now, sha, id)
	return err
}

// ResetPollState forgets the observed tip so the next poll re-seeds instead of
// building. Used when the repo URL or branch changes — the stored SHA belongs
// to a ref that is no longer being watched.
func (d *DB) ResetPollState(id int64) error {
	_, err := d.conn.Exec(
		`UPDATE projects SET last_polled_sha='', last_polled_at=NULL, last_poll_error='' WHERE id=?`, id)
	return err
}

// HasActiveBuild reports whether the project has a build queued or running.
// The poller uses it to avoid stacking builds when one commit's build outlasts
// the poll interval.
func (d *DB) HasActiveBuild(projectID int64) (bool, error) {
	var exists int
	err := d.conn.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM builds WHERE project_id=? AND status IN (?, ?))`,
		projectID, models.StatusPending, models.StatusRunning,
	).Scan(&exists)
	return exists == 1, err
}

func (d *DB) DeleteProject(id int64) error {
	_, err := d.conn.Exec("DELETE FROM projects WHERE id = ?", id)
	return err
}

// --- Builds ---

func (d *DB) CreateBuild(b *models.Build) error {
	res, err := d.conn.Exec(
		`INSERT INTO builds (project_id, status, commit_sha, commit_message, created_at)
		 VALUES (?, ?, ?, ?, datetime('now'))`,
		b.ProjectID, b.Status, b.CommitSHA, b.CommitMessage,
	)
	if err != nil {
		return err
	}
	b.ID, _ = res.LastInsertId()
	return nil
}

func (d *DB) GetBuild(id int64) (*models.Build, error) {
	b := &models.Build{}
	err := d.conn.QueryRow(
		`SELECT b.id, b.project_id, p.name, b.status, b.commit_sha, b.commit_message, b.log, b.started_at, b.finished_at, b.created_at
		 FROM builds b JOIN projects p ON p.id = b.project_id
		 WHERE b.id = ?`, id,
	).Scan(&b.ID, &b.ProjectID, &b.ProjectName, &b.Status, &b.CommitSHA, &b.CommitMessage,
		&b.Log, &b.StartedAt, &b.FinishedAt, &b.CreatedAt)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (d *DB) ListBuildsByProject(projectID int64, limit int) ([]models.Build, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := d.conn.Query(
		`SELECT b.id, b.project_id, p.name, b.status, b.commit_sha, b.commit_message, b.log, b.started_at, b.finished_at, b.created_at
		 FROM builds b JOIN projects p ON p.id = b.project_id
		 WHERE b.project_id = ?
		 ORDER BY b.created_at DESC, b.id DESC LIMIT ?`, projectID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var builds []models.Build
	for rows.Next() {
		var b models.Build
		if err := rows.Scan(&b.ID, &b.ProjectID, &b.ProjectName, &b.Status, &b.CommitSHA, &b.CommitMessage,
			&b.Log, &b.StartedAt, &b.FinishedAt, &b.CreatedAt); err != nil {
			return nil, err
		}
		builds = append(builds, b)
	}
	return builds, rows.Err()
}

func (d *DB) ListRecentBuilds(limit int) ([]models.Build, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := d.conn.Query(
		`SELECT b.id, b.project_id, p.name, b.status, b.commit_sha, b.commit_message, b.log, b.started_at, b.finished_at, b.created_at
		 FROM builds b JOIN projects p ON p.id = b.project_id
		 ORDER BY b.created_at DESC, b.id DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var builds []models.Build
	for rows.Next() {
		var b models.Build
		if err := rows.Scan(&b.ID, &b.ProjectID, &b.ProjectName, &b.Status, &b.CommitSHA, &b.CommitMessage,
			&b.Log, &b.StartedAt, &b.FinishedAt, &b.CreatedAt); err != nil {
			return nil, err
		}
		builds = append(builds, b)
	}
	return builds, rows.Err()
}

func (d *DB) ListBuildsByStatus(status models.BuildStatus) ([]models.Build, error) {
	rows, err := d.conn.Query(
		`SELECT b.id, b.project_id, p.name, b.status, b.commit_sha, b.commit_message, b.log, b.started_at, b.finished_at, b.created_at
		 FROM builds b JOIN projects p ON p.id = b.project_id
		 WHERE b.status = ?
		 ORDER BY b.created_at ASC`, status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var builds []models.Build
	for rows.Next() {
		var b models.Build
		if err := rows.Scan(&b.ID, &b.ProjectID, &b.ProjectName, &b.Status, &b.CommitSHA, &b.CommitMessage,
			&b.Log, &b.StartedAt, &b.FinishedAt, &b.CreatedAt); err != nil {
			return nil, err
		}
		builds = append(builds, b)
	}
	return builds, rows.Err()
}

// ClaimBuild atomically transitions a pending build to running. Returns false
// if the build was not pending (e.g. canceled while queued).
func (d *DB) ClaimBuild(id int64) (bool, error) {
	res, err := d.conn.Exec(
		`UPDATE builds SET status=?, started_at=? WHERE id=? AND status=?`,
		models.StatusRunning, time.Now().UTC(), id, models.StatusPending,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// CancelPendingBuild atomically cancels a build that has not started yet.
// Returns false if the build was not pending.
func (d *DB) CancelPendingBuild(id int64) (bool, error) {
	res, err := d.conn.Exec(
		`UPDATE builds SET status=?, finished_at=?, log = log || '[canceled while queued]' || char(10)
		 WHERE id=? AND status=?`,
		models.StatusCanceled, time.Now().UTC(), id, models.StatusPending,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// FinishBuild marks a build terminal without touching the log column (the log
// has already been streamed in via AppendBuildLog).
func (d *DB) FinishBuild(id int64, status models.BuildStatus) error {
	_, err := d.conn.Exec(
		`UPDATE builds SET status=?, finished_at=? WHERE id=?`,
		status, time.Now().UTC(), id,
	)
	return err
}

// FailStaleRunning marks every running build except exceptID as failed.
// finished_at is deliberately left untouched (NULL): the build's real end
// time is unknowable, and stamping "now" poisons history durations. With a
// single worker, any running row that isn't the current build is stale by
// definition (crash, SIGKILL, or an abandoned process).
func (d *DB) FailStaleRunning(exceptID int64) ([]int64, error) {
	rows, err := d.conn.Query(
		`SELECT id FROM builds WHERE status=? AND id != ?`, models.StatusRunning, exceptID,
	)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var failed []int64
	for _, id := range ids {
		res, err := d.conn.Exec(
			`UPDATE builds SET status=?, log = log || ? WHERE id=? AND status=?`,
			models.StatusFailed,
			"\n[ERROR] Build interrupted by server restart\n",
			id, models.StatusRunning,
		)
		if err != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n == 1 {
			failed = append(failed, id)
		}
	}
	return failed, nil
}

// RepairInterruptedDurations fixes rows swept by older code, which stamped
// finished_at with the restart time and produced absurd history durations.
// Idempotent; new sweeps leave finished_at NULL from the start.
func (d *DB) RepairInterruptedDurations() (int64, error) {
	res, err := d.conn.Exec(
		`UPDATE builds SET finished_at=NULL
		 WHERE status=? AND finished_at IS NOT NULL
		   AND log LIKE '%[ERROR] Build interrupted by server restart%'`,
		models.StatusFailed,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteOrphanedBuilds removes build rows whose project no longer exists.
// They accumulated while the foreign-key pragma was silently off, so
// DELETE FROM projects never cascaded. Every read path JOINs projects, which
// made these rows invisible in the UI while they kept their logs on disk.
// Idempotent, and a no-op once the cascade is doing its job.
func (d *DB) DeleteOrphanedBuilds() (int64, error) {
	res, err := d.conn.Exec(
		`DELETE FROM builds WHERE project_id NOT IN (SELECT id FROM projects)`,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ExpectedDuration estimates how long the project's next build should take:
// the mean of its last five successful build durations.
func (d *DB) ExpectedDuration(projectID int64) (time.Duration, bool) {
	rows, err := d.conn.Query(
		`SELECT started_at, finished_at FROM builds
		 WHERE project_id=? AND status=? AND started_at IS NOT NULL AND finished_at IS NOT NULL
		 ORDER BY id DESC LIMIT 5`, projectID, models.StatusSuccess,
	)
	if err != nil {
		return 0, false
	}
	defer rows.Close()

	var total time.Duration
	n := 0
	for rows.Next() {
		var started, finished time.Time
		if err := rows.Scan(&started, &finished); err != nil {
			return 0, false
		}
		if d := finished.Sub(started); d > 0 {
			total += d
			n++
		}
	}
	if n == 0 || rows.Err() != nil {
		return 0, false
	}
	return total / time.Duration(n), true
}

// QueuePosition returns a pending build's 1-based position in the run order:
// pending builds ahead of it (lower id) plus one slot for any running build.
func (d *DB) QueuePosition(id int64) (int, error) {
	var ahead int
	err := d.conn.QueryRow(
		`SELECT COUNT(*) FROM builds WHERE status=? AND id < ?`, models.StatusPending, id,
	).Scan(&ahead)
	if err != nil {
		return 0, err
	}
	var running int
	err = d.conn.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM builds WHERE status=?)`, models.StatusRunning,
	).Scan(&running)
	if err != nil {
		return 0, err
	}
	return 1 + ahead + running, nil
}

func (d *DB) UpdateBuildStatus(id int64, status models.BuildStatus, log string) error {
	now := time.Now().UTC()
	var started, finished *time.Time

	switch status {
	case models.StatusRunning:
		started = &now
	case models.StatusSuccess, models.StatusFailed, models.StatusCanceled:
		finished = &now
	}

	_, err := d.conn.Exec(
		`UPDATE builds SET status=?, log=?, started_at=COALESCE(?, started_at), finished_at=? WHERE id=?`,
		status, log, started, finished, id,
	)
	return err
}

func (d *DB) AppendBuildLog(id int64, line string) error {
	_, err := d.conn.Exec(`UPDATE builds SET log = log || ? WHERE id = ?`, line, id)
	return err
}
