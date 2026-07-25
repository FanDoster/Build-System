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

type Build struct {
	ID            int64       `json:"id"`
	ProjectID     int64       `json:"project_id"`
	ProjectName   string      `json:"project_name,omitempty"`
	Status        BuildStatus `json:"status"`
	CommitSHA     string      `json:"commit_sha"`
	CommitMessage string      `json:"commit_message"`
	Log           string      `json:"log"`
	StartedAt     *time.Time  `json:"started_at"`
	FinishedAt    *time.Time  `json:"finished_at"`
	CreatedAt     time.Time   `json:"created_at"`

	// Computed, never stored. Populated only for ?meta=1 and /builds/active
	// API responses.
	LogLen        int64  `json:"log_len,omitempty"`
	QueuePosition int    `json:"queue_position,omitempty"`
	CurrentStep   string `json:"current_step,omitempty"`
	ExpectedSecs  int64  `json:"expected_secs,omitempty"`
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
