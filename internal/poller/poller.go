// Package poller watches project repositories for new commits and queues
// builds, as an alternative to being pushed at by a GitHub webhook / Action.
//
// It is pull-based on purpose: nothing has to reach the build server, so it
// works for repos on hosts that can't send webhooks, for a server behind NAT,
// and for forges where configuring a webhook isn't an option. Webhooks stay
// available and the two can be enabled together — the poller checks whether a
// commit already has a build before queueing one, so the two triggers racing
// on the same push produce a single build.
package poller

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/models"
	"github.com/FanDoster/Build-System/internal/runner"
)

// tick is how often the poller wakes to look for projects that are due. It is
// the scheduling granularity, not the per-project interval — each project is
// polled no more often than its own PollInterval.
const tick = 10 * time.Second

// probeTimeout bounds a single `git ls-remote`. An unreachable host would
// otherwise stall the whole sweep behind TCP timeouts.
const probeTimeout = 30 * time.Second

// Poller periodically compares each polling-enabled project's remote branch
// tip against the last SHA it observed, and enqueues a build when it moves.
type Poller struct {
	DB      *db.DB
	BuildCh chan<- *models.Build

	// Tick overrides the sweep interval (tests use a short one).
	Tick time.Duration
	// LsRemote resolves a branch tip. Replaced in tests; defaults to git.
	LsRemote func(ctx context.Context, repoURL, branch string) (string, error)

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// due tracks the next poll time per project id, so changing a project's
	// interval takes effect without restarting the server.
	mu  sync.Mutex
	due map[int64]time.Time
}

func New(database *db.DB, buildCh chan<- *models.Build) *Poller {
	ctx, cancel := context.WithCancel(context.Background())
	return &Poller{
		DB:       database,
		BuildCh:  buildCh,
		Tick:     tick,
		LsRemote: gitLsRemote,
		ctx:      ctx,
		cancel:   cancel,
		due:      map[int64]time.Time{},
	}
}

func (p *Poller) Start() {
	p.wg.Add(1)
	go p.loop()
}

// Stop ends the sweep loop and waits for an in-flight sweep to finish.
func (p *Poller) Stop() {
	p.cancel()
	p.wg.Wait()
}

func (p *Poller) loop() {
	defer p.wg.Done()
	t := time.NewTicker(p.Tick)
	defer t.Stop()
	// Sweep once at startup so a commit pushed while the server was down is
	// picked up immediately rather than one interval later.
	p.Sweep()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-t.C:
			p.Sweep()
		}
	}
}

// Sweep polls every project that is enabled and due. Exported for tests.
func (p *Poller) Sweep() {
	projects, err := p.DB.ListProjects()
	if err != nil {
		log.Printf("Poller: failed to list projects: %v", err)
		return
	}

	live := make(map[int64]bool, len(projects))
	now := time.Now()
	for i := range projects {
		proj := &projects[i]
		live[proj.ID] = true
		if !proj.PollEnabled {
			continue
		}
		p.mu.Lock()
		next, seen := p.due[proj.ID]
		p.mu.Unlock()
		if seen && now.Before(next) {
			continue
		}
		p.mu.Lock()
		p.due[proj.ID] = now.Add(proj.PollInterval())
		p.mu.Unlock()

		if p.ctx.Err() != nil {
			return
		}
		p.pollProject(proj.ID)
	}

	// Drop schedule entries for deleted projects so the map can't grow without
	// bound over a long uptime.
	p.mu.Lock()
	for id := range p.due {
		if !live[id] {
			delete(p.due, id)
		}
	}
	p.mu.Unlock()
}

