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
	StatePaused  State = "paused"  // here, but deliberately not taking work
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
	ID        int64  `json:"id"`
	Project   string `json:"project"`
	CommitSHA string `json:"commit_sha"`
	Step      string `json:"step,omitempty"`
	// Executor is the queue this build came from — which is also the proof
	// that the agent running it serves that queue.
	Executor    string     `json:"executor,omitempty"`
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
	// Known is false for an agent seen only in build history or in the agents
	// table — it has been here before but has not checked in since THIS server
	// process started.
	//
	// A persisted row must never set this. It is what gates the post-restart
	// grace, and a redeploy takes longer than the staleness tolerance: mark a
	// remembered agent as known and the first minutes after every deploy show
	// it offline, which also drops it out of queue coverage and puts the
	// panel's loudest warning on screen for no reason.
	Known bool `json:"known"`

	// Paused, and the rest of this block, are the admission dimension: whether
	// the agent is ALLOWED to take work, which is independent of whether it is
	// reachable and of whether it is busy. Kept as their own fields rather than
	// folded into State — conflating the three is the mistake this design
	// specifically set out not to repeat, and State is only the headline.
	Paused      bool       `json:"paused,omitempty"`
	PausedUntil *time.Time `json:"paused_until,omitempty"`
	PauseNote   string     `json:"pause_note,omitempty"`

	// FirstSeen is when this agent first ever contacted this server, from the
	// persisted row. Nil for an agent known only from build history.
	FirstSeen *time.Time `json:"first_seen,omitempty"`

	// Remembered is true when the agent has a persisted row. It separates "we
	// have never heard of this machine" from "we know it, it is just not here".
	Remembered bool `json:"remembered,omitempty"`
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

	// AllPaused means the queue has agents but every one of them is paused, so
	// nothing will be claimed from it.
	//
	// Deliberately a separate signal rather than clearing Served. "No agent
	// serving this" means a name nothing answers to — a typo, needing a config
	// change. "Served, but paused" means a decision somebody made and can undo.
	// Collapsing them would send an operator hunting for a misspelling they
	// never made, and would also un-cover a queue an agent is at that moment
	// building from.
	AllPaused bool `json:"all_paused,omitempty"`
}

// Fleet is the whole page.
type Fleet struct {
	Agents        []Agent    `json:"agents"`
	Executors     []Executor `json:"executors"`
	ServerStarted time.Time  `json:"server_started"`
	Now           time.Time  `json:"now"`
	// Degraded is set when part of the page could not be assembled but the
	// rest still can. Shown rather than swallowed: a page quietly missing its
	// pause state would be read as "nothing is paused".
	Degraded string `json:"degraded,omitempty"`
}

// Source is the database surface Build needs, so tests need no SQLite.
type Source interface {
	AgentNames() ([]string, error)
	RunningBuildForAgent(agent string) (*models.Build, error)
	RecentBuildsForAgent(agent string, limit int) ([]models.Build, error)
	RemoteExecutors() ([]db.RemoteExecutor, error)
	LogTailBytes(id int64, n int) ([]byte, error)
	ListAgentRows() ([]db.AgentRow, error)
}

