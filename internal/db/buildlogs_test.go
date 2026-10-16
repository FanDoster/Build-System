package db

import (
	"bytes"
	"fmt"
	"github.com/FanDoster/Build-System/internal/models"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChunkLogLegacyCompatibility(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	p := newProject(t, d)
	legacy := strings.Repeat("old π\x00世界\n", 10000)
	res, err := d.conn.Exec(`INSERT INTO builds(project_id,project_build_number,status,log) VALUES(?,1,'running',?)`, p.ID, legacy)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	// Recreate the exact old log layout, with neither new table present.
	if _, err = d.conn.Exec(`DROP TABLE build_log_chunks; DROP TABLE build_log_state`); err != nil {
		t.Fatal(err)
	}
	if err = d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { d.Close() }()
	if err = d.migrate(); err != nil {
		t.Fatal(err)
	}
	var states int
	if err = d.conn.QueryRow(`SELECT count(*) FROM build_log_state`).Scan(&states); err != nil || states != 0 {
		t.Fatalf("eager migration: states=%d %v", states, err)
	}
	suffix := strings.Repeat("new 🦊\x00\n", 19000)
	if err = d.AppendBuildLog(id, suffix); err != nil {
		t.Fatal(err)
	}
	want := legacy + suffix
	var stored string
	var legacyN, total int
	if err = d.conn.QueryRow(`SELECT log FROM builds WHERE id=?`, id).Scan(&stored); err != nil || stored != legacy {
		t.Fatalf("prefix rewritten %v", err)
	}
	if err = d.conn.QueryRow(`SELECT legacy_bytes,total_bytes FROM build_log_state WHERE build_id=?`, id).Scan(&legacyN, &total); err != nil || legacyN != len(legacy) || total != len(want) {
		t.Fatalf("bounds %d %d %v", legacyN, total, err)
	}
	assertLogReads(t, d, id, want)
	if err = d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	assertLogReads(t, d, id, want)
	if err = d.DeleteProject(p.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"build_log_state", "build_log_chunks"} {
		var n int
		if err = d.conn.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("cascade %s %d %v", table, n, err)
		}
	}
}

func assertLogReads(t *testing.T, d *DB, id int64, want string) {
	t.Helper()
	if n, err := d.BuildLogLen(id); err != nil || n != len(want) {
		t.Fatalf("len=%d err=%v want=%d", n, err, len(want))
	}
	for _, offset := range []int{0, 1, 3, 65535, 65536, 65537, len(want) - 100, len(want) - 1, len(want), len(want) + 5} {
		if offset < 0 {
			continue
		}
		for _, n := range []int{1, 13, 65540, len(want) + 5} {
			got, err := d.BuildLogSlice(id, offset, n)
			if err != nil {
				t.Fatal(err)
			}
			start := offset
			if start > len(want) {
				start = len(want)
			}
			end := start + n
			if end > len(want) {
				end = len(want)
			}
			if !bytes.Equal(got, []byte(want[start:end])) {
				t.Fatalf("slice [%d:%d] got %d want %d", offset, n, len(got), end-start)
			}
		}
	}
	b, err := d.GetBuild(id)
	if err != nil || b.Log != want {
		t.Fatalf("GetBuild %v", err)
	}
	for _, list := range []func() ([]models.Build, error){func() ([]models.Build, error) { return d.ListRecentBuilds(20) }, func() ([]models.Build, error) { return d.ListBuildsByProject(b.ProjectID, 20) }, func() ([]models.Build, error) { return d.ListBuildsByStatus(b.Status) }} {
		builds, err := list()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, v := range builds {
			if v.ID == id {
				found = true
				if v.Log != want {
					t.Fatal("list log mismatch")
				}
			}
		}
		if !found {
			t.Fatal("list missing build")
		}
	}
}

