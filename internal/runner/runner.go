package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/logbus"
	"github.com/FanDoster/Build-System/internal/models"
)

const DefaultBuildTimeout = 30 * time.Minute

// ErrCanceledByUser is the cancellation cause set by Cancel so that a
// user-initiated cancel is distinguishable from timeouts and shutdown.
var ErrCanceledByUser = errors.New("canceled by user")

// LOG GRAMMAR — pinned contract between the runner and the web UI's parser
// (internal/web/static/js/app.js). Changing any of these requires updating
// both sides and internal/runner/testdata/log_fixture.txt:
//
//	step boundary: "[HH:MM:SS] ##[step:<id>] <detail>\n"  <id> ∈ clone|checkout|build|push|deploy
//	error:         "[ERROR] <msg>\n" (preceded by a blank line)
//	success:       "[HH:MM:SS] BUILD SUCCESS\n"
//	cancel:        "[HH:MM:SS] Build canceled by user (partial artifacts may remain)\n"

type Runner struct {
	DB      *db.DB
	Jobs    <-chan *models.Build
	Bus     *logbus.Bus
	Timeout time.Duration

	// NotifyEmail receives build-completion mail (BUILDS_NOTIFY_EMAIL).
	// Empty disables notifications entirely. See notify.go for the transport
	// and the server-side setup it depends on.
	NotifyEmail string
	// PublicURL is the externally reachable base of this server, e.g.
	// "https://fandoster.com/builds", used for the link in that mail. The
	// server cannot infer it — it only ever sees proxied requests — so when it
	// is unset the mail simply carries no link.
	PublicURL string
	// SMTPAddr overrides the relay. Exposed for tests; production uses the
	// host's Postfix over the Docker bridge.
	SMTPAddr string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// JanitorInterval is how often stale 'running' rows (left by crashes or
	// killed processes) are swept to failed. Exposed for tests.
	JanitorInterval time.Duration

	// startedAt is when this process came up. It is the floor the sweep
	// measures agent heartbeats from, so agents get a full grace period to
	// check back in after a restart rather than all looking dead at once
	// (they had nowhere to send heartbeats while the server was down).
	// Exposed via SetStartedAt for tests.
	startedAt time.Time

	mu            sync.Mutex
	currentID     int64
	currentStep   string
	cancelCurrent context.CancelCauseFunc
}

func New(database *db.DB, jobs <-chan *models.Build, bus *logbus.Bus) *Runner {
	ctx, cancel := context.WithCancelCause(context.Background())
	return &Runner{
		DB:              database,
		Jobs:            jobs,
		Bus:             bus,
		Timeout:         DefaultBuildTimeout,
		SMTPAddr:        defaultSMTPAddr,
		JanitorInterval: 30 * time.Second,
		startedAt:       time.Now(),
		ctx:             ctx,
		cancel:          func() { cancel(context.Canceled) },
	}
}

// SetStartedAt overrides the agent-heartbeat grace floor. Tests use it to make
// the sweep behave as though the process has been up for a while.
func (r *Runner) SetStartedAt(t time.Time) { r.startedAt = t }

func (r *Runner) Start() {
	r.wg.Add(2)
	go r.loop()
	go r.janitor()
}

// Stop cancels any in-flight build and waits for the worker to exit.
func (r *Runner) Stop() {
	r.cancel()
	r.wg.Wait()
}

// Cancel requests cancellation of the currently running build. Returns true
// iff id is the build the worker is processing right now.
func (r *Runner) Cancel(id int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.currentID == id && r.cancelCurrent != nil {
		r.cancelCurrent(ErrCanceledByUser)
		return true
	}
	return false
}

// Progress reports the step the worker is currently executing for build id.
// ok is false when id is not the build being processed right now.
func (r *Runner) Progress(id int64) (step string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.currentID == id {
		return r.currentStep, true
	}
	return "", false
}

func (r *Runner) setStep(step string) {
	r.mu.Lock()
	r.currentStep = step
	r.mu.Unlock()
}

