package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/FanDoster/Build-System/internal/api"
	"github.com/FanDoster/Build-System/internal/auth"
	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/live"
	"github.com/FanDoster/Build-System/internal/logbus"
	"github.com/FanDoster/Build-System/internal/models"
	"github.com/FanDoster/Build-System/internal/poller"
	"github.com/FanDoster/Build-System/internal/runner"
	"github.com/FanDoster/Build-System/internal/web"
)

func main() {
	addr := getEnv("BUILDS_ADDR", ":8080")
	dbPath := getEnv("BUILDS_DB", "/var/lib/builds/builds.db")
	basePath := getEnv("BUILDS_BASE_PATH", "")

	// Open database
	database, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()
	log.Printf("Database opened: %s", dbPath)

	// Build job queue (buffered channel)
	buildCh := make(chan *models.Build, 100)

	// Live log pub/sub hub
	bus := logbus.New()

	// Recover builds orphaned by a previous shutdown before accepting new work.
	recoverOrphanedBuilds(database, buildCh)

	// Start runner
	r := runner.New(database, buildCh, bus)
	if v := os.Getenv("BUILDS_BUILD_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("Invalid BUILDS_BUILD_TIMEOUT %q: %v", v, err)
		}
		r.Timeout = d
	}
	r.Start()
	log.Printf("Runner started (build timeout: %s)", r.Timeout)

	// Git polling: the pull-based alternative to webhooks, opted into per
	// project in its settings. Idle unless at least one project enables it.
	pl := poller.New(database, buildCh)
	pl.Start()
	log.Printf("Git poller started (sweep every %s)", pl.Tick)

	// Wire up HTTP
	mux := http.NewServeMux()

	// Live dashboard feed. Demand-driven: it samples the DB only while a list
	// page is connected.
	liveHub := live.New(database, r)

	// API
	apiServer := &api.Server{DB: database, BuildCh: buildCh, Bus: bus, Runner: r, Live: liveHub, BasePath: basePath}
	apiServer.RegisterRoutes(mux)

	// Web UI
	webHandler := web.New(database, basePath)
	webHandler.RegisterRoutes(mux)

	// Authentication: everything behind a password when one is configured.
	authz, err := auth.New(database, os.Getenv("BUILDS_PASSWORD"), os.Getenv("BUILDS_PASSWORD_HASH"), basePath)
	if err != nil {
		log.Fatalf("Failed to initialise auth: %v", err)
	}
	if authz.Disabled() {
		log.Println("WARNING: BUILDS_PASSWORD is not set — the UI and API are UNPROTECTED")
	} else {
		authz.RegisterRoutes(mux)
		log.Println("Authentication enabled")
	}

	server := &http.Server{Addr: addr, Handler: authz.Middleware(mux)}

	// Shut down cleanly on SIGINT/SIGTERM so in-flight builds are not left
	// stuck in "running".
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Println("Shutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	}()

	log.Printf("Builds server listening on %s", addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("Server failed: %v", err)
	}

	// ListenAndServe returns as soon as Shutdown closes the listener, so this
	// runs promptly on SIGTERM — well inside docker's stop grace period.
	// Stop the poller before the runner so no build is queued into a worker
	// that is already shutting down.
	pl.Stop()
	// Cancel any in-flight build. It is handed back to the queue (not failed)
	// and resumes after the restart; see Runner.runBuild's shutdown branch.
	r.Stop()
	log.Println("Shutdown complete")
}

// recoverOrphanedBuilds puts the queue back together after a restart:
// interrupted builds go back to pending (bounded by MaxBuildRequeues), the
// ones out of retries are failed, and everything pending is re-queued.
func recoverOrphanedBuilds(database *db.DB, buildCh chan *models.Build) {
	// Builds still marked running were killed mid-flight — SIGKILL after the
	// stop grace period, or a crash. Hand them back to the queue rather than
	// losing the work; this is the hard-kill counterpart to the graceful
	// requeue the runner does on SIGTERM. Rows over the requeue cap fall
	// through to FailStaleRunning below.
	if requeued, err := database.RequeueStaleRunning(models.MaxBuildRequeues); err != nil {
		log.Printf("Recovery: failed to re-queue interrupted builds: %v", err)
	} else {
		for _, id := range requeued {
			log.Printf("Recovery: re-queued build %d (interrupted by restart)", id)
		}
	}

	// finished_at stays NULL for interrupted builds — the real end time is
	// unknown, and stamping the restart time poisons history durations.
	interrupted, err := database.FailStaleRunning(0)
	if err != nil {
		log.Printf("Recovery: failed to sweep running builds: %v", err)
	}
	for _, id := range interrupted {
		log.Printf("Recovery: marked build %d failed (interrupted by restart)", id)
	}
	// One-time repair of rows swept by older code, which stamped finished_at
	// with the restart time.
	if n, err := database.RepairInterruptedDurations(); err == nil && n > 0 {
		log.Printf("Recovery: cleared bogus finish times on %d interrupted builds", n)
	}
	// Sweep builds left behind by project deletes that never cascaded (the
	// foreign-key pragma was silently off). Harmless once the cascade works.
	if n, err := database.DeleteOrphanedBuilds(); err != nil {
		log.Printf("Recovery: failed to delete orphaned builds: %v", err)
	} else if n > 0 {
		log.Printf("Recovery: deleted %d orphaned builds (project no longer exists)", n)
	}

	pending, err := database.ListBuildsByStatus(models.StatusPending)
	if err != nil {
		log.Printf("Recovery: failed to list pending builds: %v", err)
	}
	for i := range pending {
		b := &pending[i]
		select {
		case buildCh <- b:
			log.Printf("Recovery: re-queued pending build %d", b.ID)
		default:
			database.UpdateBuildStatus(b.ID, models.StatusFailed, b.Log+"\n[ERROR] Build not re-queued after restart: queue is full\n")
			log.Printf("Recovery: queue full, marked pending build %d failed", b.ID)
		}
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
