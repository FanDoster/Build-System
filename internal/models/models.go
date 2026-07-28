package models

import "time"

type BuildStatus string

const (
	StatusPending  BuildStatus = "pending"
	StatusRunning  BuildStatus = "running"
	StatusSuccess  BuildStatus = "success"
	StatusFailed   BuildStatus = "failed"
	StatusCanceled BuildStatus = "canceled"
)

// Terminal reports whether the status is a final state.
func (s BuildStatus) Terminal() bool {
	return s == StatusSuccess || s == StatusFailed || s == StatusCanceled
}

// ExecutorLocal names the built-in Docker runner — the single in-process
// worker that has always run every build. Any other value is a queue name a
// remote build agent serves: those builds are never put on the local channel,
// their pending row IS the queue, and an agent claims it over HTTP.
const ExecutorLocal = "local"

// Remote reports whether an executor value routes builds to an agent rather
// than the built-in runner. Empty counts as local — every project predating
// the column has no executor set.
func Remote(executor string) bool {
	return executor != "" && executor != ExecutorLocal
}

type Project struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	RepoURL           string `json:"repo_url"`
	Branch            string `json:"branch"`
	DockerfilePath    string `json:"dockerfile_path"`
	ImageName         string `json:"image_name"`
	DeployComposePath string `json:"deploy_compose_path,omitempty"`
	DeployServiceName string `json:"deploy_service_name,omitempty"`
	WebhookSecret     string `json:"webhook_secret,omitempty"`
	CloneToken        string `json:"clone_token,omitempty"`
	NoCache           bool   `json:"no_cache"`
	// Executor decides who runs this project's builds: ExecutorLocal (the
	// Docker runner in this process) or the name of a queue an agent serves,
	// e.g. "mac".
	Executor string `json:"executor"`

	// Polling: an alternative to GitHub Actions / webhooks. When enabled the
	// server asks the remote for the branch tip every PollIntervalSecs and
	// queues a build when the SHA moves. LastPolledSHA is the tip the poller
	// last observed — it is seeded (without building) on the first successful
	// poll so enabling polling never fires a build for history that already
	// existed.
	PollEnabled      bool       `json:"poll_enabled"`
	PollIntervalSecs int        `json:"poll_interval_secs"`
	LastPolledSHA    string     `json:"last_polled_sha,omitempty"`
	LastPolledAt     *time.Time `json:"last_polled_at,omitempty"`
	LastPollError    string     `json:"last_poll_error,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// MinPollIntervalSecs floors the poll interval. Every poll is a network round
// trip to the forge; anything tighter is rate-limit bait for no real gain.
const MinPollIntervalSecs = 30

// DefaultPollIntervalSecs is used when polling is enabled without an interval.
const DefaultPollIntervalSecs = 60

// PollInterval returns the effective, floored poll interval.
func (p *Project) PollInterval() time.Duration {
	secs := p.PollIntervalSecs
	if secs <= 0 {
		secs = DefaultPollIntervalSecs
	}
	if secs < MinPollIntervalSecs {
		secs = MinPollIntervalSecs
	}
	return time.Duration(secs) * time.Second
}

// Sanitize clears sensitive fields for API responses.
func (p *Project) Sanitize() {
	p.WebhookSecret = ""
	p.CloneToken = ""
}

// MaxBuildRequeues bounds how many times one build may be handed back to the
// queue after a server restart interrupted it. Without a bound, a build that
// takes the server down with it (an OOM, say) would be retried on every boot
// forever. Two is enough for the case this exists for — a redeploy killing an
// in-flight build — while still converging on a real failure.
const MaxBuildRequeues = 2

type Build struct {
	ID            int64       `json:"id"`
	ProjectID     int64       `json:"project_id"`
	ProjectName   string      `json:"project_name,omitempty"`
	Status        BuildStatus `json:"status"`
	CommitSHA     string      `json:"commit_sha"`
	CommitMessage string      `json:"commit_message"`
	Log           string      `json:"log"`
	// Requeues counts restarts this build survived. Non-zero means an earlier
	// attempt's output is in the log above the restart marker.
	Requeues   int        `json:"requeues,omitempty"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	CreatedAt  time.Time  `json:"created_at"`

	// Agent names the build agent that claimed this build; empty means the
	// local Docker runner owns it. LastHeartbeatAt is when that agent last
	// reported in — the janitor's only evidence it is still alive.
	// CancelRequested is how a cancel reaches an out-of-process executor:
	// nothing can call into an agent, so it reads the flag off its own next
	// heartbeat or log-append response.
	Agent           string     `json:"agent,omitempty"`
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at,omitempty"`
	CancelRequested bool       `json:"cancel_requested,omitempty"`

	// Executor is the owning project's executor, populated by the JOIN in the
	// same way as ProjectName. Never stored on the build row.
	Executor string `json:"executor,omitempty"`

	// Computed, never stored. Populated only for ?meta=1 and /builds/active
	// API responses.
	LogLen        int64  `json:"log_len,omitempty"`
	QueuePosition int    `json:"queue_position,omitempty"`
	CurrentStep   string `json:"current_step,omitempty"`
	ExpectedSecs  int64  `json:"expected_secs,omitempty"`
}

// AgentHeartbeatTTL is how long a build claimed by an agent may go without a
// heartbeat before its agent is presumed dead and the build is failed. Three
// missed beats at the agent's 20s interval, plus margin for a slow network.
const AgentHeartbeatTTL = 90 * time.Second

// AgentStale reports whether an agent-owned running build has gone quiet long
// enough to presume its agent is gone.
//
// floor is the earliest instant an agent could have reported in to THIS server
// process — its start time. It matters because after a restart every heartbeat
// in the DB is old through no fault of the agent: it had nowhere to send them
// while the server was down. Measuring from the floor gives every agent a full
// TTL to check back in before its build is failed, which is what lets a live
// agent build survive a redeploy. In steady state the stored heartbeat is
// newer than the floor and dominates.
func AgentStale(lastHeartbeat *time.Time, floor, now time.Time) bool {
	last := floor
	if lastHeartbeat != nil && lastHeartbeat.After(last) {
		last = *lastHeartbeat
	}
	return now.Sub(last) >= AgentHeartbeatTTL
}

// Duration returns a human-readable build duration, or "" if the build
// hasn't both started and finished. Value receiver so it is callable on
// range variables in templates.
func (b Build) Duration() string {
	if b.StartedAt == nil || b.FinishedAt == nil {
		return ""
	}
	return b.FinishedAt.Sub(*b.StartedAt).Round(time.Second).String()
}
