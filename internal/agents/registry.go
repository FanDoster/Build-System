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
// Kept in memory on purpose. The alternative is a row written twice a minute
// per agent forever, and the thing it would buy — knowing on Tuesday when an
// agent was last seen on Monday — is worth less than not having that write.
// A restart therefore starts from nothing, which is handled explicitly rather
// than reported as a fleet of dead machines: see Registry.StartedAt.
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
