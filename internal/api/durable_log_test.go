package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/FanDoster/Build-System/internal/logbus"
	"github.com/FanDoster/Build-System/internal/models"
)

func TestDurableLogPollingAndMetaIgnoreBus(t *testing.T) {
	s, mux := newTestServer(t)
	_, b := seedBuild(t, s, models.StatusRunning, "stored →\n")
	// The bus may lag a committed append, or contain old data left by an
	// earlier implementation. Neither changes the durable polling response.
	s.Bus.Publish(b.ID, []byte("stale"))
	for _, offset := range []int{0, len("stored "), len("stored →\n"), 999} {
		w := doJSON(t, mux, "GET", fmt.Sprintf("/api/builds/%d/log?offset=%d", b.ID, offset), nil)
		from := offset
		if from > len("stored →\n") {
			from = len("stored →\n")
		}
		if w.Code != 200 || w.Body.String() != "stored →\n"[from:] || w.Header().Get("X-Log-Offset") != strconv.Itoa(len("stored →\n")) {
			t.Fatalf("offset %d: status=%d body=%q headers=%v", offset, w.Code, w.Body.String(), w.Header())
		}
	}
	w := doJSON(t, mux, "GET", fmt.Sprintf("/api/builds/%d?meta=1", b.ID), nil)
	var got models.Build
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Log != "" || got.LogLen != int64(len("stored →\n")) {
		t.Fatalf("metadata log=%q log_len=%d", got.Log, got.LogLen)
	}
}

func TestDurableSSETerminalReplayIgnoresIncompleteBus(t *testing.T) {
	for _, offset := range []int{0, len("prefix\n"), 999} {
		t.Run(strconv.Itoa(offset), func(t *testing.T) {
			s, mux := newTestServer(t)
			full := "prefix\nfinal →\n"
			_, b := seedBuild(t, s, models.StatusSuccess, full)
			s.Bus.Publish(b.ID, []byte("prefix\n"))
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", fmt.Sprintf("/api/builds/%d/events?offset=1", b.ID), nil)
			r.Header.Set("Last-Event-ID", strconv.Itoa(offset))
			mux.ServeHTTP(w, r)
			sc := bufio.NewScanner(strings.NewReader(w.Body.String()))
			from := offset
			if from > len(full) {
				from = len(full)
			}
			if from < len(full) {
				checkBuildAgentLog(t, nextBuildAgentEvent(t, sc), full[from:], len(full))
			}
			ev := nextBuildAgentEvent(t, sc)
			if ev.kind != "status" || !strings.Contains(string(ev.data), `"status":"success"`) {
				t.Fatalf("terminal event: %s %s", ev.kind, ev.data)
			}
		})
	}
}

func TestDurableSSEReplayDeduplicatesDelayedAndOverlappingPublish(t *testing.T) {
	s, mux := newTestServer(t)
	prefix, tail := "prefix →\n", "new tail\n"
	_, b := seedBuild(t, s, models.StatusRunning, prefix)
	w := &buildAgentHookWriter{ResponseRecorder: httptest.NewRecorder()}
	w.onFlush = func() {
		// Replay already sent prefix. These notifications arrive late, after
		// their bytes were durable, and one overlaps the previous byte range.
		if err := s.DB.AppendBuildLog(b.ID, tail); err != nil {
			t.Fatal(err)
		}
		s.Bus.PublishAt(b.ID, 0, []byte(prefix))
		s.Bus.PublishAt(b.ID, len(prefix)-1, []byte(prefix[len(prefix)-1:]+tail))
		if err := s.DB.FinishBuild(b.ID, models.StatusSuccess); err != nil {
			t.Fatal(err)
		}
		s.Bus.PublishStatus(b.ID, models.StatusSuccess, nil, nil)
	}
	mux.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/api/builds/%d/events", b.ID), nil))
	sc := bufio.NewScanner(strings.NewReader(w.Body.String()))
	if ev := nextBuildAgentEvent(t, sc); ev.kind != "status" {
		t.Fatalf("initial event = %s", ev.kind)
	}
	checkBuildAgentLog(t, nextBuildAgentEvent(t, sc), prefix, len(prefix))
	checkBuildAgentLog(t, nextBuildAgentEvent(t, sc), tail, len(prefix+tail))
	if ev := nextBuildAgentEvent(t, sc); ev.kind != "status" || !strings.Contains(string(ev.data), `"status":"success"`) {
		t.Fatalf("expected terminal status, got %s %s", ev.kind, ev.data)
	}
}

