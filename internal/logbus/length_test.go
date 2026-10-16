package logbus

import (
	"fmt"
	"strings"
	"testing"
)

func TestLengthMetadataAndCommittedOffsets(t *testing.T) {
	b := New()
	if _, ok := b.Length(1); ok {
		t.Fatal("unexpected topic")
	}
	b.Seed(1, []byte("old π"))
	_, _, ch, unsub := b.Subscribe(1, 0)
	defer unsub()
	b.PublishAt(1, len("old π"), []byte("世界"))
	b.PublishAt(1, len("old π"), []byte("世界"))
	b.PublishAt(1, len("old π世"), []byte("界!"))
	n, ok := b.Length(1)
	if !ok || n != len("old π世界!") {
		t.Fatal(n, ok)
	}
	tail, _, _ := b.LogTail(1, 0)
	if string(tail) != "old π世界!" {
		t.Fatal(string(tail))
	}
	if ev := <-ch; ev.Offset != len("old π世界") {
		t.Fatal(ev)
	}
	if ev := <-ch; ev.Chunk != "!" {
		t.Fatal(ev)
	}
	select {
	case ev := <-ch:
		t.Fatal("duplicate", ev)
	default:
	}
	b.PublishAt(1, n+2, []byte("gap"))
	if _, open := <-ch; open {
		t.Fatal("gap must disconnect")
	}
	tail, cur, ok := b.LogTail(1, n+2)
	if !ok || string(tail) != "gap" || cur != n+5 {
		t.Fatal("gap tail must retain absolute bytes", cur, ok)
	}
	if _, _, ok = b.LogTail(1, 0); ok {
		t.Fatal("missing durable prefix must fall back")
	}

}
func BenchmarkLogbusLength(b *testing.B) {
	for _, size := range []int{1 << 20, 20 << 20} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			bus := New()
			bus.Seed(1, []byte(strings.Repeat("x", size)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if n, ok := bus.Length(1); !ok || n != size {
					b.Fatal(n, ok)
				}
			}
		})
	}
}

// Baseline models the pre-optimization full-log copy used only to obtain len.
func BenchmarkLogbusLengthViaTail(b *testing.B) {
	data := []byte(strings.Repeat("x", 20<<20))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copyOfLog := append([]byte(nil), data...)
		if len(copyOfLog) != len(data) {
			b.Fatal("copy")
		}
	}
}