func TestChunkLogsTransactionalWritesAndReplacement(t *testing.T) {
	d := openTestDB(t)
	p := newProject(t, d)
	b := &models.Build{ProjectID: p.ID, Status: models.StatusPending}
	if err := d.CreateBuild(b); err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat("π\x00🦊\n", 20000)
	if err := d.AppendBuildLog(b.ID, want); err != nil {
		t.Fatal(err)
	}
	var maxChunk, legacy int
	d.conn.QueryRow(`SELECT max(length(data)) FROM build_log_chunks`).Scan(&maxChunk)
	d.conn.QueryRow(`SELECT length(CAST(log AS BLOB)) FROM builds WHERE id=?`, b.ID).Scan(&legacy)
	if maxChunk > 65536 || legacy != 0 {
		t.Fatalf("chunk=%d legacy=%d", maxChunk, legacy)
	}
	assertLogReads(t, d, b.ID, want)
	// Fail the second physical chunk. Neither its first chunk nor length/status
	// changes may survive the transaction, including cancel and replacement.
	_, err := d.conn.Exec(`CREATE TRIGGER reject_log BEFORE INSERT ON build_log_chunks BEGIN SELECT RAISE(FAIL,'synthetic disk failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.AppendBuildLog(b.ID, "must not land"); err == nil {
		t.Fatal("append should fail")
	}
	if ok, err := d.CancelPendingBuild(b.ID); err == nil || ok {
		t.Fatal("cancel should roll back")
	}
	if err = d.UpdateBuildStatus(b.ID, models.StatusFailed, "replacement"); err == nil {
		t.Fatal("replace should roll back")
	}
	assertLogReads(t, d, b.ID, want)
	got, _ := d.GetBuildSummary(b.ID)
	if got.Status != models.StatusPending {
		t.Fatal("status escaped failed transaction")
	}
	d.conn.Exec(`DROP TRIGGER reject_log`)
	if err = d.UpdateBuildStatus(b.ID, models.StatusSuccess, "short\x00π"); err != nil {
		t.Fatal(err)
	}
	assertLogReads(t, d, b.ID, "short\x00π")
	if err = d.AppendBuildLog(b.ID, "!"); err != nil {
		t.Fatal(err)
	}
	assertLogReads(t, d, b.ID, "short\x00π!")
}

func TestChunkAgentAppendLeaseAndLengthRace(t *testing.T) {
	d := openTestDB(t)
	p := agentProject(t, d, "game", "mac")
	b := pendingBuild(t, d, p.ID)
	if _, err := d.ClaimBuildForAgent("mac-1", []string{"mac"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AppendAgentBuildLog(b.ID, "wrong", 0, "x"); err != ErrLogConflict {
		t.Fatalf("owner=%v", err)
	}
	if start, err := d.AppendAgentBuildLog(b.ID, "mac-1", 0, "世界"); err != nil || start != 0 {
		t.Fatal(start, err)
	}
	if _, err := d.AppendAgentBuildLog(b.ID, "mac-1", 0, "world"); err != ErrLogConflict {
		t.Fatalf("offset=%v", err)
	}
	d.conn.Exec(`UPDATE builds SET last_heartbeat_at=? WHERE id=?`, time.Now().Add(-time.Hour), b.ID)
	if failed, err := d.FailStaleRunning(0, time.Now().Add(-time.Hour)); err != nil || len(failed) != 1 {
		t.Fatal(failed, err)
	}
	if _, err := d.AppendAgentBuildLog(b.ID, "mac-1", len("世界"+AgentLostNote), "late"); err != ErrLogConflict {
		t.Fatalf("terminal=%v", err)
	}
	assertLogReads(t, d, b.ID, "世界"+AgentLostNote)
}

func TestChunkSliceQueryPlanUsesSeek(t *testing.T) {
	d := openTestDB(t)
	rows, err := d.conn.Query(`EXPLAIN QUERY PLAN SELECT data FROM build_log_chunks WHERE build_id=? AND byte_offset >= COALESCE((SELECT MAX(byte_offset) FROM build_log_chunks WHERE build_id=? AND byte_offset<=?),?) AND byte_offset<? ORDER BY byte_offset`, 1, 1, 99999, 99999, 100100)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plans []string
	for rows.Next() {
		var a, b, c int
		var s string
		if err = rows.Scan(&a, &b, &c, &s); err != nil {
			t.Fatal(err)
		}
		plans = append(plans, s)
	}
	joined := strings.Join(plans, "\n")
	t.Log(joined)
	if strings.Contains(joined, "SCAN build_log_chunks") || strings.Count(joined, "USING PRIMARY KEY") < 2 {
		t.Fatalf("non-seeking plan: %s", joined)
	}
}

func BenchmarkChunkLogTail(b *testing.B) {
	for _, size := range []int{1 << 20, 20 << 20} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			d, err := Open(filepath.Join(b.TempDir(), "bench.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer func() { d.Close() }()
			p := &models.Project{Name: "bench", RepoURL: "/synthetic", Branch: "main", ImageName: "bench"}
			if err = d.CreateProject(p); err != nil {
				b.Fatal(err)
			}
			row := &models.Build{ProjectID: p.ID, Status: models.StatusRunning}
			if err = d.CreateBuild(row); err != nil {
				b.Fatal(err)
			}
			if err = d.AppendBuildLog(row.ID, strings.Repeat("x", size)); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err = d.BuildLogSlice(row.ID, size-100, 100); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestAgentChunkCommitRenewsLeaseAndTerminalDecisionWins(t *testing.T) {
	d := openTestDB(t)
	p := agentProject(t, d, "game", "mac")
	b := pendingBuild(t, d, p.ID)
	if _, err := d.ClaimBuildForAgent("mac-1", []string{"mac"}); err != nil {
		t.Fatal(err)
	}
	d.conn.Exec(`UPDATE builds SET last_heartbeat_at=? WHERE id=?`, time.Now().Add(-time.Hour), b.ID)
	if _, err := d.AppendAgentBuildLog(b.ID, "mac-1", 0, "alive π\n"); err != nil {
		t.Fatal(err)
	}
	if failed, err := d.FailStaleRunning(0, time.Now().Add(-time.Hour)); err != nil || len(failed) != 0 {
		t.Fatalf("fresh append killed: %v %v", failed, err)
	}
	d.conn.Exec(`UPDATE builds SET last_heartbeat_at=? WHERE id=?`, time.Now().Add(-time.Hour), b.ID)
	if failed, err := d.FailStaleRunning(0, time.Now().Add(-time.Hour)); err != nil || len(failed) != 1 {
		t.Fatal(failed, err)
	}
	if won, err := d.FinishAgentBuild(b.ID, "mac-1", models.StatusSuccess); err != nil || won {
		t.Fatalf("late success overwrote terminal: %v %v", won, err)
	}
	got, _ := d.GetBuild(b.ID)
	if got.Status != models.StatusFailed || got.FinishedAt != nil {
		t.Fatal("janitor history rewritten")
	}
}

func TestStaleChunkPartialFailureReturnsCommittedRows(t *testing.T) {
	d := openTestDB(t)
	p := newProject(t, d)
	first := &models.Build{ProjectID: p.ID, Status: models.StatusRunning}
	second := &models.Build{ProjectID: p.ID, Status: models.StatusRunning}
	for _, b := range []*models.Build{first, second} {
		if err := d.CreateBuild(b); err != nil {
			t.Fatal(err)
		}
	}
	_, err := d.conn.Exec(fmt.Sprintf(`CREATE TRIGGER reject_second BEFORE INSERT ON build_log_chunks WHEN NEW.build_id=%d BEGIN SELECT RAISE(FAIL,'synthetic failure'); END`, second.ID))
	if err != nil {
		t.Fatal(err)
	}
	failed, err := d.FailStaleRunning(0, time.Now())
	if err == nil || len(failed) != 1 || failed[0].ID != first.ID {
		t.Fatalf("lost partial result %v %v", failed, err)
	}
	assertLogReads(t, d, first.ID, RestartNote)
	got, _ := d.GetBuildSummary(second.ID)
	if got.Status != models.StatusRunning {
		t.Fatal("failed transaction changed second row")
	}
}