// Build assembles the page from the registry and the database.
//
// Three sources name agents and they overlap: the in-memory registry (here
// now), the agents table (here before, and where pause lives), and the builds
// table (has built here at some point). Each is merged into one record per
// name — an agent that appears twice on this page reads as two machines, and
// the whole point of the page is knowing how many there are.
func Build(src Source, reg *Registry, now time.Time) (*Fleet, error) {
	f := &Fleet{ServerStarted: reg.StartedAt(), Now: now}

	var order []*Agent
	byName := map[string]*Agent{}
	at := func(name string) *Agent {
		if a, ok := byName[name]; ok {
			return a
		}
		a := &Agent{Name: name}
		byName[name] = a
		order = append(order, a)
		return a
	}

	// 1. Live sightings. The only source that can say "polling right now".
	for _, s := range reg.Snapshot() {
		a := at(s.Name)
		a.Executors = s.Executors
		a.Scheme = s.Scheme
		a.Polling = s.Polling > 0
		a.Known = true
		last := s.LastPoll
		a.LastSeen, a.LastSeenFrom = &last, "claim poll"
	}

	// 2. Remembered agents. Where pause comes from, and where last-seen comes
	// from for an agent that has not polled since this process started.
	//
	// Read failing open. A scan error here must not take down the page: the
	// coverage panel is what an operator opens when nothing is building, and
	// losing it because one row would not parse is the worst possible moment.
	// Degrading to A1's behaviour beats a 500.
	rows, rowsErr := src.ListAgentRows()
	if rowsErr != nil {
		f.Degraded = "agent records could not be read; showing live state only"
		rows = nil
	}
	for i := range rows {
		applyRow(at(rows[i].Name), &rows[i], now)
	}

	// 3. Agents in the build history. After a restart that may be all of them,
	// and a page that hid them would be empty exactly when someone is asking
	// why nothing is building.
	names, err := src.AgentNames()
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		at(name)
	}

	// Fill each agent in, then decide its state.
	for _, a := range order {
		if err := fill(src, a); err != nil {
			return nil, err
		}
		a.State = stateOf(a, reg.StartedAt(), now)
	}
	sort.Slice(order, func(i, j int) bool { return order[i].Name < order[j].Name })
	f.Agents = make([]Agent, 0, len(order))
	for _, a := range order {
		f.Agents = append(f.Agents, *a)
	}

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
		live := 0
		for _, a := range f.Agents {
			if a.State == StateOffline {
				continue
			}
			if !contains(a.Executors, e.Name) {
				continue
			}
			out.Agents = append(out.Agents, a.Name)
			out.Served = true
			if !a.Paused {
				live++
			}
		}
		// Every agent on this queue is paused, so nothing will be claimed from
		// it. Said separately from "no agent serving this", which means a name
		// nothing answers to and needs a config change rather than a resume.
		out.AllPaused = out.Served && live == 0
		f.Executors = append(f.Executors, out)
	}
	return f, nil
}

// applyRow folds a persisted agent record into the view.
//
// Note what this does NOT set: Known. That flag means "has checked in since
// this process started", and it gates the post-restart grace period. A redeploy
// of this server takes about two minutes — longer than the staleness tolerance
// — so if a remembered agent counted as known, every deploy would show the
// fleet offline, drop those agents out of queue coverage, and put the panel's
// loudest warning on screen for no reason at all.
func applyRow(a *Agent, row *db.AgentRow, now time.Time) {
	a.Remembered = true
	a.FirstSeen = row.FirstSeenAt
	a.Paused = row.Paused(now)
	if a.Paused {
		a.PausedUntil = row.PausedUntil
		a.PauseNote = row.PauseNote
	}
	// The live sighting wins on both of these: it describes the connection that
	// is open now, while the row describes whatever the last stored one was.
	if len(a.Executors) == 0 {
		a.Executors = row.Executors
	}
	if a.Scheme == "" {
		a.Scheme = row.LastScheme
	}
	// Only when there is no live sighting at all. The stored timestamp is
	// written on the very poll the registry also records, a few microseconds
	// later, so comparing the two and taking the newer relabels every live
	// agent as one last seen before a restart — which is what this did on its
	// first run against a real server. The registry is never behind: it is
	// updated on every poll, the row only once per throttle interval.
	if a.LastSeen == nil && row.LastSeenAt != nil {
		a.LastSeen, a.LastSeenFrom = row.LastSeenAt, "last poll before restart"
	}
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
		cur.Executor = running.Executor
		a.Current = cur

		// A build in flight is proof the agent serves the queue it came from —
		// better proof than the advertised list, which is only what the agent
		// last claimed to want.
		//
		// This is not a nicety. The advertised list lives in the registry, and
		// an agent that is building has not polled since the server started, so
		// for a busy agent the list is empty. Without this the coverage panel
		// calls a queue unserved while an agent is actively building from it,
		// which is the loudest warning on the page firing at the one moment it
		// is wrong. Seen in production the first time this shipped.
		if models.Remote(running.Executor) && !contains(a.Executors, running.Executor) {
			a.Executors = append(a.Executors, running.Executor)
		}

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

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
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
	// The headline for a reachable agent, in the order an operator cares.
	// Busy outranks paused: a paused agent that is mid-build really is
	// building, and hiding that behind "paused" would make the running build
	// invisible on the one page that lists what each machine is doing. The
	// pause is still shown — it is its own field, and the row says both.
	here := func() State {
		if a.Current != nil {
			return StateBusy
		}
		if a.Paused {
			return StatePaused
		}
		return StateOnline
	}
	if a.Polling {
		return here()
	}
	if a.LastSeen != nil && !models.AgentStale(a.LastSeen, time.Time{}, now) {
		return here()
	}
	// Nothing recent. Immediately after a restart that says nothing about the
	// agent — it simply has not had a chance to poll yet — so the page says so
	// instead of painting it red.
	if !a.Known && now.Sub(floor) < models.AgentHeartbeatTTL {
		return StateWaiting
	}
	return StateOffline
}
