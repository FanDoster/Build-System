// Package live broadcasts dashboard state to the list pages.
//
// The unit of broadcast is a whole snapshot — the most recent builds, log-free
// and decorated with live progress — not a delta. Snapshots are idempotent, so
// a client that reconnects, misses a frame, or joins late converges on the
// right thing by applying whatever arrived last; there is no sequence to
// resume and nothing to replay. That is the opposite trade from internal/logbus,
// where a build log is an append-only byte stream and every byte matters.
//
// A single goroutine samples the DB (see Hub.Tick) and fans out only when the
// serialized snapshot actually changed, so N idle dashboards cost one query per
// tick and zero bytes on the wire. It runs only while someone is subscribed.
// Sampling rather than notifying on write is deliberate: builds are created by
// the API, the webhook, the poller and startup recovery, and their statuses
// move in the runner, the janitor and the DB layer. A sampler observes every
// one of those without a notification hook in each, and a missed hook would be
// invisible.
package live

import (
	"bytes"
	"encoding/json"
	"sync"
	"time"

	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/models"
)

const (
	// DefaultTick is the sampling interval. Elapsed time and progress bars are
	// animated client-side from absolute timestamps, so this only has to be
	// fast enough that a status change feels immediate.
	DefaultTick = time.Second
	// DefaultLimit is how many recent builds a snapshot carries. It covers the
	// dashboard's list with room for the project pages to filter by project.
	DefaultLimit = 20
	// subBuffer is the per-subscriber queue depth. A subscriber this far behind
	// is dropped and closed; the client reconnects and gets a fresh snapshot,
	// which is all it ever needed.
	subBuffer = 4
)

// Progresser reports the step a running build is on. runner.Runner implements
// it; nil is fine (the field is simply left empty).
type Progresser interface {
	Progress(buildID int64) (step string, ok bool)
}

// Message is the wire format. Type is always "builds" today and exists so a
// client can ignore frames it does not understand.
type Message struct {
	Type   string         `json:"type"`
	Builds []models.Build `json:"builds"`
}

type Hub struct {
	// Tick is the sampling interval; set before the first Subscribe.
	Tick time.Duration
	// Limit is how many recent builds a snapshot carries.
	Limit int

	db       *db.DB
	progress Progresser

	mu      sync.Mutex
	subs    map[chan []byte]struct{}
	last    []byte
	running bool
	stop    chan struct{}
}

func New(database *db.DB, progress Progresser) *Hub {
	return &Hub{
		Tick:     DefaultTick,
		Limit:    DefaultLimit,
		db:       database,
		progress: progress,
		subs:     make(map[chan []byte]struct{}),
	}
}

// Subscribe returns a channel of serialized snapshots and an unsubscribe func.
// The current snapshot is delivered immediately — either the cached one or the
// first sample of the loop this call starts — so a client never waits a tick
// to paint. The channel is closed if the subscriber falls behind; treat that
// as "reconnect".
func (h *Hub) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, subBuffer)

	h.mu.Lock()
	h.subs[ch] = struct{}{}
	if !h.running {
		// Drop any snapshot cached from a previous idle period: it is stale,
		// and leaving it would make the new loop's first sample compare equal
		// and skip the broadcast this subscriber is waiting for.
		h.last = nil
		h.running = true
		h.stop = make(chan struct{})
		go h.loop(h.stop)
	} else if h.last != nil {
		ch <- h.last // buffered and empty; cannot block
	}
	h.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if _, ok := h.subs[ch]; ok {
				delete(h.subs, ch)
				close(ch)
			}
			if len(h.subs) == 0 && h.running {
				close(h.stop)
				h.running = false
			}
		})
	}
}

// Snapshot samples the current dashboard state as a JSON Message. Exported so
// the same bytes can be served over a plain GET — the fallback for clients or
// proxies that cannot do WebSocket.
func (h *Hub) Snapshot() ([]byte, error) {
	builds, err := h.db.ListRecentBuildSummaries(h.limit())
	if err != nil {
		return nil, err
	}
	if builds == nil {
		builds = []models.Build{}
	}
	for i := range builds {
		Decorate(h.db, h.progress, &builds[i])
	}
	return json.Marshal(Message{Type: "builds", Builds: builds})
}

// Decorate fills the computed live-progress fields of an active build: its
// queue position, the step the runner is on, and the expected duration from
// recent history. A finished build is left alone.
func Decorate(database *db.DB, progress Progresser, b *models.Build) {
	if b.Status != models.StatusRunning && b.Status != models.StatusPending {
		return
	}
	if b.Status == models.StatusPending {
		if pos, err := database.QueuePosition(b.ID); err == nil {
			b.QueuePosition = pos
		}
	}
	if b.Status == models.StatusRunning && progress != nil {
		if step, ok := progress.Progress(b.ID); ok {
			b.CurrentStep = step
		}
	}
	if d, ok := database.ExpectedDuration(b.ProjectID); ok {
		b.ExpectedSecs = int64(d.Seconds() + 0.5)
	}
}

func (h *Hub) limit() int {
	if h.Limit <= 0 {
		return DefaultLimit
	}
	return h.Limit
}

func (h *Hub) loop(stop chan struct{}) {
	tick := h.Tick
	if tick <= 0 {
		tick = DefaultTick
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		// Sample first: the subscriber that started this loop is waiting.
		h.publish()
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

// publish samples and fans out, but only when the snapshot changed. A DB error
// is skipped rather than broadcast — clients keep the state they have and the
// next tick corrects them.
func (h *Hub) publish() {
	snap, err := h.Snapshot()
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if bytes.Equal(snap, h.last) {
		return
	}
	h.last = snap
	for ch := range h.subs {
		select {
		case ch <- snap:
		default:
			delete(h.subs, ch)
			close(ch)
		}
	}
}
