package db

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/FanDoster/Build-System/internal/models"
)

func sighting(t *testing.T, d *DB, name string, execs []string, scheme string, at time.Time) {
	t.Helper()
	if err := d.RecordAgentSighting(name, execs, scheme, SelfReport{}, at); err != nil {
		t.Fatalf("record sighting: %v", err)
	}
}

func TestSightingCreatesThenUpdatesARow(t *testing.T) {
	d := openTestDB(t)
	t0 := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	sighting(t, d, "mac", []string{"mac"}, "https", t0)
	a, err := d.GetAgentRow("mac")
	if err != nil || a == nil {
		t.Fatalf("get: %v %v", a, err)
	}
	if len(a.Executors) != 1 || a.Executors[0] != "mac" {
		t.Errorf("executors = %v", a.Executors)
	}
	if a.FirstSeenAt == nil || !a.FirstSeenAt.Equal(t0) {
		t.Errorf("first seen = %v, want %v", a.FirstSeenAt, t0)
	}
	if a.LastScheme != "https" {
		t.Errorf("scheme = %q", a.LastScheme)
	}

	// A later sighting moves last-seen and the advertised list, and leaves
	// first-seen alone — it is the one fact here nothing else records.
	t1 := t0.Add(time.Hour)
	sighting(t, d, "mac", []string{"mac", "ios"}, "https", t1)
	a, _ = d.GetAgentRow("mac")
	if !a.FirstSeenAt.Equal(t0) {
		t.Errorf("first seen moved to %v; an agent restart must not erase its history", a.FirstSeenAt)
	}
	if !a.LastSeenAt.Equal(t1) {
		t.Errorf("last seen = %v, want %v", a.LastSeenAt, t1)
	}
	if len(a.Executors) != 2 {
		t.Errorf("executors = %v, want the newly advertised pair", a.Executors)
	}
}

