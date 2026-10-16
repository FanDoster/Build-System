package db

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/FanDoster/Build-System/internal/models"
)

func telegramProject(t *testing.T, d *DB, name string) *models.Project {
	t.Helper()
	p := &models.Project{Name: name, RepoURL: "https://private.example/repo", Branch: "main", DockerfilePath: "Dockerfile", ImageName: name, TelegramNotifications: true}
	if err := d.CreateProject(p); err != nil {
		t.Fatal(err)
	}
	return p
}
func notificationCount(t *testing.T, d *DB) int {
	t.Helper()
	var count int
	if err := d.conn.QueryRow(`SELECT COUNT(*) FROM notification_outbox`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestNotificationMigrationSeedsHistoryAndDefaultsExistingProjects(t *testing.T) {
	d := openTestDB(t)
	p := telegramProject(t, d, "history")
	// Simulate the genuine previous schema, before notification observations.
	_, err := d.conn.Exec(`DROP TRIGGER notification_build_insert; DROP TRIGGER notification_build_update; DROP TRIGGER notification_project_mute;
 DROP TABLE notification_outbox; DROP TABLE notification_build_seen; DROP TABLE notification_agent_seen;
 DELETE FROM app_settings WHERE key='notification_build_seed_v1';
 ALTER TABLE projects DROP COLUMN telegram_notifications;`)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []models.BuildStatus{models.StatusSuccess, models.StatusFailed, models.StatusCanceled} {
		if err := d.CreateBuild(&models.Build{ProjectID: p.ID, Status: status}); err != nil {
			t.Fatal(err)
		}
	}
	active := &models.Build{ProjectID: p.ID, Status: models.StatusRunning}
	if err := d.CreateBuild(active); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := d.migrate(); err != nil {
			t.Fatal(err)
		}
	}
	got, err := d.GetProject(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.TelegramNotifications {
		t.Fatal("existing project did not default on")
	}
	if notificationCount(t, d) != 0 {
		t.Fatal("historical terminal replay")
	}
	if err := d.FinishBuild(active.ID, models.StatusFailed); err != nil {
		t.Fatal(err)
	}
	if notificationCount(t, d) != 1 {
		t.Fatal("pre-migration running outcome was lost")
	}
	// A model's explicit zero/false is respected for direct DB callers.
	muted := &models.Project{Name: "muted", RepoURL: "x", ImageName: "muted"}
	if err := d.CreateProject(muted); err != nil {
		t.Fatal(err)
	}
	got, err = d.GetProject(muted.ID)
	if err != nil || got.TelegramNotifications {
		t.Fatalf("direct false ignored: %+v %v", got, err)
	}
}

func TestNotificationTriggersCoverEveryTerminalWriterAndSkipRequeues(t *testing.T) {
	cases := []struct {
		name string
		run  func(*DB, *models.Build) error
	}{
		{"finish local", func(d *DB, b *models.Build) error { return d.FinishBuild(b.ID, models.StatusSuccess) }},
		{"timeout update", func(d *DB, b *models.Build) error {
			return d.UpdateBuildStatus(b.ID, models.StatusFailed, "private log must not appear")
		}},
		{"queued cancel", func(d *DB, b *models.Build) error { _, e := d.CancelPendingBuild(b.ID); return e }},
		{"agent canceled", func(d *DB, b *models.Build) error {
			_, e := d.conn.Exec(`UPDATE builds SET agent='real-machine',status='running' WHERE id=?`, b.ID)
			if e != nil {
				return e
			}
			return d.FinishBuild(b.ID, models.StatusCanceled)
		}},
		{"janitor local", func(d *DB, b *models.Build) error {
			if e := d.UpdateBuildStatus(b.ID, models.StatusRunning, ""); e != nil {
				return e
			}
			_, e := d.FailStaleRunning(0, time.Now())
			return e
		}},
		{"janitor agent", func(d *DB, b *models.Build) error {
			_, e := d.conn.Exec(`UPDATE builds SET agent='remote',status='running',last_heartbeat_at=? WHERE id=?`, time.Now().Add(-time.Hour), b.ID)
			if e != nil {
				return e
			}
			_, e = d.FailStaleRunning(0, time.Now().Add(-time.Hour))
			return e
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := openTestDB(t)
			p := telegramProject(t, d, "project")
			b := &models.Build{ProjectID: p.ID, Status: models.StatusPending}
			if e := d.CreateBuild(b); e != nil {
				t.Fatal(e)
			}
			if e := tc.run(d, b); e != nil {
				t.Fatal(e)
			}
			if n := notificationCount(t, d); n != 1 {
				t.Fatalf("events=%d", n)
			}
			if err := d.FinishBuild(b.ID, models.StatusFailed); err != nil {
				t.Fatal(err)
			}
			if n := notificationCount(t, d); n != 1 {
				t.Fatalf("duplicate terminal=%d", n)
			}
		})
	}
	d := openTestDB(t)
	p := telegramProject(t, d, "restart")
	b := &models.Build{ProjectID: p.ID, Status: models.StatusPending}
	if err := d.CreateBuild(b); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimBuild(b.ID); err != nil {
		t.Fatal(err)
	}
	if okay, err := d.RequeueBuild(b.ID, models.MaxBuildRequeues); err != nil || !okay {
		t.Fatal(okay, err)
	}
	if notificationCount(t, d) != 0 {
		t.Fatal("restart requeue is not terminal")
	}
	if err := d.FinishBuild(b.ID, models.StatusSuccess); err != nil {
		t.Fatal(err)
	}
	if notificationCount(t, d) != 1 {
		t.Fatal("eventual success omitted")
	}
}

func TestNotificationOutboxBoundedLogFreeRetentionAndStableRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	p := telegramProject(t, d, "bounded")
	for i := 0; i < NotificationQueueLimit+3; i++ {
		b := &models.Build{ProjectID: p.ID, Status: models.StatusSuccess}
		if err := d.CreateBuild(b); err != nil {
			t.Fatal(err)
		}
	}
	if n := notificationCount(t, d); n != NotificationQueueLimit {
		t.Fatalf("queue not bounded: %d", n)
	}
	rows, err := d.DueNotifications(time.Now().Add(time.Second), 10000)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 25 {
		t.Fatalf("query unbounded: %d", len(rows))
	}
	key := rows[0].EventKey
	if strings.Contains(rows[0].EventJSON, "log") {
		t.Fatal("log captured")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows, err = d.DueNotifications(time.Now().Add(time.Second), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].EventKey != key {
		t.Fatal("restart changed idempotency")
	}
	if err := d.PruneNotifications(time.Now().Add(25 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, err = d.DueNotifications(time.Now().Add(25*time.Hour), 25)
	if err != nil || len(rows) != 0 {
		t.Fatalf("stale intents not expired: %v %d", err, len(rows))
	}
	if err := d.PruneNotifications(time.Now().Add(8 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if notificationCount(t, d) != 0 {
		t.Fatal("retention not bounded")
	}
	// Observations outlive outbox cleanup, so repeat terminal writes do not
	// resurrect evicted events.
	if _, err := d.conn.Exec(`UPDATE builds SET status='failed'`); err != nil {
		t.Fatal(err)
	}
	if notificationCount(t, d) != 0 {
		t.Fatal("expired terminal event replayed")
	}
}

func TestAgentObservationKeysUseSafeHashAndMutedBaseline(t *testing.T) {
	d := openTestDB(t)
	p := telegramProject(t, d, "agents")
	now := time.Now()
	name := "Build Mac 🌍 with spaces"
	payload := `{"text":"hello","format":"plain"}`
	if err := d.ObserveNotificationAgent(p.ID, name, "healthy", payload, false, now); err != nil {
		t.Fatal(err)
	}
	if notificationCount(t, d) != 0 {
		t.Fatal("healthy startup storm")
	}
	if err := d.ObserveNotificationAgent(p.ID, name, "bad", payload, true, now); err != nil {
		t.Fatal(err)
	}
	due, err := d.DueNotifications(now, 1)
	if err != nil || len(due) != 1 {
		t.Fatal(due, err)
	}
	if len(due[0].EventKey) > 128 || !regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`).MatchString(due[0].EventKey) {
		t.Fatalf("unsafe key %q", due[0].EventKey)
	}
	p.TelegramNotifications = false
	if err := d.UpdateProject(p); err != nil {
		t.Fatal(err)
	}
	if err := d.ObserveNotificationAgent(p.ID, name, "worse", payload, true, now); err != nil {
		t.Fatal(err)
	}
	p.TelegramNotifications = true
	if err := d.UpdateProject(p); err != nil {
		t.Fatal(err)
	}
	if err := d.ObserveNotificationAgent(p.ID, name, "worse", payload, true, now); err != nil {
		t.Fatal(err)
	}
	if notificationCount(t, d) != 1 {
		t.Fatal("unmute replayed previous issue")
	}
}

func TestNotificationCaptureBoundsMetadataNotLogs(t *testing.T) {
	d := openTestDB(t)
	p := telegramProject(t, d, "large")
	p.Branch = strings.Repeat("a", 50000)
	if err := d.UpdateProject(p); err != nil {
		t.Fatal(err)
	}
	b := &models.Build{ProjectID: p.ID, Status: models.StatusPending, CommitSHA: strings.Repeat("b", 50000)}
	if err := d.CreateBuild(b); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateBuildStatus(b.ID, models.StatusSuccess, strings.Repeat("secret-log", 100000)); err != nil {
		t.Fatal(err)
	}
	rows, err := d.DueNotifications(time.Now().Add(time.Second), 1)
	if err != nil || len(rows) != 0 {
		t.Fatal("oversized raw metadata must be safely discarded", rows, err)
	}
	// Avoid truncating raw strings before known-secret redaction: a bounded
	// whole snapshot is retained, or none at all. Log size is irrelevant.
	p.Branch = "main"
	if err := d.UpdateProject(p); err != nil {
		t.Fatal(err)
	}
	b = &models.Build{ProjectID: p.ID, Status: models.StatusPending, CommitSHA: "abc"}
	if err := d.CreateBuild(b); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateBuildStatus(b.ID, models.StatusSuccess, strings.Repeat("secret-log", 100000)); err != nil {
		t.Fatal(err)
	}
	rows, err = d.DueNotifications(time.Now().Add(time.Second), 1)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	if len(rows[0].EventJSON) > 16384 || !json.Valid([]byte(rows[0].EventJSON)) || strings.Contains(rows[0].EventJSON, "secret-log") {
		t.Fatal("unbounded or logged capture", fmt.Sprint(len(rows[0].EventJSON)))
	}
}