// janitor periodically sweeps stale 'running' rows — builds a crashed or
// killed process left behind — so history never shows phantom running
// builds until the next restart. Single-worker invariant: any LOCAL running
// row that isn't the current build is stale. (Do not run two server processes
// against one DB; their janitors would fail each other's builds.)
//
// Builds claimed by a remote agent are the deliberate exception: they are
// running on another machine, so "not mine" says nothing about their health.
// Their liveness evidence is the heartbeat, and the sweep leaves them alone
// until it goes quiet — see db.FailStaleRunning.
func (r *Runner) janitor() {
	defer r.wg.Done()
	ticker := time.NewTicker(r.JanitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.SweepStale()
		}
	}
}

// SweepStale runs one janitor pass. Returns the ids it failed.
//
// The registry lock is held for the whole sweep: releasing it after reading
// currentID would let the worker register-and-claim a build inside the
// window, and the sweep would kill that live build. Holding mu serializes
// the sweep against registration — a build is either still pending (not
// swept) or already registered as current (excluded).
func (r *Runner) SweepStale() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	swept, err := r.DB.FailStaleRunning(r.currentID, r.startedAt)
	if err != nil {
		return nil
	}
	ids := make([]int64, 0, len(swept))
	for _, sb := range swept {
		// Mirror onto the bus the bytes the sweep just appended in SQL, so the
		// buffer and the stored log stay identical for anyone watching. This
		// matters for agent builds specifically: their topic lives in THIS
		// process even though the build does not, so without the mirror the
		// stream ends on a bare "failed" and the reader never learns why.
		//
		// Only when the topic already holds that build's log, though. Seeding
		// an absent or empty topic with just this note would leave the buffer
		// looking like a one-line log, and every reader prefers the buffer to
		// the row while it exists — which would hide the whole build. An empty
		// topic is the ordinary local-restart case, where the process that had
		// the log is gone and the DB is rightly the only copy.
		if _, cur, ok := r.Bus.LogTail(sb.ID, 0); ok && cur > 0 {
			r.Bus.Publish(sb.ID, []byte(sb.Note))
		}
		// Close any open topic/subscribers; finished time is unknown.
		r.Bus.PublishStatus(sb.ID, models.StatusFailed, nil, nil)
		ids = append(ids, sb.ID)
	}
	return ids
}

func (r *Runner) loop() {
	defer r.wg.Done()
	for {
		select {
		case <-r.ctx.Done():
			return
		case build := <-r.Jobs:
			r.process(build)
		}
	}
}

// process is one loop iteration. INVARIANT: the cancel registry is populated
// BEFORE ClaimBuild, so at every instant a cancel request lands either on the
// pending row (CancelPendingBuild) or on the registered context — never in a
// gap between the two.
func (r *Runner) process(build *models.Build) {
	ctx, cancelCause := context.WithCancelCause(r.ctx)
	defer cancelCause(nil)

	r.mu.Lock()
	r.currentID = build.ID
	r.cancelCurrent = cancelCause
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.currentID = 0
		r.currentStep = ""
		r.cancelCurrent = nil
		r.mu.Unlock()
	}()

	claimed, err := r.DB.ClaimBuild(build.ID)
	if err != nil || !claimed {
		// Canceled while queued (or already handled elsewhere) — skip.
		return
	}
	startedAt := time.Now().UTC()
	r.runBuild(ctx, build, startedAt)
}

