// Package agents answers "which machines are building things, and are they
// alive".
//
// It exists because of one property of the protocol: an idle agent writes
// nothing to the database. db.ClaimBuildForAgent performs no UPDATE when there
// is nothing to claim, so an agent that has been polling healthily for an hour
// is, in the builds table, indistinguishable from one that was unplugged an
// hour ago. The only signal an idle agent produces is the claim request
// itself, and this is where that signal is kept.
//
// This registry is the LIVE half of that answer and is kept in memory: it is
// the only thing that can say "an open claim is being held right now", it costs
// nothing per poll, and it is rebuilt within one poll cycle of a restart.
//
// The durable half lives in the agents table (internal/db/agentstore.go). That
// was originally rejected — a row written twice a minute per agent forever, to
// learn on Tuesday when an agent was last seen on Monday, was not worth the
// write — and two things overturned it. A pause has to survive a redeploy, or
// deploying the server silently resumes every paused machine. And last-seen has
// to outlive the process, or the minutes after each deploy show a fleet that
// has never been seen. The write is made affordable by throttling it to once
// per AgentSightingInterval per agent rather than once per poll.
//
// A restart still starts this registry from nothing, which is handled
// explicitly rather than reported as a fleet of dead machines: see
// Registry.StartedAt, and the Known flag in fleet.go.
package agents

import (
	"sort"
	"sync"
	"time"
)

// Sighting is what the server has observed of one agent since it started.
type Sighting struct {
	Name      string
	Executors []string
	// Scheme is how the agent's last request reached us — "https", or "http"
	// if it crossed the network in clear, which is worth shouting about.
	Scheme    string
	FirstSeen time.Time
	LastPoll  time.Time
	// Polling counts claim requests open right now. A held long-poll is the
	// strongest liveness evidence available: the agent is not merely known to
	// have existed recently, it is on the other end of an open socket.
	Polling int

	// Skew is how far the agent's clock is from ours, and SkewKnown says
	// whether it was ever reported. Kept here rather than in the agents table
	// for the same reason the rest of this is: a clock reading is only
	// meaningful while the agent is live, and it is re-measured within one poll
	// of a restart. A column would have to be aged, and would outlive the fact.
	Skew      time.Duration
	SkewKnown bool

	// DoubleSince and LastDouble bracket the period over which this name has
	// had more than one claim open at once. Two live processes under one name
	// is the duplicate case: ownership is name equality, so either can write
	// into the other's build.
	//
	// Two instants rather than one flag, because neither alone works. A single
	// agent can briefly overlap two polls — a retry landing on a fresh
	// connection — so an instantaneous flag cries wolf. But two real agents
	// poll in cycles and BOTH close between them, so a rule that reset on every
	// gap would never fire at all: the span, not the continuity, is the
	// evidence.
	DoubleSince time.Time
	LastDouble  time.Time

	// lastPersist is when this sighting was last written to the database.
	// Unexported: it is throttle bookkeeping, not something a caller reports.
	lastPersist time.Time
}

type Registry struct {
	mu      sync.Mutex
	seen    map[string]*Sighting
	started time.Time
	now     func() time.Time
}

func NewRegistry() *Registry {
	return &Registry{
		seen:    make(map[string]*Sighting),
		started: time.Now(),
		now:     time.Now,
	}
}

// SetClock replaces the time source. Tests only.
func (r *Registry) SetClock(now func() time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = now
	r.started = now()
}

// StartedAt is when this process began.
//
// It is the floor for every staleness judgement, exactly as models.AgentStale
// uses it: immediately after a restart the registry is empty through no fault
// of any agent, and a page that painted the whole fleet red would be
// technically defensible and practically a lie.
func (r *Registry) StartedAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started
}

// PollStarted records an agent asking for work and returns the function that
// records the request ending. Call it for EVERY claim, including the ones that
// find nothing — an empty poll is the only thing an idle agent ever does.
func (r *Registry) PollStarted(name string, executors []string, scheme string) (done func()) {
	if name == "" {
		return func() {}
	}
	r.mu.Lock()
	now := r.now()
	s, ok := r.seen[name]
	if !ok {
		s = &Sighting{Name: name, FirstSeen: now}
		r.seen[name] = s
	}
	s.Executors = append([]string(nil), executors...)
	s.Scheme = scheme
	s.LastPoll = now
	s.Polling++
	if s.Polling > 1 {
		// Forget an old episode before starting a new one, so a machine that
		// was briefly duplicated last week does not stay flagged forever.
		if !s.LastDouble.IsZero() && now.Sub(s.LastDouble) > DuplicateForget {
			s.DoubleSince = time.Time{}
		}
		if s.DoubleSince.IsZero() {
			s.DoubleSince = now
		}
		s.LastDouble = now
	}
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if cur, ok := r.seen[name]; ok {
				if cur.Polling > 0 {
					cur.Polling--
				}
				// The poll ending is itself a sighting: the agent was there a
				// moment ago, whether it got work or not.
				cur.LastPoll = r.now()
			}
		})
	}
}

// ShouldPersist reports whether this agent's sighting is due to be written to
// the database, and stamps it as written if so.
//
// The check and the stamp are one atomic step under the registry's own lock,
// because the claim handler is concurrent: two polls arriving together must not
// both decide they are due. The caller does the actual write AFTER this
// returns, never while holding a lock — a database call under a mutex that
// every claim contends on would turn one slow write into a stalled fleet.
//
// The throttle is what makes a persisted sighting affordable at all. Without
// it this is a row written twice a minute per agent forever, which is the cost
// the in-memory registry was built to avoid.
func (r *Registry) ShouldPersist(name string, interval time.Duration) bool {
	if name == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.seen[name]
	if !ok {
		return false // no sighting to persist; PollStarted has not run
	}
	now := r.now()
	if !s.lastPersist.IsZero() && now.Sub(s.lastPersist) < interval {
		return false
	}
	s.lastPersist = now
	return true
}

// NoteClock records how far an agent's clock is from this server's.
//
// Measured here, at the moment the request arrives, because that is the only
// point where both clocks are readable at once. The figure includes the network
// latency of the request that carried it — a few milliseconds on this path, and
// nothing next to a skew worth telling anybody about.
func (r *Registry) NoteClock(name string, agentClock time.Time) {
	if name == "" || agentClock.IsZero() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.seen[name]
	if !ok {
		return
	}
	s.Skew = agentClock.Sub(r.now())
	s.SkewKnown = true
}

// Snapshot copies what is known, newest contact first.
func (r *Registry) Snapshot() []Sighting {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Sighting, 0, len(r.seen))
	for _, s := range r.seen {
		c := *s
		c.Executors = append([]string(nil), s.Executors...)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastPoll.After(out[j].LastPoll) })
	return out
}

// Forget drops an agent, for a name that was a typo and will never return.
func (r *Registry) Forget(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.seen, name)
}
