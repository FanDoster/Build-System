package db

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/FanDoster/Build-System/internal/models"
)

// Seed large inline legacy records, not production data. The health queries
// must not depend on log migration having already removed overflow pages.
func fleetHealthFixture(tb testing.TB, count int) *DB {
	tb.Helper()
	d, err := Open(filepath.Join(tb.TempDir(), "fleet.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { d.Close() })
	p := &models.Project{Name: "game", Executor: "unity-windows"}
	if err := d.CreateProject(p); err != nil {
		tb.Fatal(err)
	}
	tx, err := d.conn.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO builds
 (project_id, project_build_number, agent, status, log, last_heartbeat_at)
 VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		tb.Fatal(err)
	}
	defer stmt.Close()
	log := strings.Repeat("large legacy log 🛠\n", 4096)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for i := 0; i < count; i++ {
		status := models.StatusFailed
		if i >= count-16 {
			status = models.StatusRunning
		}
		if _, err := stmt.Exec(p.ID, i+1, fmt.Sprintf("machine-%02d", i%16), status, log, now); err != nil {
			tb.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	return d
}

func fleetQueryPlan(t *testing.T, d *DB, query string, args ...any) string {
	t.Helper()
	rows, err := d.conn.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(details, "\n")
}

func TestFleetHealthCoveringQueryPlans(t *testing.T) {
	d := fleetHealthFixture(t, 256)
	// Check the planner both before and after representative statistics exist.
	for _, analyzed := range []bool{false, true} {
		if analyzed {
			if _, err := d.conn.Exec("ANALYZE"); err != nil {
				t.Fatal(err)
			}
		}
		for _, tc := range []struct {
			name, query, index string
			args               []any
		}{
			{"names", agentNamesSQL, "idx_builds_agent_recent", nil},
			{"newest ten statuses", recentBuildStatusesSQL, "idx_builds_agent_recent", []any{"machine-00", 10}},
			{"current ownership and heartbeat", runningBuildHealthSQL, "idx_builds_agent_running_health", []any{"machine-00", models.StatusRunning}},
		} {
			t.Run(fmt.Sprintf("%s/analyzed=%v", tc.name, analyzed), func(t *testing.T) {
				plan := fleetQueryPlan(t, d, tc.query, tc.args...)
				t.Log(plan)
				if !strings.Contains(plan, "USING COVERING INDEX "+tc.index) {
					t.Fatalf("health query visits builds records instead of covering index %s:\n%s", tc.index, plan)
				}
				// Sorting the small grouped list of agent names is expected; sorting
				// running/recent build rows would lose the bounded index-seek behavior.
				if tc.name != "names" && strings.Contains(plan, "TEMP B-TREE") {
					t.Fatalf("health lookup sorts instead of seeking:\n%s", plan)
				}
			})
		}
	}
}

func TestFleetHealthDatabaseProjectionAndWindow(t *testing.T) {
	d := openTestDB(t)
	p := newProject(t, d)
	p.Executor = "unity-windows"
	if err := d.UpdateProject(p); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	statuses := []models.BuildStatus{models.StatusFailed, models.StatusFailed,
		models.StatusSuccess, models.StatusFailed, models.StatusCanceled,
		models.StatusFailed, models.StatusFailed, models.StatusFailed,
		models.StatusPending, models.StatusRunning, models.StatusRunning, models.StatusPending}
	var newestRunning int64
	for i, status := range statuses {
		b := &models.Build{ProjectID: p.ID, Status: status, CommitSHA: "not needed", CommitMessage: "not needed"}
		if err := d.CreateBuild(b); err != nil {
			t.Fatal(err)
		}
		// Mix SQLite's timestamp form and Go-driver timestamps without comparing
		// either in SQL. Newest ownership is chosen by global id, not heartbeat.
		var hb any = now
		if i%2 == 0 {
			hb = "2026-10-16 12:00:00"
		}
		if _, err := d.conn.Exec(`UPDATE builds SET agent = 'machine', last_heartbeat_at = ? WHERE id = ?`, hb, b.ID); err != nil {
			t.Fatal(err)
		}
		if status == models.StatusRunning {
			newestRunning = b.ID
		}
	}
	running, err := d.RunningBuildHealthForAgent("machine")
	if err != nil {
		t.Fatal(err)
	}
	if running == nil || running.ID != newestRunning || running.ProjectID != p.ID || running.ProjectName != p.Name || running.Executor != p.Executor || !running.LastHeartbeatAt.Equal(now) {
		t.Fatalf("wrong ownership/heartbeat: %+v", running)
	}
	if running.Log != "" || running.CommitSHA != "" || running.CommitMessage != "" || running.Number != 0 {
		t.Fatalf("health projection read display/history fields: %+v", running)
	}
	// Old releases ran without FK enforcement. Keep the UI's INNER JOIN
	// semantics even if a newer orphan would otherwise enter the window.
	if _, err := d.conn.Exec(`PRAGMA foreign_keys = OFF;
      INSERT INTO builds (project_id, project_build_number, agent, status)
      VALUES (999999, 1, 'machine', 'canceled');
      PRAGMA foreign_keys = ON;`); err != nil {
		t.Fatal(err)
	}
	got, err := d.RecentBuildStatusesForAgent("machine", 0)
	if err != nil {
		t.Fatal(err)
	}
	var want []models.BuildStatus
	for i := len(statuses) - 1; i >= len(statuses)-10; i-- {
		want = append(want, statuses[i])
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("status window = %v, want %v", got, want)
	}
	if missing, err := d.RunningBuildHealthForAgent("missing"); err != nil || missing != nil {
		t.Fatalf("missing = %+v, %v", missing, err)
	}
	d.Close()
	if _, err := d.RunningBuildHealthForAgent("machine"); err == nil {
		t.Fatal("database failure treated as absent ownership")
	}
	if _, err := d.RecentBuildStatusesForAgent("machine", 10); err == nil {
		t.Fatal("database failure treated as empty history")
	}
}

func BenchmarkFleetHealthDatabase(b *testing.B) {
	d := fleetHealthFixture(b, 256)
	b.Run("health/index-only", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			names, err := d.AgentNames()
			if err != nil {
				b.Fatal(err)
			}
			for _, name := range names {
				if _, err := d.RunningBuildHealthForAgent(name); err != nil {
					b.Fatal(err)
				}
				if _, err := d.RecentBuildStatusesForAgent(name, 10); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
	b.Run("UI/full-summaries", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			names, err := d.AgentNames()
			if err != nil {
				b.Fatal(err)
			}
			for _, name := range names {
				if _, err := d.RunningBuildForAgent(name); err != nil {
					b.Fatal(err)
				}
				if _, err := d.RecentBuildsForAgent(name, 10); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
}
