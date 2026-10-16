package notifications

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/FanDoster/Build-System/internal/agents"
	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/models"
)

func TestAgentFingerprintSemanticChanges(t *testing.T) {
	healthy := agents.Agent{State: agents.StateOnline}
	base, unhealthy := agentFingerprint(healthy)
	if unhealthy {
		t.Fatal("healthy startup must be silent")
	}
	busy := healthy
	busy.State = agents.StateBusy
	busy.Current = &agents.CurrentBuild{ID: 42}
	busy.Uptime, busy.SeenAgo = "3d", "1s"
	busy.DiskFreeGB, busy.DiskFloorGB = 100, 10
	busy.ConsecutiveFailures = 2
	busy.ClockKnown = true
	if got, _ := agentFingerprint(busy); got != base {
		t.Fatal("normal activity changed fingerprint")
	}
	cases := map[string]func(*agents.Agent){
		"offline":      func(a *agents.Agent) { a.State = agents.StateOffline },
		"paused":       func(a *agents.Agent) { a.Paused = true },
		"disk":         func(a *agents.Agent) { a.DiskLow = true },
		"stale health": func(a *agents.Agent) { a.StatusStale = true },
		"duplicate":    func(a *agents.Agent) { a.Duplicate = true },
		"clock":        func(a *agents.Agent) { a.ClockOff = true },
		"failures":     func(a *agents.Agent) { a.FailureRun = true },
		"insecure":     func(a *agents.Agent) { a.Scheme = "http" },
		"doctor":       func(a *agents.Agent) { a.Problems = []models.AgentCheck{{Name: "git-lfs", Detail: "Install git-lfs"}} },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			a := healthy
			change(&a)
			got, bad := agentFingerprint(a)
			if !bad || got == base {
				t.Fatal("issue did not change health")
			}
			recovered, bad := agentFingerprint(healthy)
			if bad || recovered != base || recovered == got {
				t.Fatal("recovery not represented")
			}
			a.ConsecutiveFailures = 10
			a.DiskFreeGB = 1
			a.ClockSkew = "+12h"
			a.PauseNote = "updated note"
			if again, _ := agentFingerprint(a); again != got {
				t.Fatal("changing issue measurement caused churn")
			}
		})
	}
	a := healthy
	a.Problems = []models.AgentCheck{{Name: "a", Detail: "fix A"}, {Name: "b", Detail: "fix B", NeedsOperator: true}}
	one, _ := agentFingerprint(a)
	a.Problems[0], a.Problems[1] = a.Problems[1], a.Problems[0]
	two, _ := agentFingerprint(a)
	if one != two {
		t.Fatal("doctor report order caused churn")
	}
	a.Problems[0].Detail = "a new remedy"
	three, _ := agentFingerprint(a)
	if three == two {
		t.Fatal("changed doctor findings were ignored")
	}
}

func TestAgentFingerprintDoctorMeasurementChurn(t *testing.T) {
	a := agents.Agent{State: agents.StateOnline, DiskLow: true, FailureRun: true,
		Problems: []models.AgentCheck{{Name: "disk", Detail: "8 GB free, floor 40 GB. Free disk space."},
			{Name: "failures", Detail: "3 builds in a row have failed. Check machine."}}}
	first, _ := agentFingerprint(a)
	a.Problems[0].Detail = "7 GB free, floor 40 GB. Free disk space."
	a.Problems[1].Detail = "4 builds in a row have failed. Check machine."
	if second, _ := agentFingerprint(a); second != first {
		t.Fatal("doctor measurements caused notification churn")
	}
	a.Problems[0].Detail += " Run disk cleanup."
	if changed, _ := agentFingerprint(a); changed == first {
		t.Fatal("doctor remedy change ignored")
	}
}

