package runner

import (
	"context"
	"database/sql"
	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/logbus"
	"github.com/FanDoster/Build-System/internal/models"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSinkFailedCommitNeverPublishesAndCloseRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	p := &models.Project{Name: "p", RepoURL: "/synthetic", ImageName: "p"}
	if err = d.CreateProject(p); err != nil {
		t.Fatal(err)
	}
	row := &models.Build{ProjectID: p.ID, Status: models.StatusRunning}
	if err = d.CreateBuild(row); err != nil {
		t.Fatal(err)
	}
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = conn.Exec(`CREATE TRIGGER reject_log BEFORE INSERT ON build_log_chunks BEGIN SELECT RAISE(FAIL,'synthetic failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	bus := logbus.New()
	s := newLogSink(row.ID, "", d, bus)
	s.Write([]byte("世界\n"))
	if err = s.Close(); err == nil {
		t.Fatal("close must report failed persistence")
	}
	if n, _ := bus.Length(row.ID); n != 0 {
		t.Fatal("uncommitted bytes published", n)
	}
	if n, err := d.BuildLogLen(row.ID); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	conn.Exec(`DROP TRIGGER reject_log`)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	tail, _, _ := bus.LogTail(row.ID, 0)
	if string(tail) != "世界\n" {
		t.Fatal(string(tail))
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	tail, _, _ = bus.LogTail(row.ID, 0)
	if string(tail) != "世界\n" {
		t.Fatal("retry duplicated bytes")
	}
}
func TestSinkResumesDurablePrefixBeforePublishing(t *testing.T) {
	d, bus, id := sinkFixtures(t)
	prefix := strings.Repeat("old π\n", 10000)
	if err := d.AppendBuildLog(id, prefix); err != nil {
		t.Fatal(err)
	}
	s := newLogSink(id, "", d, bus)
	s.Write([]byte("new 世界\n"))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	tail, _, _ := bus.LogTail(id, 0)
	if string(tail) != prefix+"new 世界\n" {
		t.Fatal("missing prefix or repeated append")
	}
}

func TestRequeuePersistenceErrorDoesNotFabricateExhaustedRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	p := &models.Project{Name: "p", RepoURL: "/synthetic", Branch: "main", DockerfilePath: "Dockerfile", ImageName: "p"}
	if err = d.CreateProject(p); err != nil {
		t.Fatal(err)
	}
	row := &models.Build{ProjectID: p.ID, Status: models.StatusRunning}
	if err = d.CreateBuild(row); err != nil {
		t.Fatal(err)
	}
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = conn.Exec(`CREATE TRIGGER reject_requeue BEFORE INSERT ON build_log_chunks WHEN CAST(NEW.data AS TEXT) LIKE '%[restart]%' BEGIN SELECT RAISE(FAIL,'synthetic requeue failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	bus := logbus.New()
	r := New(d, make(chan *models.Build), bus)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.runBuild(ctx, row, time.Now())
	got, err := d.GetBuild(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.StatusRunning || got.Requeues != 0 || got.FinishedAt != nil {
		t.Fatalf("retryable error rewrote outcome: %s %d", got.Status, got.Requeues)
	}
	if strings.Contains(got.Log, "no re-queues left") || strings.Contains(got.Log, "[restart]") {
		t.Fatal("failed requeue wrote false history")
	}
	if !bus.Live(row.ID) {
		t.Fatal("failed requeue published terminal")
	}
}