func (r *Runner) runBuild(ctx context.Context, build *models.Build, startedAt time.Time) {
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	r.Bus.PublishStatus(build.ID, models.StatusRunning, &startedAt, nil)

	project, err := r.DB.GetProject(build.ProjectID)
	if err != nil {
		msg := "\n[ERROR] Error loading project: " + err.Error() + "\n"
		// Bus first, DB second — same order as the sink, so a subscriber's
		// DB-fallback replay can never double up with a queued publish.
		r.Bus.Publish(build.ID, []byte(msg))
		r.DB.AppendBuildLog(build.ID, msg)
		r.finish(build.ID, models.StatusFailed, startedAt)
		return
	}

	sink := newLogSink(build.ID, project.CloneToken, r.DB, r.Bus)
	defer sink.Close()

	logStep := func(msg string) {
		fmt.Fprintf(sink, "[%s] %s\n", time.Now().UTC().Format("15:04:05"), msg)
	}
	stepStart := func(id, detail string) {
		r.setStep(id)
		logStep("##[step:" + id + "] " + detail)
	}
	// fail terminates the build. The cancellation cause outranks whatever
	// error the killed command surfaced: a user cancel is not a failure, and
	// a server shutdown is not the build's fault at all — that one goes back
	// on the queue so a redeploy doesn't destroy work in progress.
	fail := func(msg string) {
		switch context.Cause(ctx) {
		case ErrCanceledByUser:
			logStep("Build canceled by user (partial artifacts may remain)")
			sink.Close()
			r.finish(build.ID, models.StatusCanceled, startedAt)
			return
		case context.Canceled:
			// Claim the requeue before writing anything: the sink drops
			// writes once closed, so the give-up path below needs the sink
			// still open.
			if ok, err := r.DB.RequeueBuild(build.ID, models.MaxBuildRequeues); err == nil && ok {
				// Through the sink, not straight to the row — the DB log and
				// the logbus buffer have to stay byte-identical.
				fmt.Fprint(sink, db.RequeueNote)
				sink.Close()
				// Not terminal: startup recovery re-queues pending rows, and
				// clients see it go back to pending rather than failed.
				r.Bus.PublishStatus(build.ID, models.StatusPending, nil, nil)
				return
			}
			// Out of retries (or no longer running) — fall through and record
			// a failure, so a build that takes the server down with it can't
			// be retried forever.
			msg += " (no re-queues left)"
		}
		fmt.Fprintf(sink, "\n[ERROR] %s\n", msg)
		sink.Close()
		r.finish(build.ID, models.StatusFailed, startedAt)
	}

	logStep(fmt.Sprintf("Starting build for project: %s", project.Name))
	logStep(fmt.Sprintf("Commit: %s — %s", build.CommitSHA, build.CommitMessage))

	// Create temp workdir
	workDir, err := os.MkdirTemp("", fmt.Sprintf("builds-%d-", build.ID))
	if err != nil {
		fail("Failed to create temp dir: " + err.Error())
		return
	}
	defer os.RemoveAll(workDir)

	logStep(fmt.Sprintf("Work dir: %s", workDir))

	// Validate Dockerfile path before doing any work.
	dockerfile, err := resolveDockerfile(workDir, project.DockerfilePath)
	if err != nil {
		fail(err.Error())
		return
	}

	// Step 1: Clone
	cloneURL := project.RepoURL
	if project.CloneToken != "" {
		cloneURL = injectToken(cloneURL, project.CloneToken)
	}

	stepStart("clone", fmt.Sprintf("Cloning %s (branch: %s)", project.RepoURL, project.Branch))
	cloneCmd := newCmd(ctx, sink, "git", "clone", "--depth", "1", "--branch", project.Branch, cloneURL, workDir)
	if err := cloneCmd.Run(); err != nil {
		fail(fmt.Sprintf("Git clone failed: %v%s", err, timeoutHint(ctx)))
		return
	}

	// Checkout specific commit if provided and not "manual"
	if build.CommitSHA != "" && build.CommitSHA != "manual" {
		stepStart("checkout", "Checking out "+build.CommitSHA)
		checkoutCmd := newCmd(ctx, sink, "git", "-C", workDir, "checkout", build.CommitSHA)
		if err := checkoutCmd.Run(); err != nil {
			if context.Cause(ctx) != nil {
				fail(fmt.Sprintf("Checkout failed: %v%s", err, timeoutHint(ctx)))
				return
			}
			logStep(fmt.Sprintf("Warning: checkout failed: %v (continuing with branch HEAD)", err))
		}
	}

	// A repository that uses LFS clones perfectly well without git-lfs present
	// — and lands every large file as a text pointer, which Docker builds into
	// an image that is wrong in a way nothing reports. The image ships, the
	// deploy succeeds, and the fault surfaces as a missing texture or a corrupt
	// binary much later and somewhere else.
	//
	// git-lfs is installed in the runtime image, so this should never fire. It
	// exists because the failure it guards is silent, and the cost of being
	// wrong about that is a green build that produced the wrong artifact.
	if usesLFS(workDir) && !haveGitLFS() {
		fail("This repository uses Git LFS and git-lfs is not installed in the build image. " +
			"Large files would be built as pointer files, producing an image that looks fine and is not. " +
			"Add git-lfs to the Dockerfile (apk add git-lfs && git lfs install --system).")
		return
	}

	// Step 2: Docker build
	imageTag := fmt.Sprintf("registry.fandoster.com/%s:latest", project.ImageName)

	// No --progress flag: legacy (non-BuildKit) docker rejects it with exit
	// 125, and BuildKit already falls back to plain progress when stdout is
	// not a TTY — which a pipe to the log sink never is.
	buildArgs := []string{"build", "-t", imageTag, "-f", dockerfile}
	if project.NoCache {
		// Force a clean rebuild — bypasses Docker's layer cache, which can
		// otherwise serve a stale image when a content change slips past its
		// cache heuristics.
		buildArgs = append(buildArgs, "--no-cache")
	}
	buildArgs = append(buildArgs, workDir)

	stepStart("build", "Building Docker image: "+imageTag)
	buildCmd := newCmd(ctx, sink, "docker", buildArgs...)
	if err := buildCmd.Run(); err != nil {
		fail(fmt.Sprintf("Docker build failed: %v%s", err, timeoutHint(ctx)))
		return
	}

	// Step 3: Docker push
	stepStart("push", "Pushing image: "+imageTag)
	pushCmd := newCmd(ctx, sink, "docker", "push", imageTag)
	if err := pushCmd.Run(); err != nil {
		fail(fmt.Sprintf("Docker push failed: %v%s", err, timeoutHint(ctx)))
		return
	}

	// Step 4: Deploy (if configured)
	if project.DeployComposePath != "" && project.DeployServiceName != "" {
		// An absolute compose path is a server-managed file (an admin
		// pre-places it, e.g. /opt/docker/<app>/docker-compose.yml). A
		// relative path is resolved against the cloned repo, letting a
		// project ship its own compose file and self-describe its deploy —
		// rejected if it escapes the checkout (same guard as the Dockerfile).
		composePath, err := resolveComposePath(workDir, project.DeployComposePath)
		if err != nil {
			fail(err.Error())
			return
		}
		// --wait blocks until the recreated service is running AND (if it
		// declares a healthcheck) healthy, so a crash-looping or unhealthy
		// image fails the deploy step visibly instead of reporting success the
		// moment the process starts. Services without a healthcheck still gate
		// only on "running" — unchanged behavior for other projects.
		stepStart("deploy", fmt.Sprintf("Deploying: docker compose -f %s up -d --wait %s", project.DeployComposePath, project.DeployServiceName))
		deployCmd := newCmd(ctx, sink, "docker", "compose", "-f", composePath, "up", "-d", "--pull", "always", "--wait", "--wait-timeout", "120", project.DeployServiceName)
		if err := deployCmd.Run(); err != nil {
			fail(fmt.Sprintf("Deploy failed: %v%s", err, timeoutHint(ctx)))
			return
		}
	} else {
		logStep("No deploy config — image pushed to registry. Watchtower will auto-deploy if watching.")
	}

	// Success!
	logStep("BUILD SUCCESS")
	sink.Close()
	r.finish(build.ID, models.StatusSuccess, startedAt)
}