// The failure this table's write path is shaped to avoid. INSERT OR REPLACE
// rewrites the whole row, so a paused agent's next poll would blank its own
// pause and quietly start taking work again.
func TestASightingDoesNotClearAPause(t *testing.T) {
	d := openTestDB(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	until := now.Add(2 * time.Hour)

	sighting(t, d, "mac", []string{"mac"}, "https", now)
	if err := d.PauseAgent("mac", until, "updating Unity"); err != nil {
		t.Fatal(err)
	}

	// The agent keeps polling while paused — that is required, so that it stays
	// visibly connected rather than decaying to offline.
	sighting(t, d, "mac", []string{"mac"}, "https", now.Add(time.Minute))

	a, _ := d.GetAgentRow("mac")
	if !a.Paused(now.Add(time.Minute)) {
		t.Fatal("the agent un-paused itself by polling")
	}
	if a.PauseNote != "updating Unity" {
		t.Errorf("note = %q, want it preserved across a sighting", a.PauseNote)
	}
	if !a.PausedUntil.Equal(until) {
		t.Errorf("paused until = %v, want %v", a.PausedUntil, until)
	}
}

// Pause expiry is decided in Go. Proven here rather than asserted, because the
// SQL spelling of this test would pass while matching nothing.
func TestPauseExpiresOnItsOwn(t *testing.T) {
	d := openTestDB(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	if err := d.PauseAgent("mac", now.Add(30*time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	a, _ := d.GetAgentRow("mac")
	if !a.Paused(now.Add(29 * time.Minute)) {
		t.Error("not paused a minute before expiry")
	}
	if a.Paused(now.Add(31 * time.Minute)) {
		t.Error("still paused after expiry — an unexpiring pause is a dead CI that looks healthy")
	}
}

// Written because the driver's format and SQLite's own disagree: any SQL
// comparison on these columns silently matches nothing. This proves the value
// survives the round trip so the Go-side comparison has something true to work
// with.
func TestPauseTimeSurvivesTheRoundTrip(t *testing.T) {
	d := openTestDB(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	until := now.Add(90 * time.Minute)

	if err := d.PauseAgent("mac", until, ""); err != nil {
		t.Fatal(err)
	}
	got, err := d.AgentPausedUntil("mac")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("paused_until came back nil")
	}
	if !got.Equal(until) {
		t.Errorf("paused until = %v, want %v", got, until)
	}

	// And the SQL spelling really is broken, which is why the comparison lives
	// in Go. If this ever starts matching, the driver's time format changed and
	// the rule in this file's header should be revisited.
	var n int
	if err := d.conn.QueryRow(
		`SELECT COUNT(*) FROM agents WHERE datetime(paused_until) > datetime('now')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Logf("NOTE: datetime(paused_until) now matches %d row(s) — the driver's time format may have changed", n)
	}
}

// An agent that has never connected can still be paused, which is the case
// where it matters most: there is nothing else to stop a machine taking work
// the instant it arrives.
func TestAnUnseenAgentCanBePaused(t *testing.T) {
	d := openTestDB(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	if err := d.PauseAgent("not-here-yet", now.Add(time.Hour), "cable unplugged"); err != nil {
		t.Fatal(err)
	}
	until, err := d.AgentPausedUntil("not-here-yet")
	if err != nil || until == nil {
		t.Fatalf("pause did not stick: %v %v", until, err)
	}

	// And when it does arrive, its first poll must not clear the pause.
	sighting(t, d, "not-here-yet", []string{"mac"}, "https", now.Add(time.Minute))
	a, _ := d.GetAgentRow("not-here-yet")
	if !a.Paused(now.Add(time.Minute)) {
		t.Error("the agent's first poll cleared a pause set before it connected")
	}
}

func TestResumeClearsThePauseAndIsIdempotent(t *testing.T) {
	d := openTestDB(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	if err := d.PauseAgent("mac", now.Add(time.Hour), "note"); err != nil {
		t.Fatal(err)
	}
	if err := d.ResumeAgent("mac"); err != nil {
		t.Fatal(err)
	}
	a, _ := d.GetAgentRow("mac")
	if a.Paused(now) {
		t.Error("still paused after resume")
	}
	if a.PauseNote != "" {
		t.Errorf("note = %q, want cleared with the pause", a.PauseNote)
	}
	// Resuming something that is not paused is what the operator wants anyway.
	if err := d.ResumeAgent("mac"); err != nil {
		t.Errorf("second resume: %v", err)
	}
	if err := d.ResumeAgent("never-existed"); err != nil {
		t.Errorf("resume of an unknown agent: %v", err)
	}
}

// A name is self-asserted, so one typo in a config file creates a row nothing
// would otherwise remove.
func TestForgetRemovesTheRow(t *testing.T) {
	d := openTestDB(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	sighting(t, d, "typo", []string{"mac"}, "https", now)
	if err := d.ForgetAgent("typo"); err != nil {
		t.Fatal(err)
	}
	a, err := d.GetAgentRow("typo")
	if err != nil {
		t.Fatal(err)
	}
	if a != nil {
		t.Errorf("row survived forget: %+v", a)
	}
	if err := d.ForgetAgent("never-existed"); err != nil {
		t.Errorf("forget of an unknown agent: %v", err)
	}
}

// The name is a primary key chosen by whoever holds the agent token. Without a
// bound, one request writes a 64 KiB key; without a cap, one credential fills
// the table by varying the name.
func TestAbsurdNamesAreNotStoredButNeverError(t *testing.T) {
	d := openTestDB(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	for _, name := range []string{
		"",
		strings.Repeat("a", models.MaxAgentNameLen+1),
		"trailing ",
		"nul\x00byte",
		"line\nbreak",
	} {
		if err := d.RecordAgentSighting(name, []string{"mac"}, "https", SelfReport{}, now); err != nil {
			t.Errorf("name %q returned an error; a bad name must never fail a claim: %v", name, err)
		}
	}
	rows, _ := d.ListAgentRows()
	if len(rows) != 0 {
		t.Errorf("stored %d rows for names that should have been ignored: %+v", len(rows), rows)
	}
}

func TestTheTableIsCappedButKnownAgentsKeepWorking(t *testing.T) {
	d := openTestDB(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	sighting(t, d, "real-agent", []string{"mac"}, "https", now)
	for i := 0; i < MaxAgentRows+50; i++ {
		name := "junk-" + strings.Repeat("x", i%10) + string(rune('a'+i%26)) + itoa(i)
		if err := d.RecordAgentSighting(name, []string{"mac"}, "https", SelfReport{}, now); err != nil {
			t.Fatalf("cap must never surface as an error: %v", err)
		}
	}
	rows, err := d.ListAgentRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) > MaxAgentRows {
		t.Errorf("table grew to %d rows, past the %d cap", len(rows), MaxAgentRows)
	}

	// The real agent must keep being updated after the table filled up.
	later := now.Add(time.Hour)
	sighting(t, d, "real-agent", []string{"mac"}, "https", later)
	a, _ := d.GetAgentRow("real-agent")
	if a == nil || !a.LastSeenAt.Equal(later) {
		t.Errorf("a known agent stopped being updated once the table was full: %+v", a)
	}
}

func TestPauseNoteIsBounded(t *testing.T) {
	d := openTestDB(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	long := strings.Repeat("x", models.MaxPauseNoteLen*3)

	if err := d.PauseAgent("mac", now.Add(time.Hour), long); err != nil {
		t.Fatal(err)
	}
	a, _ := d.GetAgentRow("mac")
	if len(a.PauseNote) > models.MaxPauseNoteLen {
		t.Errorf("note stored at %d bytes, past the %d cap", len(a.PauseNote), models.MaxPauseNoteLen)
	}
}

func TestMigrationCreatesTheAgentsTable(t *testing.T) {
	d := openTestDB(t)

	var name string
	err := d.conn.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'agents'`).Scan(&name)
	if err != nil {
		t.Fatalf("agents table missing after migrate: %v", err)
	}

	// migrate() must stay idempotent: it runs on every start, not once.
	for i := 0; i < 3; i++ {
		if err := d.migrate(); err != nil {
			t.Fatalf("re-migrate %d: %v", i, err)
		}
	}
	// And a pause must survive those re-runs, or every restart resumes the fleet.
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	if err := d.PauseAgent("mac", now.Add(time.Hour), "n"); err != nil {
		t.Fatal(err)
	}
	if err := d.migrate(); err != nil {
		t.Fatal(err)
	}
	until, _ := d.AgentPausedUntil("mac")
	if until == nil {
		t.Error("a pause did not survive a re-migration")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// A pause is not a sighting. Pausing a machine that has never connected must
// not record a contact that never happened — and nothing would correct it
// later, because the sighting path only fills first_seen_at while it is NULL.
func TestPausingAnUnseenAgentDoesNotInventAFirstContact(t *testing.T) {
	d := openTestDB(t)
	pausedAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	if err := d.PauseAgent("not-here-yet", pausedAt.Add(time.Hour), ""); err != nil {
		t.Fatal(err)
	}
	a, _ := d.GetAgentRow("not-here-yet")
	if a.FirstSeenAt != nil {
		t.Errorf("first seen = %v, want none — the machine has never been in touch", a.FirstSeenAt)
	}

	// Its real first contact is its first poll, and that is what gets recorded.
	arrived := pausedAt.Add(3 * time.Hour)
	sighting(t, d, "not-here-yet", []string{"mac"}, "https", arrived)
	a, _ = d.GetAgentRow("not-here-yet")
	if a.FirstSeenAt == nil || !a.FirstSeenAt.Equal(arrived) {
		t.Errorf("first seen = %v, want the first actual poll at %v", a.FirstSeenAt, arrived)
	}
}

// The note is display text an operator typed. Cutting it at a byte offset
// splits the last character of any note containing an accent, and the invalid
// bytes survive into SQLite and back out onto the page.
func TestALongNoteIsCutOnACharacterBoundary(t *testing.T) {
	d := openTestDB(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	// Three bytes per rune, so a byte-offset cut lands mid-character.
	note := strings.Repeat("é", models.MaxPauseNoteLen)
	if err := d.PauseAgent("mac", now.Add(time.Hour), note); err != nil {
		t.Fatal(err)
	}
	a, _ := d.GetAgentRow("mac")
	if len(a.PauseNote) > models.MaxPauseNoteLen {
		t.Errorf("note stored at %d bytes, past the %d cap", len(a.PauseNote), models.MaxPauseNoteLen)
	}
	if !utf8.ValidString(a.PauseNote) {
		t.Errorf("stored note is not valid UTF-8: %q", a.PauseNote)
	}
}

// --- A3: what the agent says about itself ---

func TestTheClaimBlockIsStored(t *testing.T) {
	d := openTestDB(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	up := now.Add(-3 * time.Hour)

	self := SelfReport{Version: "2026-07-29", OSArch: "darwin/arm64",
		StartedAt: &up, DiskFreeGB: ptr(120), DiskFloorGB: ptr(40)}
	if err := d.RecordAgentSighting("mac", []string{"mac"}, "https", self, now); err != nil {
		t.Fatal(err)
	}
	a, _ := d.GetAgentRow("mac")
	if a.Version != "2026-07-29" || a.OSArch != "darwin/arm64" {
		t.Errorf("version=%q os=%q", a.Version, a.OSArch)
	}
	if a.DiskFreeGB != 120 || a.DiskFloorGB != 40 {
		t.Errorf("disk free=%d floor=%d", a.DiskFreeGB, a.DiskFloorGB)
	}
	if a.StartedAt == nil || !a.StartedAt.Equal(up) {
		t.Errorf("started = %v, want %v", a.StartedAt, up)
	}
}

// The block rides the INSERT as well as the UPDATE. Without that a brand-new
// agent's very first claim would store none of it, and the page would stay
// blank until the next throttle window for no visible reason.
func TestTheFirstClaimEverStoresTheBlock(t *testing.T) {
	d := openTestDB(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	self := SelfReport{Version: "v1", OSArch: "darwin/arm64", DiskFreeGB: ptr(90), DiskFloorGB: ptr(40)}
	if err := d.RecordAgentSighting("brand-new", []string{"mac"}, "https", self, now); err != nil {
		t.Fatal(err)
	}
	a, _ := d.GetAgentRow("brand-new")
	if a == nil || a.Version != "v1" || a.DiskFreeGB != 90 {
		t.Errorf("first sighting stored %+v", a)
	}
}

// An agent too old to send the block must not blank what a newer one reported.
// This is the rollout case: the server ships first, so for a while every poll
// arrives with none of these fields set.
func TestAnOlderAgentDoesNotBlankTheReportedBlock(t *testing.T) {
	d := openTestDB(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	up := now.Add(-time.Hour)

	if err := d.RecordAgentSighting("mac", []string{"mac"}, "https",
		SelfReport{Version: "2026-07-29", OSArch: "darwin/arm64",
			StartedAt: &up, DiskFreeGB: ptr(120), DiskFloorGB: ptr(40)}, now); err != nil {
		t.Fatal(err)
	}
	// The same machine, polling from a build of the agent that knows nothing
	// about any of this.
	if err := d.RecordAgentSighting("mac", []string{"mac"}, "https",
		SelfReport{}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	a, _ := d.GetAgentRow("mac")
	if a.Version != "2026-07-29" {
		t.Errorf("version = %q, want it preserved through a poll that reported none", a.Version)
	}
	if a.OSArch != "darwin/arm64" || a.DiskFreeGB != 120 || a.DiskFloorGB != 40 {
		t.Errorf("block was blanked: %+v", a)
	}
	if a.StartedAt == nil || !a.StartedAt.Equal(up) {
		t.Errorf("started_at = %v, want it preserved", a.StartedAt)
	}
	// The ordinary fields still move, or the sighting would not be a sighting.
	if !a.LastSeenAt.Equal(now.Add(time.Minute)) {
		t.Errorf("last seen = %v, want it updated", a.LastSeenAt)
	}
}

func TestStatusRoundTripsAndOrdersProblems(t *testing.T) {
	d := openTestDB(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	used := now.Add(-48 * time.Hour)

	st := &models.AgentStatus{
		Checks: []models.AgentCheck{
			{Name: "git", Detail: "git 2.39.5", OK: true},
			{Name: "git-lfs", Detail: "not installed; brew install git-lfs", OK: false},
			{Name: "unity", Detail: "no licence; open Unity Hub and sign in", OK: false, NeedsOperator: true},
		},
		Unity:      []string{"2022.3.62f2"},
		Tools:      map[string]string{"git": "/usr/bin/git"},
		Timeouts:   map[string]string{"build": "90m"},
		Workspaces: []models.AgentWorkspace{{Name: "ship-main", Used: &used}},
	}
	if err := d.RecordAgentStatus("mac", st, now); err != nil {
		t.Fatal(err)
	}
	a, _ := d.GetAgentRow("mac")
	if a.Status == nil {
		t.Fatal("no status stored")
	}
	if a.LastStatusAt == nil || !a.LastStatusAt.Equal(now) {
		t.Errorf("last status at = %v", a.LastStatusAt)
	}
	probs := a.Status.Problems()
	if len(probs) != 2 {
		t.Fatalf("problems = %+v, want the two failures and not the passing check", probs)
	}
	if !probs[0].NeedsOperator {
		t.Errorf("problems[0] = %q; the one needing a person must come first", probs[0].Name)
	}
	if len(a.Status.Workspaces) != 1 || a.Status.Workspaces[0].Used == nil {
		t.Errorf("workspaces = %+v", a.Status.Workspaces)
	}
}

// A status report must not be able to displace a pause or a sighting.
func TestAStatusReportTouchesNothingElse(t *testing.T) {
	d := openTestDB(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	if err := d.RecordAgentSighting("mac", []string{"mac"}, "https",
		SelfReport{Version: "v1"}, now); err != nil {
		t.Fatal(err)
	}
	if err := d.PauseAgent("mac", now.Add(time.Hour), "moving it"); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordAgentStatus("mac", &models.AgentStatus{
		Checks: []models.AgentCheck{{Name: "disk", Detail: "fine", OK: true}}}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	a, _ := d.GetAgentRow("mac")
	if !a.Paused(now.Add(time.Minute)) || a.PauseNote != "moving it" {
		t.Error("a status report cleared the pause")
	}
	if a.Version != "v1" || !a.LastSeenAt.Equal(now) {
		t.Errorf("a status report disturbed the sighting: %+v", a)
	}
}

// An agent can report a problem before it has ever successfully claimed —
// which is the most interesting time for it to do so.
func TestAnUnseenAgentCanReportStatus(t *testing.T) {
	d := openTestDB(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	if err := d.RecordAgentStatus("never-claimed", &models.AgentStatus{
		Checks: []models.AgentCheck{{Name: "unity", Detail: "no licence", NeedsOperator: true}}}, now); err != nil {
		t.Fatal(err)
	}
	a, _ := d.GetAgentRow("never-claimed")
	if a == nil || a.Status == nil || len(a.Status.Problems()) != 1 {
		t.Errorf("row = %+v", a)
	}
	if a.FirstSeenAt != nil {
		t.Error("a status report invented a first contact")
	}
}

// The report is written by the machine being described, so it is bounded on
// arrival rather than trusted to be reasonable.
func TestAnAbsurdStatusIsTrimmedNotRejected(t *testing.T) {
	d := openTestDB(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	st := &models.AgentStatus{}
	for i := 0; i < models.MaxStatusChecks*4; i++ {
		st.Checks = append(st.Checks, models.AgentCheck{
			Name:   strings.Repeat("n", models.MaxStatusNameLen*2),
			Detail: strings.Repeat("é", models.MaxStatusDetailLen),
		})
	}
	for i := 0; i < models.MaxStatusWorkspaces*3; i++ {
		st.Workspaces = append(st.Workspaces, models.AgentWorkspace{Name: "w"})
	}
	if err := d.RecordAgentStatus("mac", st, now); err != nil {
		t.Fatal(err)
	}
	a, _ := d.GetAgentRow("mac")
	if a.Status == nil {
		t.Fatal("an oversized report was dropped entirely; the checks are the part an operator acts on")
	}
	if len(a.Status.Checks) > models.MaxStatusChecks {
		t.Errorf("stored %d checks, past the %d cap", len(a.Status.Checks), models.MaxStatusChecks)
	}
	for _, c := range a.Status.Checks {
		if len(c.Detail) > models.MaxStatusDetailLen || len(c.Name) > models.MaxStatusNameLen {
			t.Errorf("check not clamped: name=%d detail=%d", len(c.Name), len(c.Detail))
		}
		if !utf8.ValidString(c.Detail) || !utf8.ValidString(c.Name) {
			t.Error("clamping split a character and stored invalid UTF-8")
		}
	}
	if len(a.Status.Workspaces) > models.MaxStatusWorkspaces {
		t.Errorf("stored %d workspaces", len(a.Status.Workspaces))
	}
}

// Control characters and invalid bytes are stripped, so nothing reaches the
// page that could break a line of it.
func TestStatusTextIsLaundered(t *testing.T) {
	d := openTestDB(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	if err := d.RecordAgentStatus("mac", &models.AgentStatus{
		Checks: []models.AgentCheck{{Name: "x", Detail: "bad\x00\x07byte\xffhere\nsecond line"}},
	}, now); err != nil {
		t.Fatal(err)
	}
	a, _ := d.GetAgentRow("mac")
	got := a.Status.Checks[0].Detail
	if strings.ContainsAny(got, "\x00\x07\n") {
		t.Errorf("control characters survived: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("invalid UTF-8 survived: %q", got)
	}
}

func ptr(n int) *int { return &n }

// A disk that has genuinely filled reports 0 GB free. Treating that as "no
// reading" would leave the last healthy number on the page at the exact moment
// it stops being true — the most alarming reading there is, silently dropped.
func TestAFullDiskIsStoredRatherThanDiscarded(t *testing.T) {
	d := openTestDB(t)
	t0 := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	if err := d.RecordAgentSighting("mac", []string{"mac"}, "https",
		SelfReport{DiskFreeGB: ptr(210), DiskFloorGB: ptr(40)}, t0); err != nil {
		t.Fatal(err)
	}
	// statfs now says a few hundred megabytes; FreeGB truncates that to 0.
	if err := d.RecordAgentSighting("mac", []string{"mac"}, "https",
		SelfReport{DiskFreeGB: ptr(0), DiskFloorGB: ptr(40)}, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	a, _ := d.GetAgentRow("mac")
	if a.DiskFreeGB != 0 {
		t.Errorf("stored free = %d GB; the agent reported 0 and the page would show a stale, reassuring number", a.DiskFreeGB)
	}
	if a.DiskFloorGB != 40 {
		t.Errorf("floor = %d", a.DiskFloorGB)
	}

	// And an agent that reports nothing at all still leaves the reading alone.
	if err := d.RecordAgentSighting("mac", []string{"mac"}, "https",
		SelfReport{}, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	a, _ = d.GetAgentRow("mac")
	if a.DiskFloorGB != 40 {
		t.Errorf("an agent that reported nothing blanked the floor: %d", a.DiskFloorGB)
	}
}

// A negative figure is a broken measurement, not a disk.
func TestANegativeDiskReadingIsIgnored(t *testing.T) {
	d := openTestDB(t)
	t0 := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	if err := d.RecordAgentSighting("mac", []string{"mac"}, "https",
		SelfReport{DiskFreeGB: ptr(210), DiskFloorGB: ptr(40)}, t0); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordAgentSighting("mac", []string{"mac"}, "https",
		SelfReport{DiskFreeGB: ptr(-5), DiskFloorGB: ptr(40)}, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	a, _ := d.GetAgentRow("mac")
	if a.DiskFreeGB != 210 {
		t.Errorf("free = %d, want the last figure that made sense", a.DiskFreeGB)
	}
}
