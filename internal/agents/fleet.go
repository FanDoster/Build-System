package agents

import (
	"regexp"
	"sort"
	"time"

	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/models"
)

// State is the headline badge. Reachability, admission and busyness are
// separate things underneath; this is the one-word summary of them for
// somebody glancing at the page.
type State string

const (
	StateBusy    State = "busy"    // running a build right now
	StateOnline  State = "online"  // here, and free
	StateOffline State = "offline" // not heard from within the tolerance
	StateWaiting State = "waiting" // server restarted; nothing has checked in yet
)

// stepTail is how much of a running build's log is read to find its current
// step. A step line is short and they arrive regularly; 64 KiB reaches back
// through even a very chatty compile.
const stepTail = 64 << 10

// stepRE matches the pinned log grammar the agent emits. Same shape as the
// web UI's parser — see internal/runner/runner.go.
var stepRE = regexp.MustCompile(`##\[step:([a-z]+)\] ?(.*)`)

// CurrentBuild is what an agent is doing now.
type CurrentBuild struct {
	ID          int64      `json:"id"`
	Project     string     `json:"project"`
	CommitSHA   string     `json:"commit_sha"`
	Step        string     `json:"step,omitempty"`
	StepDetail  string     `json:"step_detail,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	HeartbeatAt *time.Time `json:"heartbeat_at,omitempty"`
}

// RecentBuild is one line of history.
type RecentBuild struct {
	ID       int64  `json:"id"`
	Project  string `json:"project"`
	Status   string `json:"status"`
	Duration string `json:"duration,omitempty"`
}

// Agent is one machine as the page sees it.
type Agent struct {
	Name      string   `json:"name"`
	State     State    `json:"state"`
	Executors []string `json:"executors,omitempty"`
	// Scheme is how its last request reached us. "http" means the agent token
	// and any clone token it was handed crossed the network in clear.
	Scheme string `json:"scheme,omitempty"`
	// LastSeen and LastSeenFrom say when we last had evidence of this agent,
	// and what that evidence was. Both are shown: a badge nobody can check is
	// a badge nobody trusts at three in the morning.
	LastSeen     *time.Time    `json:"last_seen,omitempty"`
	LastSeenFrom string        `json:"last_seen_from,omitempty"`
	Polling      bool          `json:"polling"`
	Current      *CurrentBuild `json:"current,omitempty"`
	Recent       []RecentBuild `json:"recent,omitempty"`
	// ConsecutiveFailures counts reds from the newest build backwards. Three
	// in a row on one machine is the fastest signal available for "the box is
	// broken" rather than "the code is broken".
	ConsecutiveFailures int `json:"consecutive_failures,omitempty"`
	// Known is false for an agent seen only in build history — it has run
	// things here before but has not checked in since this server started.
	Known bool `json:"known"`
}

// Executor is a queue, and whether anything is serving it.
type Executor struct {
	Name          string     `json:"name"`
	Projects      []string   `json:"projects,omitempty"`
	Agents        []string   `json:"agents,omitempty"`
	Pending       int        `json:"pending"`
	OldestPending *time.Time `json:"oldest_pending,omitempty"`
	// Served is false when no live agent is asking for this executor. With
	// pending builds, that is the loudest thing on the page: it is what a
	// one-character typo in a project's executor looks like, and today it
	// produces a green agent, a build stuck forever, and no error anywhere.
	Served bool `json:"served"`
}

// Fleet is the whole page.
type Fleet struct {
	Agents        []Agent    `json:"agents"`
	Executors     []Executor `json:"executors"`
	ServerStarted time.Time  `json:"server_started"`
	Now           time.Time  `json:"now"`
}

// Source is the database surface Build needs, so tests need no SQLite.
type Source interface {
	AgentNames() ([]string, error)
	RunningBuildForAgent(agent string) (*models.Build, error)
	RecentBuildsForAgent(agent string, limit int) ([]models.Build, error)
	RemoteExecutors() ([]db.RemoteExecutor, error)
	LogTailBytes(id int64, n int) ([]byte, error)
}

// Build assembles the page from the registry and the database.
func Build(src Source, reg *Registry, now time.Time) (*Fleet, error) {
	f := &Fleet{ServerStarted: reg.StartedAt(), Now: now}

	sightings := reg.Snapshot()
	byName := make(map[string]*Agent, len(sightings))
	for _, s := range sightings {
		a := &Agent{
			Name:      s.Name,
			Executors: s.Executors,
			Scheme:    s.Scheme,
			Polling:   s.Polling > 0,
			Known:     true,
		}
		last := s.LastPoll
		a.LastSeen, a.LastSeenFrom = &last, "claim poll"
		byName[s.Name] = a
		f.Agents = append(f.Agents, *a)
	}

	// Agents in the history that have not checked in since this server
	// started. Worth listing: after a restart that is every one of them, and a
	// page that hid them would be empty exactly when someone is asking why
	// nothing is building.
	names, err := src.AgentNames()
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if _, ok := byName[name]; ok {
			continue
		}
		a := &Agent{Name: name}
		byName[name] = a
		f.Agents = append(f.Agents, *a)
	}

	// Fill each agent in, then decide its state.
	for i := range f.Agents {
		a := &f.Agents[i]
		if err := fill(src, a); err != nil {
			return nil, err
		}
		a.State = stateOf(a, reg.StartedAt(), now)
	}
	sort.Slice(f.Agents, func(i, j int) bool { return f.Agents[i].Name < f.Agents[j].Name })

	execs, err := src.RemoteExecutors()
	if err != nil {
		return nil, err
	}
	for _, e := range execs {
		out := Executor{Name: e.Name, Projects: e.Projects, Pending: e.Pending}
		if !e.OldestPending.IsZero() {
			t := e.OldestPending
			out.OldestPending = &t
		}
		for _, a := range f.Agents {
			if a.State == StateOffline {
				continue
			}
			for _, want := range a.Executors {
				if want == e.Name {
					out.Agents = append(out.Agents, a.Name)
					out.Served = true
				}
			}
		}
		f.Executors = append(f.Executors, out)
	}
	return f, nil
}

func fill(src Source, a *Agent) error {
	running, err := src.RunningBuildForAgent(a.Name)
	if err != nil {
		return err
	}
	if running != nil {
		cur := &CurrentBuild{
			ID:          running.ID,
			Project:     running.ProjectName,
			CommitSHA:   running.CommitSHA,
			StartedAt:   running.StartedAt,
			HeartbeatAt: running.LastHeartbeatAt,
		}
		cur.Step, cur.StepDetail = currentStep(src, running.ID)
		a.Current = cur

		// A running build's heartbeat is evidence of life, and better evidence
		// than a claim poll: an agent stops polling for the whole of a build,
		// so without this every busy agent would read as offline.
		if hb := running.LastHeartbeatAt; hb != nil {
			if a.LastSeen == nil || hb.After(*a.LastSeen) {
				a.LastSeen, a.LastSeenFrom = hb, "build heartbeat"
			}
		}
	}

	recent, err := src.RecentBuildsForAgent(a.Name, 10)
	if err != nil {
		return err
	}
	counting := true
	for _, b := range recent {
		a.Recent = append(a.Recent, RecentBuild{
			ID: b.ID, Project: b.ProjectName, Status: string(b.Status), Duration: b.Duration(),
		})
		if !counting {
			continue
		}
		switch b.Status {
		case models.StatusFailed:
			a.ConsecutiveFailures++
		case models.StatusPending, models.StatusRunning:
			// Not an outcome yet. Skipped rather than counted or treated as a
			// break: a build in flight is usually the retry of the very run
			// this counter exists to warn about, and letting it reset the count
			// hides the warning at the moment somebody is watching.
		default:
			counting = false // a success or a cancel ends the run
		}
	}
	return nil
}

// currentStep reads the tail of a running build's log for the last step
// boundary the agent emitted.
//
// Derived rather than reported: the agent already writes the pinned grammar and
// the server already stores every byte, so this needs no protocol field and
// works on builds that are in flight right now.
func currentStep(src Source, buildID int64) (step, detail string) {
	tail, err := src.LogTailBytes(buildID, stepTail)
	if err != nil || len(tail) == 0 {
		return "", ""
	}
	matches := stepRE.FindAllSubmatch(tail, -1)
	if len(matches) == 0 {
		return "", ""
	}
	last := matches[len(matches)-1]
	return string(last[1]), string(last[2])
}

// stateOf decides the headline badge.
//
// Liveness is a disjunction of three things and every term is load-bearing:
// a poll open right now, a recent poll, or a recent heartbeat on a build this
// agent owns. Drop the third and every busy agent reads as offline, because an
// agent does not poll while it builds.
func stateOf(a *Agent, floor, now time.Time) State {
	if a.Polling {
		if a.Current != nil {
			return StateBusy
		}
		return StateOnline
	}
	if a.LastSeen != nil && !models.AgentStale(a.LastSeen, time.Time{}, now) {
		if a.Current != nil {
			return StateBusy
		}
		return StateOnline
	}
	// Nothing recent. Immediately after a restart that says nothing about the
	// agent — it simply has not had a chance to poll yet — so the page says so
	// instead of painting it red.
	if !a.Known && now.Sub(floor) < models.AgentHeartbeatTTL {
		return StateWaiting
	}
	return StateOffline
}