// pollProject runs one probe. It re-fetches the full row because ListProjects
// clears the clone token, which private repos need for ls-remote.
func (p *Poller) pollProject(id int64) {
	project, err := p.DB.GetProject(id)
	if err != nil {
		return
	}

	repoURL := project.RepoURL
	if project.CloneToken != "" {
		repoURL = runner.InjectToken(repoURL, project.CloneToken)
	}

	ctx, cancel := context.WithTimeout(p.ctx, probeTimeout)
	sha, err := p.LsRemote(ctx, repoURL, project.Branch)
	cancel()
	if err != nil {
		msg := runner.ScrubSecret(err.Error(), project.CloneToken)
		log.Printf("Poller: %s: %v", project.Name, msg)
		p.DB.UpdatePollState(project.ID, "", msg)
		return
	}

	// Store the abbreviated form the rest of the system uses, so the baseline
	// and a build's commit_sha are directly comparable.
	sha = short(sha)
	prev := project.LastPolledSHA

	// touch records the poll without moving the baseline off prev.
	touch := func(baseline string) {
		if err := p.DB.UpdatePollState(project.ID, baseline, ""); err != nil {
			log.Printf("Poller: %s: failed to record poll state: %v", project.Name, err)
		}
	}

	// First successful poll only seeds the baseline. Building here would mean
	// that merely enabling polling rebuilds whatever the branch already points
	// at — surprising, and a build storm if it were done for every project.
	if prev == "" {
		touch(sha)
		log.Printf("Poller: %s: watching %s at %s", project.Name, project.Branch, sha)
		return
	}
	if prev == sha {
		touch(sha)
		return
	}

	// Something else already built this exact commit — almost always the
	// webhook, which the poller races on every push when both are enabled.
	// Adopt the baseline: the commit is covered, and queueing here would
	// duplicate a build that is already done or in flight. This check must
	// come BEFORE the in-flight one below, which cannot tell whether the
	// running build is for this commit or an older one.
	if built, err := p.DB.HasBuildForCommit(project.ID, sha); err == nil && built {
		touch(sha)
		log.Printf("Poller: %s: %s already built by another trigger", project.Name, sha)
		return
	}

	// A build is already queued or running for this project. Do NOT advance the
	// baseline here: the in-flight build has its own commit pinned and will not
	// pick this one up, so moving the baseline would lose the commit entirely.
	// Leaving it put means the next sweep re-detects the same diff and builds
	// as soon as the queue is clear — one build for the newest tip, not one per
	// commit that landed while it ran.
	if active, err := p.DB.HasActiveBuild(project.ID); err == nil && active {
		touch(prev)
		log.Printf("Poller: %s: %s deferred — a build is already in flight", project.Name, sha)
		return
	}

	build := &models.Build{
		ProjectID: project.ID,
		Status:    models.StatusPending,
		// The tip is checked out by the clone itself; pinning the SHA here
		// also makes the runner's checkout step verify it.
		CommitSHA:     sha,
		CommitMessage: fmt.Sprintf("Polled: new commit on %s", project.Branch),
	}
	if err := p.DB.CreateBuild(build); err != nil {
		log.Printf("Poller: %s: failed to create build: %v", project.Name, err)
		return // baseline stays put, so the next sweep retries this commit
	}
	touch(sha)
	select {
	case p.BuildCh <- build:
		log.Printf("Poller: %s: queued build %d for %s", project.Name, build.ID, sha)
	default:
		p.DB.UpdateBuildStatus(build.ID, models.StatusFailed, "Build not started: queue is full\n")
		log.Printf("Poller: %s: queue full, dropped build %d", project.Name, build.ID)
	}
}

// gitLsRemote asks the remote for a single branch tip. No clone, no working
// tree — one cheap request per poll.
func gitLsRemote(ctx context.Context, repoURL, branch string) (string, error) {
	if branch == "" {
		branch = "main"
	}
	// "--" separates the URL from the ref pattern so neither can be read as an
	// option, whatever the project settings contain.
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--heads", "--", repoURL, "refs/heads/"+branch)
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -o StrictHostKeyChecking=no -o BatchMode=yes")
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("git ls-remote failed: %s", firstLine(detail))
	}

	// Output is "<sha>\trefs/heads/<branch>" per matching ref; --heads with an
	// exact refs/heads/<branch> pattern yields at most one.
	line := strings.TrimSpace(out.String())
	if line == "" {
		return "", fmt.Errorf("branch %q not found on remote", branch)
	}
	sha, _, ok := strings.Cut(firstLine(line), "\t")
	if !ok || sha == "" {
		return "", fmt.Errorf("unexpected ls-remote output: %q", firstLine(line))
	}
	return sha, nil
}

func firstLine(s string) string {
	return strings.TrimSpace(strings.SplitN(strings.TrimSpace(s), "\n", 2)[0])
}

// short abbreviates a SHA to the 12 characters the rest of the system stores.
func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
