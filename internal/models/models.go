package models

import (
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

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

// Agent pause.
//
// Pause is the only control here that can stop CI, so every rule below is
// written to fail OPEN. An agent is paused only when a valid future instant
// says so; anything unclear — no row, no value, an unreadable value, an error
// on the way to finding out — means not paused. A bug that leaves an agent
// running when it should be idle is a nuisance; a bug that silently pauses the
// fleet is a dead CI that looks healthy, and nobody thinks to check a pause
// they did not set.
const (
	// MaxPauseDuration bounds how long one pause can last. The expiry is
	// mandatory rather than optional because the person who pauses to update
	// Unity is the same person who will forget, and an unexpiring pause is
	// indistinguishable from a broken agent until someone goes looking.
	MaxPauseDuration = 7 * 24 * time.Hour

	// MaxPauseNoteLen bounds the operator's note. It is displayed, not parsed.
	MaxPauseNoteLen = 200

	// MaxAgentNameLen bounds a name used as a primary key. Names are asserted
	// by whoever holds the agent token and are never validated against a
	// registration, so without a bound one request can write a 64 KiB key.
	MaxAgentNameLen = 64
)

// AgentPaused reports whether a pause is in force.
//
// nil, the zero time, and any instant already past all mean "not paused". This
// is the fail-open rule in code: there is no input to this function that turns
// an absent or malformed pause into a paused agent.
func AgentPaused(pausedUntil *time.Time, now time.Time) bool {
	return pausedUntil != nil && !pausedUntil.IsZero() && pausedUntil.After(now)
}

// ValidAgentName reports whether a name is safe to use as a database key and
// to render.
//
// Deliberately more permissive than executor names, which are restricted to
// lowercase [a-z0-9_-]. An executor name is typed by an operator into a project
// setting; an agent name arrives from a machine that is already deployed, and
// tightening the rule under a running fleet would turn a cosmetic naming choice
// into a claim that fails with 400 — CI stopped by a validation rule. So this
// rejects only what is genuinely dangerous or absurd: nothing, something too
// long to be a key, control characters, leading/trailing space that would
// silently split one machine's history in two, and path separators.
//
// The separators are the one rule here that is about somewhere else. An agent
// name is addressed as a path segment by the operator endpoints, and while Go's
// own router handles a percent-encoded slash correctly, nginx sits in front of
// this server in production and normalises `%2F` before Go ever sees the
// request — so `a/../claim` would arrive at a different endpoint than the one
// the operator clicked. Nothing legitimate has a slash in its name, so this
// costs nobody anything.
func ValidAgentName(name string) bool {
	if name == "" || len(name) > MaxAgentNameLen {
		return false
	}
	if strings.TrimSpace(name) != name {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	// "." and ".." are path segments with a meaning of their own. A browser
	// resolves them out of the URL before the request is sent, so an agent
	// named ".." could never be paused or forgotten from the page — the request
	// would arrive at a different route, or none.
	if name == "." || name == ".." {
		return false
	}
	// Ranging over a string decodes invalid bytes to U+FFFD, which is above the
	// control range and would pass the loop below. That matters here more than
	// it looks: this name is a primary key, a URL path segment, and a string
	// rendered on an operator's page, so it must be text before it is any of
	// those.
	if !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// What an agent reports about itself.
//
// Two channels, deliberately split. The small block below rides the claim poll,
// which fires twice a minute forever, so it stays a handful of scalars. The
// fuller AgentStatus goes to its own endpoint on the agent's own timer —
// explicitly NOT on the heartbeat, because the heartbeat only exists while a
// build is running and would deliver nothing while the agent is idle, which is
// exactly when somebody is asking what is wrong with it.
//
// Everything here is written by the machine being described, so every field is
// untrusted: bounded on arrival, escaped on the way out.
const (
	// MaxAgentVersionLen bounds the agent's self-reported version string.
	MaxAgentVersionLen = 64
	// MaxOSArchLen bounds the reported platform.
	MaxOSArchLen = 32
	// MaxStatusChecks, MaxStatusDetailLen and the rest bound one status report.
	// The agent caps its own payload too; these are what stops a hostile or
	// broken one filling the row that the page has to read on every poll.
	MaxStatusChecks     = 32
	MaxStatusDetailLen  = 500
	MaxStatusNameLen    = 64
	MaxStatusList       = 32
	MaxStatusWorkspaces = 50
	// MaxStatusBytes caps the stored JSON. Deliberately larger than the worst
	// case the field caps above can produce (32 checks x ~564 bytes is roughly
	// 18 KiB) — set it below that and a report that has already been clamped to
	// the documented limits still fails the byte check, and the fallback path
	// becomes the normal one. Still well under the 64 KiB request cap, so a
	// report is trimmed by the agent before it is sent rather than rejected at
	// the door: a 413 is answered identically forever, so an over-large report
	// would never arrive at all.
	MaxStatusBytes = 32 << 10
	// StatusStale is how old a status report may be before the page stops
	// presenting it as current. Comfortably more than the agent's reporting
	// interval, so an ordinary gap does not read as a problem.
	StatusStale = 20 * time.Minute
)

// AgentCheck is one of the agent's own self-checks, carried verbatim.
//
// Detail is shipped word for word rather than re-worded here: each one was
// written for the person who has to walk over to the machine, and carries its
// own remedy ("brew install git-lfs, and check the LaunchAgent's PATH includes
// /opt/homebrew/bin"). Rephrasing it server-side loses the fix.
type AgentCheck struct {
	Name          string `json:"name"`
	Detail        string `json:"detail"`
	OK            bool   `json:"ok"`
	NeedsOperator bool   `json:"needs_operator,omitempty"`
}

// AgentWorkspace is one checkout on the agent, with when it was last touched.
// Least recently used first — which is the order the agent's own sweep deletes
// them in, so the list doubles as a prediction.
type AgentWorkspace struct {
	Name string     `json:"name"`
	Used *time.Time `json:"used,omitempty"`
}

// AgentStatus is the fuller report.
type AgentStatus struct {
	Checks     []AgentCheck      `json:"checks,omitempty"`
	Unity      []string          `json:"unity,omitempty"`
	Tools      map[string]string `json:"tools,omitempty"`
	Workspaces []AgentWorkspace  `json:"workspaces,omitempty"`
	Timeouts   map[string]string `json:"timeouts,omitempty"`
}

// Clamp trims a report to the bounds above, in place.
//
// Trims rather than rejects. A report that is too long is still worth most of
// what it says, and refusing it outright would leave the page showing nothing
// at the moment the machine is least healthy.
func (s *AgentStatus) Clamp() {
	if len(s.Checks) > MaxStatusChecks {
		s.Checks = s.Checks[:MaxStatusChecks]
	}
	for i := range s.Checks {
		s.Checks[i].Name = ClampText(s.Checks[i].Name, MaxStatusNameLen)
		s.Checks[i].Detail = ClampText(s.Checks[i].Detail, MaxStatusDetailLen)
	}
	if len(s.Unity) > MaxStatusList {
		s.Unity = s.Unity[:MaxStatusList]
	}
	for i := range s.Unity {
		s.Unity[i] = ClampText(s.Unity[i], MaxStatusNameLen)
	}
	if len(s.Workspaces) > MaxStatusWorkspaces {
		s.Workspaces = s.Workspaces[:MaxStatusWorkspaces]
	}
	for i := range s.Workspaces {
		s.Workspaces[i].Name = ClampText(s.Workspaces[i].Name, MaxStatusNameLen)
	}
	s.Tools = clampMap(s.Tools, MaxStatusList, MaxStatusDetailLen)
	s.Timeouts = clampMap(s.Timeouts, MaxStatusList, MaxStatusNameLen)
}

// Problems returns the failing checks, the ones needing a person first.
//
// That order is the whole point of separating them: "Unity has no licence" and
// "git-lfs is missing" are both failures, but only one of them needs somebody
// to walk over to the machine and click something.
func (s *AgentStatus) Problems() []AgentCheck {
	if s == nil {
		return nil
	}
	var operator, other []AgentCheck
	for _, c := range s.Checks {
		if c.OK {
			continue
		}
		if c.NeedsOperator {
			operator = append(operator, c)
		} else {
			other = append(other, c)
		}
	}
	return append(operator, other...)
}

// ClampText bounds a string and strips what has no business being rendered:
// invalid UTF-8, and control characters that would break a line of the page.
//
// Trims on a rune boundary. A plain s[:n] splits the last character of any
// string containing an accent, and the invalid bytes survive into storage and
// back out onto the page.
func ClampText(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || (r < 0x20 && r != '\t') || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func clampMap(m map[string]string, maxKeys, maxVal int) map[string]string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // a map iterates at random; the page must not reshuffle
	if len(keys) > maxKeys {
		keys = keys[:maxKeys]
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[ClampText(k, MaxStatusNameLen)] = ClampText(m[k], maxVal)
	}
	return out
}
