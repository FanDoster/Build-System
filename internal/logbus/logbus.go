// Package logbus is an in-memory pub/sub hub for live build logs.
//
// Each topic retains a bounded tail of committed bytes at absolute byte
// offsets, plus a set of subscribers. Older bytes replay from durable storage. Subscribe atomically
// returns a snapshot and a live channel under one lock, so a reader can never
// miss or duplicate bytes between replay and streaming. The DB row is the
// durable copy; a topic retains only its newest committed bytes.
package logbus

import (
	"sync"
	"time"
	"unicode/utf8"

	"github.com/FanDoster/Build-System/internal/models"
)

// RetainAfterClose is how long a finished build's buffer stays available for
// late reconnects before the topic is dropped.
const RetainAfterClose = 30 * time.Second

// MaxRetainedBytes bounds each live topic's backing buffer. Subscriber event
// queues are separately bounded by subBuffer. Offsets remain absolute bytes.
const MaxRetainedBytes = 256 * 1024

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
	base   int // absolute byte offset of buf[0]
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
// returned channel is closed immediately after any snapshot. If from predates
// retained history, snapshot is nil; replay durable bytes through cur before
// handling queued absolute-offset events.
func (b *Bus) Subscribe(id int64, from int) (snapshot []byte, cur int, ch <-chan Event, unsub func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	t := b.topicLocked(id)
	if from < 0 {
		from = 0
	}
	cur = t.base + len(t.buf)
	if from > cur {
		from = cur
	}
	// An old offset has no in-memory snapshot. The caller must replay durable
	// storage through cur, then deduplicate queued events by absolute offset.
	if from >= t.base {
		snapshot = append([]byte(nil), t.buf[from-t.base:]...)
	}

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
	t.appendTail(chunk)
	t.publishLog(chunk, t.base+len(t.buf))
}

// Length reads topic metadata only. It never copies log bytes.
func (b *Bus) Length(id int64) (int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.topics[id]
	if !ok {
		return 0, false
	}
	return t.base + len(t.buf), true
}

// PublishAt publishes committed bytes at their authoritative byte offset.
// Duplicate or overlapping retries are trimmed. A gap drops subscribers so
// they reconnect and replay durable storage, rather than accepting false offsets.
func (b *Bus) PublishAt(id int64, start int, chunk []byte) {
	if len(chunk) == 0 || start < 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.topicLocked(id)
	if t.closed {
		return
	}
	total := t.base + len(t.buf)
	if start > total {
		// Storage has a prefix this topic never observed. Disconnect existing
		// readers for durable replay, then retain the authoritative newest tail.
		for c := range t.subs {
			delete(t.subs, c)
			close(c)
		}
		t.buf = t.buf[:0]
		t.base = start
	} else {
		overlap := total - start
		if overlap >= len(chunk) {
			return
		}
		chunk = chunk[overlap:]
	}
	t.appendTail(chunk)
	t.publishLog(chunk, t.base+len(t.buf))
}

// appendTail never allocates a backing array larger than the configured cap,
// including a single huge append or seed. It may split UTF-8 only at the old
// retention boundary; offsets and raw tails are bytes, never rune counts.
func (t *topic) appendTail(chunk []byte) {
	total := t.base + len(t.buf) + len(chunk)
	need := len(t.buf) + len(chunk)
	if need > MaxRetainedBytes {
		need = MaxRetainedBytes
	}
	if cap(t.buf) < need {
		capacity := cap(t.buf) * 2
		if capacity < 4096 {
			capacity = 4096
		}
		if capacity < need {
			capacity = need
		}
		if capacity > MaxRetainedBytes {
			capacity = MaxRetainedBytes
		}
		buf := make([]byte, len(t.buf), capacity)
		copy(buf, t.buf)
		t.buf = buf
	}
	if len(chunk) >= MaxRetainedBytes {
		t.buf = t.buf[:MaxRetainedBytes]
		copy(t.buf, chunk[len(chunk)-MaxRetainedBytes:])
	} else {
		drop := len(t.buf) + len(chunk) - MaxRetainedBytes
		if drop > 0 {
			copy(t.buf, t.buf[drop:])
			t.buf = t.buf[:len(t.buf)-drop]
		}
		t.buf = append(t.buf, chunk...)
	}
	t.base = total - len(t.buf)
}

// Queued log events are independently bounded as well as topic retention.
// Valid UTF-8 runes are never split across event payloads.
const maxEventBytes = 64 * 1024

func (t *topic) publishLog(chunk []byte, end int) {
	offset := end - len(chunk)
	for len(chunk) > 0 && len(t.subs) > 0 {
		n := len(chunk)
		if n > maxEventBytes {
			n = maxEventBytes
			for n > 0 && !utf8.RuneStart(chunk[n]) {
				n--
			}
			if n == 0 {
				n = maxEventBytes
			}
		}
		offset += n
		ev := Event{Kind: "log", Chunk: string(chunk[:n]), Offset: offset}
		for c := range t.subs {
			select {
			case c <- ev:
			default:
				delete(t.subs, c)
				close(c)
			}
		}
		chunk = chunk[n:]
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

// Seed is a compatibility wrapper for a complete durable log. Only its
// bounded suffix is retained. Prefer SeedAt to avoid reading all durable bytes.
func (b *Bus) Seed(id int64, data []byte) int { return b.SeedAt(id, 0, data) }

// SeedAt silently installs a durable suffix at its absolute byte offset. It
// never moves the total offset backward and never publishes replay bytes.
func (b *Bus) SeedAt(id int64, start int, data []byte) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.topicLocked(id)
	total := t.base + len(t.buf)
	if start < 0 || t.closed || start+len(data) <= total {
		return total
	}
	t.buf = t.buf[:0]
	t.base = start
	t.appendTail(data)
	return t.base + len(t.buf)
}

// LogTail returns retained bytes from the absolute byte offset from. If from
// predates the retention boundary, ok is false and cur is still the absolute
// total; callers must replay durable storage rather than silently lose bytes.
func (b *Bus) LogTail(id int64, from int) (tail []byte, cur int, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, exists := b.topics[id]
	if !exists {
		return nil, 0, false
	}
	cur = t.base + len(t.buf)
	if from < 0 {
		from = 0
	}
	if from < t.base {
		return nil, cur, false
	}
	if from > cur {
		from = cur
	}
	return append([]byte(nil), t.buf[from-t.base:]...), cur, true
}

// Live reports whether the build currently has an open topic.
func (b *Bus) Live(id int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.topics[id]
	return ok && !t.closed
}
