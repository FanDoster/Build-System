package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/FanDoster/Build-System/internal/agents"
	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/models"
)

func TestAgentKindsHaveIndependentFingerprintsAndPayloads(t *testing.T) {
	a := agents.Agent{Name: "machine", State: agents.StateOnline}
	availability, _ := agentKindFingerprint(a, "availability")
	health, _ := agentKindFingerprint(a, "health")
	a.State = agents.StateOffline
	if got, _ := agentKindFingerprint(a, "health"); got != health {
		t.Fatal("offline changed health")
	}
	if got, bad := agentKindFingerprint(a, "availability"); got == availability || !bad {
		t.Fatal("offline not observed")
	}
	offline, _ := agentKindFingerprint(a, "availability")
	a.Paused = true
	a.PauseNote = "private health note"
	a.DiskLow = true
	a.StatusStale = true
	a.ClockOff = true
	a.Duplicate = true
	a.FailureRun = true
	a.Scheme = "http"
	a.Problems = []models.AgentCheck{{Name: "doctor", Detail: "private health remedy"}}
	if got, _ := agentKindFingerprint(a, "availability"); got != offline {
		t.Fatal("health changed availability")
	}
	if got, bad := agentKindFingerprint(a, "health"); got == health || !bad {
		t.Fatal("health warning lost")
	}
	p := models.Project{Name: "project"}
	text := agentKindText(p, a, "availability")
	for _, muted := range []string{"Paused", "Resumed", "health note", "health remedy", "Low disk", "stale", "Clock skew", "Duplicate", "failed", "HTTPS"} {
		if strings.Contains(text, muted) {
			t.Fatalf("availability payload leaked health %q: %s", muted, text)
		}
	}
	text = agentKindText(p, a, "health")
	if strings.Contains(text, "Offline") || strings.Contains(text, "Online") {
		t.Fatal("health payload leaked availability", text)
	}
	if !strings.Contains(text, "health remedy") || !strings.Contains(text, "Paused") {
		t.Fatal(text)
	}
}

