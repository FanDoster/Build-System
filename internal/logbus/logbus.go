// Package logbus is an in-memory pub/sub hub for live build logs.
//
// Each active build has a topic holding the full scrubbed log produced since
// the build was claimed, plus a set of subscribers. Subscribe atomically
// returns a snapshot and a live channel under one lock, so a reader can never
// miss or duplicate bytes between replay and streaming. The DB row is the
// durable copy; the topic buffer exactly mirrors the bytes appended to it.
package logbus

import (
	"sync"
	"time"

	"github.com/FanDoster/Build-System/internal/models"
)

// RetainAfterClose is how long a finished build's buffer stays available for
// late reconnects before the topic is dropped.
const RetainAfterClose = 30 * time.Second

// subBuffer is the per-subscriber channel capacity. A subscriber that falls
// this far behind is dropped (closed); clients resume via ?offset replay.
const subBuffer = 64

type Event struct {
	Kind       string // "log" or "status"
	Chunk      string // Kind=="log": the appended bytes
	Offset     int    // Kind=="log": total buffer length after this chunk
	Status     models.BuildStatus
	StartedAt  *time.Time
	FinishedAt *time.Time
}

type topic struct {
	buf    []byte
	subs   map[chan Event]struct{}
	closed bool
}

type Bus struct {
	mu     sync.Mutex
	topics map[int64]*topic
}

func New() *Bus {
	return &Bus{topics: make(map[int64]*topic)}
}

func (b *Bus) topicLocked(id int64) *topic {
	t, ok := b.topics[id]
	if !ok {
		t = &topic{subs: make(map[chan Event]struct{})}
		b.topics[id] = t
	}
	return t
}

// Subscribe returns the buffered log from byte offset `from`, the current
// total offset, a live event channel, and an unsubscribe func. Safe to call
// for builds with no topic yet (e.g. still pending): an empty topic is
// created so no events are missed. If the topic is already closed the
// returned channel is closed immediately after any snapshot.
func (b *Bus) Subscribe(id int64, from int) (snapshot []byte, cur int, ch <-chan Event, unsub func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	t := b.topicLocked(id)
	if from < 0 {
		from = 0
	}
	if from > len(t.buf) {
		from = len(t.buf)
	}
	snapshot = append([]byte(nil), t.buf[from:]...)
	cur = len(t.buf)

	c := make(chan Event, subBuffer)
	if t.closed {
		close(c)
		return snapshot, cur, c, func() {}
	}
	t.subs[c] = struct{}{}

	unsub = func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := t.subs[c]; ok {
			delete(t.subs, c)
			close(c)
		}
	}
	return snapshot, cur, c, unsub
}

// Publish appends chunk to the build's buffer and fans it out. Slow
// subscribers (full channel) are dropped and closed; they can resume from
// their last offset.
func (b *Bus) Publish(id int64, chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	t := b.topicLocked(id)
	if t.closed {
		return
	}
	t.buf = append(t.buf, chunk...)
	ev := Event{Kind: "log", Chunk: string(chunk), Offset: len(t.buf)}
	for c := range t.subs {
		select {
		case c <- ev:
		default:
			delete(t.subs, c)
			close(c)
		}
	}
}

// PublishStatus fans out a status transition. A terminal status closes the
// topic: all subscriber channels are closed after delivery and the buffer is
// retained for RetainAfterClose to serve late reconnects.
func (b *Bus) PublishStatus(id int64, status models.BuildStatus, startedAt, finishedAt *time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()

	t := b.topicLocked(id)
	if t.closed {
		return
	}
	ev := Event{Kind: "status", Status: status, StartedAt: startedAt, FinishedAt: finishedAt}
	for c := range t.subs {
		select {
		case c <- ev:
		default:
		}
	}
	if status.Terminal() {
		t.closed = true
		for c := range t.subs {
			delete(t.subs, c)
			close(c)
		}
		time.AfterFunc(RetainAfterClose, func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if cur, ok := b.topics[id]; ok && cur == t {
				delete(b.topics, id)
			}
		})
	}
}

// Seed installs already-durable log bytes into a topic's buffer WITHOUT
// publishing them, and only when the buffer is shorter than what it is given.
//
// It exists for one situation: a build that outlived the process streaming it.
// A remote agent keeps building across a server restart, so the new process
// meets a build whose stored log is long and whose topic is brand new and
// empty — and appending to that gap would hand subscribers offsets that mean
// nothing. Seeding restores the invariant that the buffer mirrors the DB.
//
// The silence is the point. Anyone who subscribed to the empty topic replayed
// those same bytes from the stored log (Subscribe returning cur == 0 is
// exactly the signal handleBuildEvents uses to fall back to it), so
// publishing them would paint the log twice.
func (b *Bus) Seed(id int64, data []byte) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	t := b.topicLocked(id)
	if t.closed || len(t.buf) >= len(data) {
		return len(t.buf)
	}
	t.buf = append(t.buf[:0], data...)
	return len(t.buf)
}

// LogTail returns buffered bytes from `from` without subscribing, plus the
// current total offset. ok is false when the build has no live topic (serve
// from the DB instead).
func (b *Bus) LogTail(id int64, from int) (tail []byte, cur int, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	t, exists := b.topics[id]
	if !exists {
		return nil, 0, false
	}
	if from < 0 {
		from = 0
	}
	if from > len(t.buf) {
		from = len(t.buf)
	}
	return append([]byte(nil), t.buf[from:]...), len(t.buf), true
}

// Live reports whether the build currently has an open topic.
func (b *Bus) Live(id int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.topics[id]
	return ok && !t.closed
}
