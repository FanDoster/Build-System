package notifications

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/FanDoster/Build-System/internal/agents"
	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/models"
)

// Legacy log-free adapter retained for existing fleet callers/tests. Production
// notification observation below uses BuildHealth's narrower database surface;
// suppressing only the log tail would still read full build summaries.
type notificationAgentSource struct {
	agents.Source
	rows []db.AgentRow
}

func (s *notificationAgentSource) LogTailBytes(int64, int) ([]byte, error) { return nil, nil }

func (s *notificationAgentSource) ListAgentRows() ([]db.AgentRow, error) {
	rows, err := s.Source.ListAgentRows()
	s.rows = rows
	return rows, err
}

func (w *Worker) observeAgents(now time.Time) error {
	if w.Registry == nil {
		return nil
	}
	// Clock skew and duplicate-name evidence live only in the registry. Give
	// them time to rebuild before comparing with durable fingerprints, or a
	// redeploy would falsely announce recovery and then repeat the warning.
	if now.Sub(w.Registry.StartedAt()) < models.AgentHeartbeatTTL {
		return nil
	}
	fleet, err := agents.BuildHealth(w.DB, w.Registry, now)
	if err != nil {
		return err
	}
	if fleet.Degraded != "" {
		return fmt.Errorf("observe agents: %s", fleet.Degraded)
	}
	projects, err := w.DB.ListProjects()
	if err != nil {
		return err
	}

	for _, a := range fleet.Agents {
		if a.State == agents.StateWaiting {
			continue
		}
		fingerprint, unhealthy := agentFingerprint(a)
		for _, p := range projects {
			if !notificationAgentMatches(p, a) {
				continue
			}
			// Do not skip muted projects: their baseline must advance too, so
			// enabling notifications does not replay old incidents. The transaction
			// rechecks the current project preference before inserting an event.
			full, err := w.DB.GetProject(p.ID)
			if err != nil {
				return err
			}
			safeProject, safeAgent := w.sanitizeAgent(*full, a)
			if err := w.DB.ObserveNotificationAgent(p.ID, a.Name, fingerprint,
				w.projectPayload(safeProject, agentText(safeProject, safeAgent)), unhealthy, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// Scrub complete raw metadata before any field is bounded. Otherwise a
// credential crossing the bound would leak its prefix. Copy the doctor slice:
// the same fleet agent can serve several projects with different secrets.
func (w *Worker) sanitizeAgent(p models.Project, a agents.Agent) (models.Project, agents.Agent) {
	p.Name = w.scrub(p, p.Name)
	a.Name = w.scrub(p, a.Name)
	a.PauseNote = w.scrub(p, a.PauseNote)
	a.ClockSkew = w.scrub(p, a.ClockSkew)
	a.Problems = append([]models.AgentCheck(nil), a.Problems...)
	for i := range a.Problems {
		a.Problems[i].Name = w.scrub(p, a.Problems[i].Name)
		a.Problems[i].Detail = w.scrub(p, a.Problems[i].Detail)
	}
	return p, a
}

func notificationAgentMatches(p models.Project, a agents.Agent) bool {
	if !models.Remote(p.Executor) {
		return false
	}
	if _, ok := p.ProfileFor(a.Executors); ok {
		return true
	}
	return a.Current != nil && a.Current.Project == p.Name && a.Current.Executor == p.Executor
}

var (
	doctorDiskReadingRE  = regexp.MustCompile(`(?i)\b[0-9]+(?:\.[0-9]+)?[ \t]*(?:[kmgt]i?b)\b`)
	doctorFailureCountRE = regexp.MustCompile(`(?i)\b[0-9]+[ \t]+(?:consecutive[ \t]+)?(?:builds?|failures?)\b`)
)

// Only semantic changes belong in this digest. Poll age, heartbeat, uptime,
// busy/idle, exact free disk, skew magnitude and a growing failure streak do
// not create new incidents. Doctor findings are sorted to ignore report order.
func agentFingerprint(a agents.Agent) (string, bool) {
	type problem struct {
		Name     string
		Detail   string
		Operator bool
	}
	problems := make([]problem, 0, len(a.Problems))
	for _, p := range a.Problems {
		detail := cleanPlain(p.Detail, models.MaxStatusDetailLen)
		name := strings.ToLower(p.Name)
		if strings.Contains(name, "disk") || strings.Contains(name, "space") {
			detail = doctorDiskReadingRE.ReplaceAllString(detail, "disk reading")
		}
		if strings.Contains(name, "fail") || strings.Contains(detail, "in a row") || strings.Contains(detail, "consecutive") {
			detail = doctorFailureCountRE.ReplaceAllString(detail, "failure streak")
		}
		problems = append(problems, problem{cleanPlain(p.Name, models.MaxStatusNameLen), detail, p.NeedsOperator})
	}
	sort.Slice(problems, func(i, j int) bool {
		if problems[i].Name != problems[j].Name {
			return problems[i].Name < problems[j].Name
		}
		if problems[i].Detail != problems[j].Detail {
			return problems[i].Detail < problems[j].Detail
		}
		return !problems[i].Operator && problems[j].Operator
	})
	offline := a.State == agents.StateOffline
	insecure := a.Scheme == "http"
	state := struct {
		Offline, Paused, DiskLow, StatusStale, Duplicate, ClockOff, FailureRun, Insecure bool
		Problems                                                                         []problem
	}{offline, a.Paused, a.DiskLow, a.StatusStale, a.Duplicate, a.ClockOff, a.FailureRun, insecure, problems}
	canonical, _ := json.Marshal(state)
	digest := sha256.Sum256(canonical)
	unhealthy := offline || a.Paused || a.DiskLow || a.StatusStale || a.Duplicate || a.ClockOff || a.FailureRun || insecure || len(problems) > 0
	return hex.EncodeToString(digest[:]), unhealthy
}

func agentText(p models.Project, a agents.Agent) string {
	lines := []string{fmt.Sprintf("🤖 Agent %s · %s", cleanPlain(a.Name, 64), cleanPlain(p.Name, 120))}
	if a.State == agents.StateOffline {
		lines = append(lines, "Offline — no recent claim poll or build heartbeat. Check the agent process, machine power and network; restart the agent if needed.")
	} else {
		lines = append(lines, "Online — claim poll or build heartbeat is current.")
	}
	if a.Paused {
		line := "Paused — not taking new builds. Resume the agent or wait for the pause to expire."
		if a.PauseNote != "" {
			line += " Note: " + cleanPlain(a.PauseNote, models.MaxPauseNoteLen)
		}
		lines = append(lines, line)
	} else {
		lines = append(lines, "Resumed — not paused; may take new builds.")
	}
	if a.DiskLow {
		lines = append(lines, fmt.Sprintf("Low disk: %d GB free, floor %d — the next build is already refused. Free disk space above the configured floor.", a.DiskFreeGB, a.DiskFloorGB))
	}
	if a.FailureRun {
		lines = append(lines, fmt.Sprintf("%d builds in a row have failed here. If other machines are green, the fault is more likely this one than the code. Check the machine.", a.ConsecutiveFailures))
	}
	if a.StatusStale {
		lines = append(lines, "Health report is stale. Check the agent process and its status reporting; run the agent doctor on the machine.")
	}
	if a.Duplicate {
		lines = append(lines, "Duplicate name — two processes are claiming under this name at once. Ownership here is name equality, so either can write into the other's build. Give one of them a different agent_name.")
	}
	if a.ClockOff {
		lines = append(lines, "Clock skew "+cleanPlain(a.ClockSkew, 40)+" — this agent's clock disagrees with the server's. Build log timestamps and the duration of a build's last step will be wrong. Synchronise the machine clock with NTP.")
	}
	if a.Scheme == "http" {
		lines = append(lines, "Not encrypted — this agent's requests arrived unencrypted. Its token has crossed the network in clear and should be rotated. Use HTTPS.")
	}
	if _, unhealthy := agentFingerprint(a); !unhealthy {
		lines = append(lines, "Healthy — no current fleet warnings; any previous issue has cleared.")
	}
	// Keep every fleet warning and its remedy ahead of bounded doctor
	// excerpts, so a long report cannot push another issue off the message.
	count := len(a.Problems)
	if count > 8 {
		count = 8
	}
	remaining := len(a.Problems) - count
	reserve := 0
	if remaining > 0 {
		reserve = 80
	}
	budget := 3200 - len(utf16.Encode([]rune(strings.Join(lines, "\n")))) - reserve
	for i, problem := range a.Problems[:count] {
		label := "Doctor problem"
		if problem.NeedsOperator {
			label = "Doctor needs operator"
		}
		prefix := fmt.Sprintf("%s · %s: ", label, cleanPlain(problem.Name, models.MaxStatusNameLen))
		allowance := budget/(count-i) - len(utf16.Encode([]rune(prefix))) - 1
		if allowance > 250 {
			allowance = 250
		}
		if allowance < 1 {
			allowance = 1
		}
		line := prefix + cleanPlain(problem.Detail, allowance)
		lines = append(lines, line)
		budget -= len(utf16.Encode([]rune(line))) + 1
	}
	if remaining > 0 {
		lines = append(lines, fmt.Sprintf("%d more doctor problems; run the agent doctor for the full report.", remaining))
	}
	return strings.Join(lines, "\n")
}