func TestObserveAgentCategoryMuteKeepsOtherCategoryAndNoReplay(t *testing.T) {
	d, _, p := workerDB(t)
	p.Executor = "mac"
	if err := d.UpdateProject(p); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	reg := agents.NewRegistry()
	reg.SetClock(func() time.Time { return now })
	w := &Worker{DB: d, Registry: reg}
	if err := d.RecordAgentSighting("machine", []string{"mac"}, "https", db.SelfReport{}, now); err != nil {
		t.Fatal(err)
	}
	reg.PollStarted("machine", []string{"mac"}, "https")()
	now = now.Add(models.AgentHeartbeatTTL)
	// Fresh poll after startup establishes healthy baselines.
	reg.PollStarted("machine", []string{"mac"}, "https")()
	if err := w.observeAgents(now); err != nil {
		t.Fatal(err)
	}
	if _, err := d.UpdateAgentNotificationPreferences("machine", map[string]bool{"availability": false}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(models.AgentHeartbeatTTL + time.Second)
	if err := d.PauseAgent("machine", now.Add(time.Hour), "maintenance"); err != nil {
		t.Fatal(err)
	}
	if err := w.observeAgents(now); err != nil {
		t.Fatal(err)
	}
	due, err := d.DueNotifications(now, 25)
	if err != nil || len(due) != 1 || due[0].EventKind != "health" || strings.Contains(due[0].Payload, "Offline") {
		t.Fatal(due, err)
	}
	if _, err = d.UpdateAgentNotificationPreferences("machine", map[string]bool{"availability": true, "health": false}); err != nil {
		t.Fatal(err)
	}
	if err = w.observeAgents(now); err != nil {
		t.Fatal(err)
	}
	due, err = d.DueNotifications(now, 25)
	if err != nil || len(due) != 0 {
		t.Fatal("unmute replayed offline", due, err)
	}
	// Recovery while health is muted emits only online, no paused details.
	reg.PollStarted("machine", []string{"mac"}, "https")()
	if err = w.observeAgents(now); err != nil {
		t.Fatal(err)
	}
	due, err = d.DueNotifications(now, 25)
	if err != nil || len(due) != 1 || due[0].EventKind != "availability" || strings.Contains(due[0].Payload, "Paused") {
		t.Fatal(due, err)
	}
	if err = d.ResumeAgent("machine"); err != nil {
		t.Fatal(err)
	}
	if err = w.observeAgents(now); err != nil {
		t.Fatal(err)
	}
	if _, err = d.UpdateAgentNotificationPreferences("machine", map[string]bool{"health": true}); err != nil {
		t.Fatal(err)
	}
	if err = w.observeAgents(now); err != nil {
		t.Fatal(err)
	}
	due, err = d.DueNotifications(now, 25)
	if err != nil || len(due) != 1 {
		t.Fatal("health recovery replayed after unmute", due, err)
	}
}

func TestAgentMuteDuringRouteRecheckAndAcceptedReceipt(t *testing.T) {
	for _, category := range []string{"availability", "health", "build"} {
		t.Run(category, func(t *testing.T) {
			d, _, p := workerDB(t)
			f := &relayFixture{}
			w, _ := fixtureWorker(t, d, f)
			now := time.Now().Add(time.Second)
			if category == "build" {
				p.Executor = "mac"
				if err := d.UpdateProject(p); err != nil {
					t.Fatal(err)
				}
				b := &models.Build{ProjectID: p.ID, Status: models.StatusPending}
				if err := d.CreateBuild(b); err != nil {
					t.Fatal(err)
				}
				if claimed, err := d.ClaimBuildForAgent("target", []string{"mac"}); err != nil || claimed == nil {
					t.Fatal(claimed, err)
				}
				if err := d.FinishBuild(b.ID, models.StatusSuccess); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := d.ObserveNotificationAgentKind(p.ID, "target", category, "bad", `{"text":"immutable"}`, true, now); err != nil {
					t.Fatal(err)
				}
			}
			due, err := d.DueNotifications(now, 25)
			if err != nil || len(due) != 1 {
				t.Fatal(due, err)
			}
			stale := due[0]
			f.onRoute = func() {
				patch := map[string]bool{"enabled": false}
				if category == "availability" {
					patch = map[string]bool{"availability": false}
				}
				if category == "health" {
					patch = map[string]bool{"health": false}
				}
				if category == "build" {
					patch = map[string]bool{"builds": false}
				}
				if _, err := d.UpdateAgentNotificationPreferences("target", patch); err != nil {
					t.Error(err)
				}
				// Even immediate unmute cannot revive this intent.
				if _, err := d.UpdateAgentNotificationPreferences("target", map[string]bool{"enabled": true, "availability": true, "health": true, "builds": true}); err != nil {
					t.Error(err)
				}
			}
			if err = w.process(context.Background(), now, stale); err != nil {
				t.Fatal(err)
			}
			posts, _ := f.snapshot()
			if len(posts) != 0 {
				t.Fatal("posted stale muted snapshot", posts)
			}
			n, err := d.GetNotification(stale.ID)
			if err != nil || n.State != "muted" {
				t.Fatal(n, err)
			}
			// A new incident can POST. Muting in the request cannot recall acceptance.
			f.change(func() {
				f.onRoute = nil
				f.onPost = func() {
					if _, err := d.UpdateAgentNotificationPreferences("target", map[string]bool{"enabled": false}); err != nil {
						t.Error(err)
					}
				}
			})
			if err = d.ObserveNotificationAgentKind(p.ID, "target", "health", "new", `{"text":"new immutable"}`, true, now); err != nil {
				t.Fatal(err)
			}
			// Target was unmuted by prior route callback, so this is a new event.
			due, err = d.DueNotifications(now, 25)
			if err != nil || len(due) != 1 {
				t.Fatal(due, err)
			}
			if err = w.process(context.Background(), now, due[0]); err != nil {
				t.Fatal(err)
			}
			n, err = d.GetNotification(due[0].ID)
			if err != nil || n.State != "submitted" {
				t.Fatal(n, err)
			}
			if err = w.process(context.Background(), now.Add(time.Minute), *n); err != nil {
				t.Fatal(err)
			}
			n, err = d.GetNotification(n.ID)
			if err != nil || n.State != "delivered" {
				t.Fatal(n, err)
			}
			var body map[string]any
			if json.Unmarshal([]byte(n.Payload), &body) != nil {
				t.Fatal("payload damaged")
			}
		})
	}
}

// A future producer may insert an intent after the preference save already
// suppressed the existing queue. The final gate must retire it, not just defer
// it until unmute. Exercise the real worker path with an unknown event kind.
func TestLateFutureIntentWhileMutedNeverReplaysAfterUnmute(t *testing.T) {
	for _, processWhileMuted := range []bool{false, true} {
		t.Run(fmt.Sprintf("worker-sees-muted-%t", processWhileMuted), func(t *testing.T) {
			d, path, p := workerDB(t)
			f := &relayFixture{}
			w, _ := fixtureWorker(t, d, f)
			now := time.Now().Add(time.Second)
			if _, err := d.UpdateAgentNotificationPreferences("target", map[string]bool{"enabled": false}); err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			result, err := raw.Exec(`INSERT INTO notification_outbox(event_key,project_id,agent_name,event_kind,payload,created_at,next_attempt_at)
 VALUES('late-future-worker',?,'target','future-kind','{"text":"immutable future message","format":"plain"}',?,?)`, p.ID, now.Unix(), now.Unix())
			if err != nil {
				t.Fatal(err)
			}
			id, err := result.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			stale, err := d.GetNotification(id)
			if err != nil {
				t.Fatal(err)
			}
			if processWhileMuted {
				if err = w.process(context.Background(), now, *stale); err != nil {
					t.Fatal(err)
				}
				n, err := d.GetNotification(id)
				if err != nil || n.State != "muted" {
					t.Fatal("muted future intent was not retired", n, err)
				}
			}
			if posts, _ := f.snapshot(); len(posts) != 0 {
				t.Fatal("posted while muted", posts)
			}
			if _, err = d.UpdateAgentNotificationPreferences("target", map[string]bool{"enabled": true}); err != nil {
				t.Fatal(err)
			}
			if err = w.process(context.Background(), now, *stale); err != nil {
				t.Fatal(err)
			}
			sweep(t, w, now.Add(time.Minute))
			if posts, _ := f.snapshot(); len(posts) != 0 {
				t.Fatal("unmute replayed future intent", posts)
			}
			can, err := d.NotificationCanSubmit(id)
			if err != nil || can {
				t.Fatal("unmute restored eligibility", can, err)
			}
			n, err := d.GetNotification(id)
			if err != nil || n.State != "muted" || n.Payload != stale.Payload || n.EventKey != stale.EventKey {
				t.Fatal("retirement changed immutable payload/key", n, err)
			}
		})
	}
}