func TestNotificationAgentMatchesProfilesAndExactOwnership(t *testing.T) {
	p := models.Project{Name: "game", Executor: "unity-windows", ExecutorProfiles: map[string]models.ExecutorProfile{"mac": {}, "linux": {}}}
	for _, profile := range []string{"mac", "linux"} {
		if !notificationAgentMatches(p, agents.Agent{Executors: []string{profile}}) {
			t.Fatalf("%s profile excluded", profile)
		}
	}
	if notificationAgentMatches(p, agents.Agent{Executors: []string{"unity-windows"}}) {
		t.Fatal("logical queue bypassed profile allowlist")
	}
	for _, current := range []*agents.CurrentBuild{
		{Project: "sibling", Executor: "unity-windows"}, {Project: "game", Executor: "other"},
	} {
		if notificationAgentMatches(p, agents.Agent{Current: current}) {
			t.Fatal("ownership inferred eligibility for another project/queue")
		}
	}
	if !notificationAgentMatches(p, agents.Agent{Current: &agents.CurrentBuild{Project: "game", Executor: "unity-windows"}}) {
		t.Fatal("exact current ownership must match after restart")
	}
	p.Executor = models.ExecutorLocal
	if notificationAgentMatches(p, agents.Agent{Executors: []string{"mac"}}) {
		t.Fatal("local project matched remote agent")
	}
}

// This fake panics if assembly ever asks for a log. The wrapper is the only
// path through Build, even when a running build has a current step to find.
type notificationFleetFixture struct {
	rows    []db.AgentRow
	running *models.Build
}

func (f *notificationFleetFixture) AgentNames() ([]string, error) { return []string{"machine"}, nil }
func (f *notificationFleetFixture) RunningBuildForAgent(string) (*models.Build, error) {
	return f.running, nil
}
func (f *notificationFleetFixture) RecentBuildsForAgent(string, int) ([]models.Build, error) {
	return nil, nil
}
func (f *notificationFleetFixture) RemoteExecutors() ([]db.RemoteExecutor, error) { return nil, nil }
func (f *notificationFleetFixture) ListAgentRows() ([]db.AgentRow, error)         { return f.rows, nil }
func (f *notificationFleetFixture) LogTailBytes(int64, int) ([]byte, error) {
	panic("notifications must not read logs")
}

func TestNotificationFleetRestartAndHeartbeatWithoutLogs(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	reg := agents.NewRegistry()
	reg.SetClock(func() time.Time { return now })
	old := now.Add(-time.Hour)
	fixture := &notificationFleetFixture{rows: []db.AgentRow{{Name: "machine", LastSeenAt: &old, Executors: []string{"mac"}}}}
	src := &notificationAgentSource{Source: fixture}
	fleet, err := agents.Build(src, reg, now)
	if err != nil {
		t.Fatal(err)
	}
	if fleet.Agents[0].State != agents.StateWaiting || fleet.Agents[0].Known {
		t.Fatal("persisted row bypassed restart grace")
	}
	now = now.Add(models.AgentHeartbeatTTL)
	fleet, err = agents.Build(src, reg, now)
	if err != nil {
		t.Fatal(err)
	}
	if fleet.Agents[0].State != agents.StateOffline {
		t.Fatal("unseen agent must become offline after grace")
	}
	fixture.running = &models.Build{ID: 1, ProjectName: "game", Executor: "unity-windows", LastHeartbeatAt: &now}
	fleet, err = agents.Build(src, reg, now)
	if err != nil {
		t.Fatal(err)
	}
	if fleet.Agents[0].State != agents.StateBusy {
		t.Fatal("build heartbeat did not prove liveness")
	}
	if fleet.Agents[0].Current.Step != "" {
		t.Fatal("notification fleet should have no log-derived step")
	}
}