// finish writes the terminal DB row and broadcasts the transition.
//
// This is the only place a build reaches a terminal state through the worker,
// which makes it the right hook for notifications. Two paths deliberately do
// not pass through here and so send no mail: a requeue after a restart (not
// terminal — the build is coming back), and the janitor's stale sweep (the
// build server was not running when that build died).
func (r *Runner) finish(buildID int64, status models.BuildStatus, startedAt time.Time) {
	r.DB.FinishBuild(buildID, status)
	finishedAt := time.Now().UTC()
	r.Bus.PublishStatus(buildID, status, &startedAt, &finishedAt)
	// Fire and forget: mail must never delay the worker picking up the next
	// build, and a dead relay must never turn a green build red.
	go r.notify(buildID, status, startedAt, finishedAt)
}

// timeoutHint annotates command failures caused by cancellation, which
// otherwise surface as an opaque "signal: killed".
func timeoutHint(ctx context.Context) string {
	switch context.Cause(ctx) {
	case context.DeadlineExceeded:
		return " (build timed out)"
	case ErrCanceledByUser:
		return " (canceled by user)"
	case context.Canceled:
		return " (build canceled by server shutdown)"
	}
	return ""
}

// resolveDockerfile joins the configured Dockerfile path with the work dir
// and rejects paths that escape the cloned repository.
func resolveDockerfile(workDir, path string) (string, error) {
	dockerfile := filepath.Join(workDir, path)
	rel, err := filepath.Rel(workDir, dockerfile)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("dockerfile path %q escapes the repository checkout", path)
	}
	return dockerfile, nil
}

