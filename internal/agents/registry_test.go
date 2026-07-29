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