func TestAgentTextIncludesIssuesRemediesAndBounds(t *testing.T) {
	detail := "Install git-lfs and check my_tool, `doctor` and /Users/build_agent/bin in the LaunchAgent PATH."
	a := agents.Agent{Name: "mac-one", State: agents.StateOffline, Paused: true, PauseNote: "maintenance", DiskLow: true, DiskFreeGB: 1, DiskFloorGB: 40,
		FailureRun: true, ConsecutiveFailures: 3, StatusStale: true, Duplicate: true, ClockOff: true, ClockSkew: "+2m", Scheme: "http",
		Problems: []models.AgentCheck{{Name: "git-lfs", Detail: detail, NeedsOperator: true}}}
	text := agentText(models.Project{Name: "game"}, a)
	if !strings.HasPrefix(text, "🤖") {
		t.Fatal("agent message must start with robot emoji")
	}
	for _, want := range []string{"Offline", "Paused", "maintenance", "Free disk space", "Check the machine", detail,
		"Health report is stale", "Give one of them a different agent_name", "NTP", "Use HTTPS"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing issue or remedy %q", want)
		}
	}
	for i := 0; i < 32; i++ {
		a.Problems = append(a.Problems, models.AgentCheck{Name: strings.Repeat("x", 64), Detail: strings.Repeat("😀", 500)})
	}
	text = agentText(models.Project{Name: strings.Repeat("p", 200)}, a)
	if n := len(utf16.Encode([]rune(text))); n > 3300 {
		t.Fatalf("text exceeds transport budget: %d", n)
	}
	if !strings.Contains(text, "more doctor problems") {
		t.Fatal("truncated report must disclose omitted checks")
	}
	text = agentText(models.Project{Name: "game"}, agents.Agent{Name: "mac-one", State: agents.StateOnline})
	if !strings.Contains(text, "Resumed") || !strings.Contains(text, "previous issue has cleared") {
		t.Fatal("healthy recovery lacks context")
	}
}

func TestObserveAgentsPersistedTransitionsAndMutedBaseline(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	p := &models.Project{Name: "game", Executor: "unity-windows", TelegramNotifications: true, ExecutorProfiles: map[string]models.ExecutorProfile{"mac": {}}}
	if err := d.CreateProject(p); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	reg := agents.NewRegistry()
	reg.SetClock(func() time.Time { return now })
	w := &Worker{DB: d, Registry: reg}
	done := reg.PollStarted("machine", []string{"mac"}, "https")
	defer done()
	if err := d.RecordAgentSighting("machine", []string{"mac"}, "https", db.SelfReport{}, now); err != nil {
		t.Fatal(err)
	}
	observe := func(want int) {
		t.Helper()
		if err := w.observeAgents(now); err != nil {
			t.Fatal(err)
		}
		events, err := d.DueNotifications(now, 25)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != want {
			t.Fatalf("got %d events, want %d", len(events), want)
		}
	}
	observe(0) // startup grace
	now = now.Add(models.AgentHeartbeatTTL)
	observe(0) // initial healthy baseline
	until := now.Add(time.Hour)
	if err := d.PauseAgent("machine", until, "maintenance"); err != nil {
		t.Fatal(err)
	}
	observe(1)
	observe(1) // no per-sweep duplicate
	if err := d.ResumeAgent("machine"); err != nil {
		t.Fatal(err)
	}
	observe(2)
	p.TelegramNotifications = false
	if err := d.UpdateProject(p); err != nil {
		t.Fatal(err)
	}
	if err := d.PauseAgent("machine", until, "maintenance"); err != nil {
		t.Fatal(err)
	}
	observe(0) // muting suppresses old pending events; observation still advances
	p.TelegramNotifications = true
	if err := d.UpdateProject(p); err != nil {
		t.Fatal(err)
	}
	observe(0) // enabling must not replay it
	// Registry replacement is a process restart; durable paused fingerprint
	// survives. Neither waiting/grace nor the unchanged paused state replays it.
	reg = agents.NewRegistry()
	reg.SetClock(func() time.Time { return now })
	w.Registry = reg
	observe(0)
	now = now.Add(models.AgentHeartbeatTTL)
	reg.PollStarted("machine", []string{"mac"}, "https")()
	observe(0)
	if err := d.ResumeAgent("machine"); err != nil {
		t.Fatal(err)
	}
	observe(1)
}