// resolveComposePath decides where the deploy step reads its compose file
// from. An ABSOLUTE path is used as-is: a server-managed file an admin
// pre-places on the box (the original behavior). A RELATIVE path is resolved
// against the cloned repository so a project can ship its own compose file
// in-repo — rejected if it escapes the checkout. The repo checkout still
// exists at deploy time (its cleanup is deferred to build-function return,
// which happens after the detached `compose up` returns).
func resolveComposePath(workDir, path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	compose := filepath.Join(workDir, path)
	rel, err := filepath.Rel(workDir, compose)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("deploy compose path %q escapes the repository checkout", path)
	}
	return compose, nil
}

// InjectToken exposes injectToken to other packages: the poller builds the
// same authenticated URL for its `git ls-remote` probe.
func InjectToken(rawURL, token string) string { return injectToken(rawURL, token) }

// ScrubSecret exposes scrubSecret so poller errors persisted to the DB and
// shown in the UI can never carry a clone token.
func ScrubSecret(s, secret string) string { return scrubSecret(s, secret) }

// injectToken adds a credential to an HTTP(S) clone URL, percent-encoding it
// so tokens containing special characters survive.
//
// Both halves are the token deliberately. Git 2.45 and later require a
// username AND a password for HTTP Basic auth: a username-only URL makes git
// prompt for the password, and with GIT_TERMINAL_PROMPT=0 that surfaces as
//
//	fatal: could not read Password for 'https://***@github.com': terminal prompts disabled
//
// GitHub accepts a PAT in either position, so token:token authenticates
// without needing a per-forge username. Do not "simplify" this back to
// url.User.
func injectToken(rawURL, token string) string {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return rawURL
	}
	u.User = url.UserPassword(token, token)
	return u.String()
}

// scrubSecret masks a secret (and its percent-encoded form) in command output
// so it never lands in stored build logs.
func scrubSecret(s, secret string) string {
	if secret == "" {
		return s
	}
	s = strings.ReplaceAll(s, secret, "***")
	if enc := url.User(secret).String(); enc != secret {
		s = strings.ReplaceAll(s, enc, "***")
	}
	return s
}

func newCmd(ctx context.Context, sink *logSink, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -o StrictHostKeyChecking=no")
	// Identical writer for both streams: os/exec then serializes Writes on a
	// single pipe, preserving interleaving.
	cmd.Stdout = sink
	cmd.Stderr = sink
	// On cancel/timeout, kill the whole process group — child processes
	// (ssh under git, buildkit under docker) would otherwise survive and
	// hold the output pipe open, blocking Run until they exit. WaitDelay
	// bounds the pipe-wait as a backstop for detached grandchildren.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// usesLFS reports whether the checkout declares Git LFS filters.
//
// Reads .gitattributes rather than asking git-lfs, because the case this exists
// for is git-lfs being absent — a check that needs the missing tool to notice
// the tool is missing would never fire. Only the root file is read: that is
// where every real repository declares it, and a deeper one would already have
// been handled by the smudge filter when git-lfs is present, which is the only
// case where missing it matters.
func usesLFS(workDir string) bool {
	b, err := os.ReadFile(filepath.Join(workDir, ".gitattributes"))
	if err != nil {
		return false
	}
	return bytes.Contains(b, []byte("filter=lfs"))
}

// haveGitLFS reports whether git can run the lfs subcommand.
func haveGitLFS() bool {
	_, err := exec.LookPath("git-lfs")
	return err == nil
}