func TestDurableSSECancelRequeuedBuildKeepsPrefix(t *testing.T) {
	s, mux := newTestServer(t)
	_, b := seedBuild(t, s, models.StatusPending, "previous attempt →\n")
	sc := openBuildAgentStream(t, mux, b.ID)
	nextBuildAgentEvent(t, sc) // pending status
	checkBuildAgentLog(t, nextBuildAgentEvent(t, sc), "previous attempt →\n", len("previous attempt →\n"))
	w := doJSON(t, mux, "POST", fmt.Sprintf("/api/builds/%d/cancel", b.ID), nil)
	if w.Code != 200 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	checkBuildAgentLog(t, nextBuildAgentEvent(t, sc), "[canceled while queued]\n", len("previous attempt →\n[canceled while queued]\n"))
	if ev := nextBuildAgentEvent(t, sc); ev.kind != "status" || !strings.Contains(string(ev.data), `"status":"canceled"`) {
		t.Fatalf("expected canceled status, got %s %s", ev.kind, ev.data)
	}
}

func TestDurableSSEReadFailureDoesNotEmitTerminalStatus(t *testing.T) {
	s, mux := newTestServer(t)
	_, b := seedBuild(t, s, models.StatusRunning, "prefix\n")
	w := &buildAgentHookWriter{ResponseRecorder: httptest.NewRecorder()}
	w.onFlush = func() {
		if err := s.DB.Close(); err != nil {
			t.Fatal(err)
		}
		s.Bus.PublishStatus(b.ID, models.StatusSuccess, nil, nil)
	}
	mux.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/api/builds/%d/events", b.ID), nil))
	if strings.Contains(w.Body.String(), `"status":"success"`) {
		t.Fatalf("terminal status hides failed durable tail read: %s", w.Body.String())
	}
}

func TestDurableTwentyMiBReplayBeyondRingRetention(t *testing.T) {
	s, mux := newTestServer(t)
	unit := "old π世界\n"
	full := strings.Repeat(unit, (20<<20)/len(unit)+1)
	_, b := seedBuild(t, s, models.StatusRunning, full)
	if err := s.seedBus(b.ID, len(full)); err != nil {
		t.Fatal(err)
	}
	if n, ok := s.Bus.Length(b.ID); !ok || n != len(full) {
		t.Fatal(n, ok)
	}
	if _, n, ok := s.Bus.LogTail(b.ID, 0); ok || n != len(full) {
		t.Fatal("seed must retain bounded suffix only", n, ok)
	}
	from := (len(full) - logbus.MaxRetainedBytes - 100) / len(unit) * len(unit)
	w := doJSON(t, mux, "GET", fmt.Sprintf("/api/builds/%d/log?offset=%d", b.ID, from), nil)
	if w.Code != 200 || w.Body.String() != full[from:] {
		t.Fatal("poll tail older than retention lost bytes")
	}
	if err := s.DB.FinishBuild(b.ID, models.StatusSuccess); err != nil {
		t.Fatal(err)
	}
	s.Bus.PublishStatus(b.ID, models.StatusSuccess, nil, nil)
	w = httptest.NewRecorder()
	r := httptest.NewRequest("GET", fmt.Sprintf("/api/builds/%d/events", b.ID), nil)
	r.Header.Set("Last-Event-ID", strconv.Itoa(from))
	mux.ServeHTTP(w, r)
	sc := bufio.NewScanner(strings.NewReader(w.Body.String()))
	sc.Buffer(make([]byte, 4096), logbus.MaxRetainedBytes*3)
	checkBuildAgentLog(t, nextBuildAgentEvent(t, sc), full[from:], len(full))
	if ev := nextBuildAgentEvent(t, sc); ev.kind != "status" || !strings.Contains(string(ev.data), `"status":"success"`) {
		t.Fatal("missing terminal after replay", ev.kind)
	}
}
