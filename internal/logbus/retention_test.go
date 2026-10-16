package logbus

import (
	"bytes"
	"github.com/FanDoster/Build-System/internal/models"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestBoundedTwentyMiBRetentionAndAbsoluteReplay(t *testing.T) {
	b := New()
	unit := "old 世界π\x00\n"
	data := []byte(strings.Repeat(unit, (20<<20)/len(unit)+1))
	b.Seed(1, data)
	if n, ok := b.Length(1); !ok || n != len(data) {
		t.Fatal(n, ok)
	}
	topic := b.topics[1]
	if len(topic.buf) > MaxRetainedBytes || cap(topic.buf) > MaxRetainedBytes {
		t.Fatal("unbounded topic", len(topic.buf), cap(topic.buf))
	}
	if _, n, ok := b.LogTail(1, 0); ok || n != len(data) {
		t.Fatal("old offset must require durable replay", n, ok)
	}
	from := len(data) - 100
	tail, n, ok := b.LogTail(1, from)
	if !ok || n != len(data) || !bytes.Equal(tail, data[from:]) {
		t.Fatal("non-ASCII byte tail mismatch")
	}
	snap, n, ch, unsub := b.Subscribe(1, 0)
	defer unsub()
	if len(snap) != 0 || n != len(data) {
		t.Fatal("old subscribe must request durable replay")
	}
	chunk := []byte("new π世界\n")
	b.PublishAt(1, len(data), chunk)
	ev := <-ch
	if ev.Offset != len(data)+len(chunk) || ev.Chunk != string(chunk) {
		t.Fatal(ev)
	}
	// A client combines its durable replay through n with absolute events.
	replay := append(append([]byte(nil), data...), []byte(ev.Chunk)...)
	if !bytes.Equal(replay, append(data, chunk...)) {
		t.Fatal("gap or duplication")
	}
	b.PublishAt(1, len(data), chunk)
	select {
	case ev := <-ch:
		t.Fatal("retry duplicated", ev)
	default:
	}
	if cap(topic.buf) > MaxRetainedBytes {
		t.Fatal("append grew buffer")
	}
}
func TestBoundedSeedAtAndOversizedEvents(t *testing.T) {
	b := New()
	start := 20 << 20
	suffix := []byte(strings.Repeat("π世界\n", 40000))
	b.SeedAt(1, start, suffix)
	if n, _ := b.Length(1); n != start+len(suffix) {
		t.Fatal(n)
	}
	if _, _, ok := b.LogTail(1, start); ok {
		t.Fatal("evicted suffix must fall back")
	}
	_, _, ch, unsub := b.Subscribe(1, start+len(suffix))
	defer unsub()
	chunk := []byte(strings.Repeat("🦊世界π\n", 20000))
	b.PublishAt(1, start+len(suffix), chunk)
	var out []byte
	end := start + len(suffix)
	for len(out) < len(chunk) {
		ev := <-ch
		if len(ev.Chunk) > maxEventBytes || !utf8.ValidString(ev.Chunk) {
			t.Fatal("invalid bounded UTF-8 event")
		}
		out = append(out, ev.Chunk...)
		end += len(ev.Chunk)
		if ev.Offset != end {
			t.Fatal("nonabsolute event")
		}
	}
	if !bytes.Equal(out, chunk) {
		t.Fatal("events changed bytes")
	}
	if len(b.topics[1].buf) > MaxRetainedBytes || cap(b.topics[1].buf) > MaxRetainedBytes {
		t.Fatal("oversized append retained too much")
	}
}
func TestBoundedSlowSubscriberAndTerminal(t *testing.T) {
	b := New()
	_, _, ch, unsub := b.Subscribe(1, 0)
	defer unsub()
	// One huge committed write is split into bounded events. Slow readers
	// disconnect and must resume from durable storage, not retained oldest bytes.
	data := []byte(strings.Repeat("π", 12<<20))
	b.PublishAt(1, 0, data)
	b.PublishStatus(1, models.StatusSuccess, nil, nil)
	count := 0
	for ev := range ch {
		count++
		if len(ev.Chunk) > maxEventBytes {
			t.Fatal("oversized queued payload")
		}
	}
	if count != subBuffer {
		t.Fatal(count)
	}
	if _, n, ok := b.LogTail(1, 0); ok || n != len(data) {
		t.Fatal("slow replay must use DB", n, ok)
	}
	if len(b.topics[1].buf) > MaxRetainedBytes || cap(b.topics[1].buf) > MaxRetainedBytes {
		t.Fatal("huge log retained")
	}
}
func TestBoundedConcurrentSubscribeLengthAndAppend(t *testing.T) {
	b := New()
	b.SeedAt(1, 20<<20, []byte("π"))
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				_, _ = b.Length(1)
				_, _, _ = b.LogTail(1, 20<<20)
				_, _, _, u := b.Subscribe(1, 0)
				u()
			}
		}()
	}
	for j := 0; j < 1000; j++ {
		b.Publish(1, []byte("世界\n"))
	}
	wg.Wait()
	if n, _ := b.Length(1); n != (20<<20)+len("π")+1000*len("世界\n") {
		t.Fatal(n)
	}
}

func BenchmarkBoundedTopicSeed(b *testing.B) {
	tail := []byte(strings.Repeat("x", MaxRetainedBytes))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bus := New()
		bus.SeedAt(1, (20<<20)-len(tail), tail)
		if n, _ := bus.Length(1); n != 20<<20 {
			b.Fatal(n)
		}
	}
}
