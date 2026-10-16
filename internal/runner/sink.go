package runner

import (
	"bytes"
	"fmt"
	"log"
	"net/url"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/logbus"
)

const (
	// sinkFlushInterval / sinkFlushBytes bound the durable batching delay.
	// Bytes are published only after the transaction commits.
	sinkFlushInterval = 500 * time.Millisecond
	sinkFlushBytes    = 8 * 1024
	// sinkMaxHold caps how long a line can grow before we force-emit it.
	sinkMaxHold = 8 * 1024
)

// logSink is an io.Writer that commits command output in batches, then
// publishes committed bytes into the log bus. Secrets are scrubbed before any
// byte leaves the sink; scanning happens on complete \n- or \r-terminated
// segments (a secret cannot contain either), with a holdback window when a
// force-drain splits an unfinished line.
type logSink struct {
	buildID int64
	secret  string
	encSec  string
	db      *db.DB
	bus     *logbus.Bus

	mu        sync.Mutex
	lineBuf   []byte // incomplete trailing segment, not yet scrubbed/emitted
	dbBuf     []byte // scrubbed bytes not yet flushed to the DB
	lastFlush time.Time
	closed    bool
	stop      chan struct{}
}

func newLogSink(buildID int64, secret string, database *db.DB, bus *logbus.Bus) *logSink {
	s := &logSink{
		buildID:   buildID,
		secret:    secret,
		db:        database,
		bus:       bus,
		lastFlush: time.Now(),
		stop:      make(chan struct{}),
	}
	if secret != "" {
		s.encSec = url.User(secret).String()
	}
	// Background flusher covers quiet periods (slow command, no output).
	go func() {
		ticker := time.NewTicker(sinkFlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				s.mu.Lock()
				s.flushDBLocked()
				s.mu.Unlock()
			}
		}
	}()
	return s
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return len(p), nil
	}
	s.lineBuf = append(s.lineBuf, p...)

	// Emit everything up to and including the last segment terminator.
	if i := lastTerminator(s.lineBuf); i >= 0 {
		s.emitLocked(s.lineBuf[:i+1])
		s.lineBuf = append(s.lineBuf[:0], s.lineBuf[i+1:]...)
	}

	// Force-drain an overlong unterminated line. Scrub the WHOLE buffer
	// first: a fully-buffered secret could otherwise be split by the cut and
	// leak as two raw halves that scrubSecret never matches. After the
	// scrub, only an incomplete secret suffix (still arriving) can remain,
	// and it fits inside the holdback window by construction.
	if len(s.lineBuf) > sinkMaxHold {
		if s.secret != "" {
			s.lineBuf = append(s.lineBuf[:0], scrubSecret(string(s.lineBuf), s.secret)...)
		}
		// Never split a multi-byte UTF-8 sequence across chunk boundaries —
		// the halves would be corrupted to U+FFFD by JSON framing.
		cut := len(s.lineBuf) - s.holdback()
		if cut >= len(s.lineBuf) {
			cut = len(s.lineBuf)
			// Hold back an incomplete trailing rune still arriving.
			for k := 1; k <= utf8.UTFMax && cut-k >= 0; k++ {
				if utf8.RuneStart(s.lineBuf[cut-k]) {
					if !utf8.FullRune(s.lineBuf[cut-k : cut]) {
						cut -= k
					}
					break
				}
			}
		} else {
			for cut > 0 && !utf8.RuneStart(s.lineBuf[cut]) {
				cut--
			}
		}
		if cut > 0 {
			s.emitLocked(s.lineBuf[:cut])
			s.lineBuf = append(s.lineBuf[:0], s.lineBuf[cut:]...)
		}
	}

	if len(s.dbBuf) >= sinkFlushBytes || time.Since(s.lastFlush) >= sinkFlushInterval {
		if err := s.flushDBLocked(); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// holdback is the number of trailing bytes that must stay unscanned so a
// secret split across a forced drain cannot leak.
func (s *logSink) holdback() int {
	if s.secret == "" {
		return 0
	}
	h := len(s.secret)
	if len(s.encSec) > h {
		h = len(s.encSec)
	}
	return h
}

// emitLocked scrubs a chunk and queues it for durable commit and publication.
func (s *logSink) emitLocked(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	scrubbed := scrubSecret(string(chunk), s.secret)
	s.dbBuf = append(s.dbBuf, scrubbed...)
}

func (s *logSink) flushDBLocked() error {
	if len(s.dbBuf) == 0 {
		s.lastFlush = time.Now()
		return nil
	}
	// On failure (e.g. transient SQLITE_BUSY) keep the buffer and retry on
	// the next flush instead of silently dropping a chunk of the log.
	if start, err := s.db.AppendBuildLogAt(s.buildID, string(s.dbBuf)); err == nil {
		if cur, ok := s.bus.Length(s.buildID); !ok || cur < start {
			// A restarted or requeued build may already have a durable prefix.
			// Seed only bytes before this append; publish only its newly stored tail.
			from := start - logbus.MaxRetainedBytes
			if from < 0 {
				from = 0
			}
			if prefix, readErr := s.db.BuildLogSlice(s.buildID, from, start-from); readErr == nil {
				s.bus.SeedAt(s.buildID, from, prefix)
			}
		}
		s.bus.PublishAt(s.buildID, start, s.dbBuf)
		s.dbBuf = s.dbBuf[:0]
	} else {
		log.Printf("build %d: log persistence failed: %v", s.buildID, err)
		s.lastFlush = time.Now()
		return err
	}
	s.lastFlush = time.Now()
	return nil
}

// Close drains any partial line and flushes the DB buffer. Idempotent.
func (s *logSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.stop)
	}
	if len(s.lineBuf) > 0 {
		s.emitLocked(s.lineBuf)
		s.lineBuf = nil
	}
	s.flushDBLocked()
	if len(s.dbBuf) > 0 {
		return fmt.Errorf("build %d: log bytes remain uncommitted", s.buildID)
	}
	return nil
}

// lastTerminator returns the index of the last \n or \r in b, or -1.
func lastTerminator(b []byte) int {
	n := bytes.LastIndexByte(b, '\n')
	r := bytes.LastIndexByte(b, '\r')
	if r > n {
		return r
	}
	return n
}
