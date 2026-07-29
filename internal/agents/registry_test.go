package agents

import (
	"sync"
	"testing"
	"time"
)

func TestPollIsCountedWhileOpenAndReleasedAfter(t *testing.T) {
	r := NewRegistry()

	done := r.PollStarted("mac", []string{"mac"}, "https")
	if s := r.Snapshot(); len(s) != 1 || s[0].Polling != 1 {
		t.Fatalf("snapshot = %+v, want one agent with an open poll", s)
	}
	done()
	if s := r.Snapshot(); s[0].Polling != 0 {
		t.Errorf("poll still counted as open after it finished: %+v", s[0])
	}
	// The poll ending is itself a sighting — the agent was there a moment ago
	// whether or not it got work.
	if s := r.Snapshot(); s[0].LastPoll.IsZero() {
		t.Error("no last-poll time recorded")
	}

	// Closing twice must not drive the count negative and make a live agent
	// look idle.
	done()
	if s := r.Snapshot(); s[0].Polling != 0 {
		t.Errorf("polling = %d after a double close, want 0", s[0].Polling)
	}
}

// Several agents, or one agent reconnecting, are ordinary. The count has to
// survive overlap or a busy moment would read as nobody home.
func TestConcurrentPollsAreCountedIndependently(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	release := make(chan struct{})

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done := r.PollStarted("mac", []string{"mac"}, "https")
			<-release
			done()
		}()
	}
	// Wait for all of them to be counted.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s := r.Snapshot(); len(s) == 1 && s[0].Polling == 20 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if s := r.Snapshot(); s[0].Polling != 20 {
		t.Fatalf("polling = %d, want 20", s[0].Polling)
	}
	close(release)
	wg.Wait()
	if s := r.Snapshot(); s[0].Polling != 0 {
		t.Errorf("polling = %d after all polls closed, want 0", s[0].Polling)
	}
}

func TestFirstSeenIsKeptAndExecutorsAreRefreshed(t *testing.T) {
	r := NewRegistry()
	r.PollStarted("mac", []string{"mac"}, "https")()
	first := r.Snapshot()[0].FirstSeen

	time.Sleep(2 * time.Millisecond)
	r.PollStarted("mac", []string{"mac", "ios"}, "https")()

	s := r.Snapshot()[0]
	if !s.FirstSeen.Equal(first) {
		t.Error("first-seen moved; it is the one field that should not")
	}
	if len(s.Executors) != 2 {
		t.Errorf("executors = %v, want the newly advertised pair", s.Executors)
	}
	if !s.LastPoll.After(first) {
		t.Error("last-poll did not advance")
	}
}

// A caller with no name is a malformed request, already rejected by the
// handler. It must not create a phantom row here either.
func TestAnEmptyNameIsIgnored(t *testing.T) {
	r := NewRegistry()
	r.PollStarted("", nil, "https")()
	if s := r.Snapshot(); len(s) != 0 {
		t.Errorf("recorded %d agents for an empty name", len(s))
	}
}

func TestForgetRemovesAnAgent(t *testing.T) {
	r := NewRegistry()
	r.PollStarted("typo", []string{"mac"}, "https")()
	r.Forget("typo")
	if s := r.Snapshot(); len(s) != 0 {
		t.Errorf("agent survived Forget: %+v", s)
	}
}

// The throttle is what makes a persisted sighting affordable. Without it this
// is one row write per poll per agent forever — the cost the in-memory registry
// was built to avoid.
func TestShouldPersistThrottlesPerAgent(t *testing.T) {
	r := NewRegistry()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	r.SetClock(func() time.Time { return now })

	r.PollStarted("mac", []string{"mac"}, "https")()
	if !r.ShouldPersist("mac", 10*time.Second) {
		t.Fatal("the first sighting was not persisted; last-seen would start out wrong")
	}
	if r.ShouldPersist("mac", 10*time.Second) {
		t.Error("persisted twice in the same instant")
	}

	now = now.Add(9 * time.Second)
	if r.ShouldPersist("mac", 10*time.Second) {
		t.Error("persisted before the interval elapsed")
	}
	now = now.Add(2 * time.Second)
	if !r.ShouldPersist("mac", 10*time.Second) {
		t.Error("did not persist after the interval elapsed")
	}

	// Throttled per agent, not globally — one chatty agent must not stop
	// another being recorded.
	r.PollStarted("mac-2", []string{"mac"}, "https")()
	if !r.ShouldPersist("mac-2", 10*time.Second) {
		t.Error("a second agent was throttled by the first")
	}
}

// An agent with no sighting has nothing to persist. Returning true would write
// a row for a name that never reached PollStarted.
func TestShouldPersistNeedsASighting(t *testing.T) {
	r := NewRegistry()
	if r.ShouldPersist("never-polled", time.Second) {
		t.Error("offered to persist an agent that has never been seen")
	}
	if r.ShouldPersist("", time.Second) {
		t.Error("offered to persist an empty name")
	}
}

// The claim handler is concurrent: two polls arriving together must not both
// decide they are due, or the throttle is not a throttle.
func TestShouldPersistIsAtomicUnderConcurrency(t *testing.T) {
	r := NewRegistry()
	r.PollStarted("mac", []string{"mac"}, "https")()

	var wg sync.WaitGroup
	var mu sync.Mutex
	yes := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r.ShouldPersist("mac", time.Hour) {
				mu.Lock()
				yes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if yes != 1 {
		t.Errorf("%d of 50 concurrent callers were told to persist, want exactly 1", yes)
	}
}

// Forget must clear the throttle too, or a re-registered agent waits out the
// old interval before its first row is written.
func TestForgetClearsTheThrottle(t *testing.T) {
	r := NewRegistry()
	r.PollStarted("mac", []string{"mac"}, "https")()
	if !r.ShouldPersist("mac", time.Hour) {
		t.Fatal("first persist refused")
	}
	r.Forget("mac")
	r.PollStarted("mac", []string{"mac"}, "https")()
	if !r.ShouldPersist("mac", time.Hour) {
		t.Error("after Forget, the agent had to wait out the old interval before being recorded again")
	}
}