func TestObserveAgentsDoesNotInferSiblingCapabilityFromOwnership(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "ownership.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	projects := []*models.Project{
		{Name: "current", Executor: "mac", TelegramNotifications: true},
		{Name: "sibling", Executor: "mac", TelegramNotifications: true},
	}
	for _, p := range projects {
		if err := d.CreateProject(p); err != nil {
			t.Fatal(err)
		}
	}
	b := &models.Build{ProjectID: projects[0].ID, Status: models.StatusPending}
	if err := d.CreateBuild(b); err != nil {
		t.Fatal(err)
	}
	if claimed, err := d.ClaimBuildForAgent("owner", []string{"mac"}); err != nil || claimed == nil {
		t.Fatalf("claim: %v, %v", claimed, err)
	}
	now := time.Now().UTC()
	reg := agents.NewRegistry()
	reg.SetClock(func() time.Time { return now })
	w := &Worker{DB: d, Registry: reg}
	if err := d.PauseAgent("owner", now.Add(time.Hour), "maintenance"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(models.AgentHeartbeatTTL)
	if err := w.observeAgents(now); err != nil {
		t.Fatal(err)
	}
	events, err := d.DueNotifications(now, 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("expected separate availability and health incidents: %+v", events)
	}
	for _, event := range events {
		if event.ProjectID != projects[0].ID {
			t.Fatalf("ownership leaked to sibling: %+v", events)
		}
	}
}

func TestAgentPayloadScrubsProjectSecretsAndUntrustedMetadata(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "redaction.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	p := &models.Project{Name: "game", Executor: "mac", CloneToken: "clone_secret_2026", WebhookSecret: "webhook_secret_2026"}
	if err := d.CreateProject(p); err != nil {
		t.Fatal(err)
	}
	sanitized, err := d.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	a := agents.Agent{Name: "machine", State: agents.StatePaused, Paused: true,
		PauseNote: "clone_secret_2026 webhook_secret_2026 server_secret_2026 https://private.invalid/path?key=hidden",
		Problems:  []models.AgentCheck{{Name: "doctor", Detail: "Fix **this**\nthen visit https://private.invalid/path?key=hidden or use ghp_0123456789abcdef"}}}
	w := &Worker{DB: d, secrets: []string{"server_secret_2026"}}
	body := w.projectPayload(sanitized[0], agentText(sanitized[0], a))
	var decoded struct{ Text, Format string }
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Format != "plain" || !strings.HasPrefix(decoded.Text, "🤖") {
		t.Fatal("invalid agent message format or heading")
	}
	if !strings.Contains(decoded.Text, "**this**") {
		t.Fatal("plain doctor text should retain literal formatting characters")
	}
	for _, secret := range []string{"clone_secret_2026", "clone＿secret＿2026", "webhook_secret_2026", "webhook＿secret＿2026",
		"server_secret_2026", "server＿secret＿2026", "private.invalid", "ghp_0123456789abcdef"} {
		if strings.Contains(decoded.Text, secret) {
			t.Errorf("untrusted metadata or secret leaked: %q", secret)
		}
	}
}

func TestSanitizeAgentBeforeBoundWithoutMutatingSharedProblems(t *testing.T) {
	secret := "super_private_clone_credential_0123456789"
	p := models.Project{Name: "game", CloneToken: secret}
	w := &Worker{secrets: []string{"server_private_credential_0123456789"}}
	detail := strings.Repeat("x", 245) + secret + "; run doctor"
	original := agents.Agent{Name: "machine", State: agents.StateOnline,
		Problems: []models.AgentCheck{{Name: "doctor", Detail: detail}}}
	safeProject, safeAgent := w.sanitizeAgent(p, original)
	text := agentText(safeProject, safeAgent)
	if strings.Contains(text, "super") {
		t.Fatal("secret prefix crossed doctor bound")
	}
	if original.Problems[0].Detail != detail {
		t.Fatal("sanitizing one project mutated the shared fleet doctor slice")
	}
	other := models.Project{Name: "other"}
	_, otherAgent := w.sanitizeAgent(other, original)
	if otherAgent.Problems[0].Detail != detail {
		t.Fatal("project-specific redaction leaked into another project observation")
	}
}
