package db

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/FanDoster/Build-System/internal/models"
)

func TestAgentPreferencesDefaultPartialPersistentAndSightingSafe(t *testing.T) {
	d := openTestDB(t)
	def := models.DefaultAgentNotificationPreferences()
	for _, name := range []string{"registry-only", "history-only", "future agent 🌍"} {
		got, err := d.AgentNotificationPreferences(name)
		if err != nil || got != def {
			t.Fatal(got, err)
		}
	}
	name := "future agent 🌍"
	got, err := d.UpdateAgentNotificationPreferences(name, map[string]bool{"availability": false})
	if err != nil || got.Availability || !got.Enabled || !got.Health || !got.Builds {
		t.Fatal(got, err)
	}
	if err = d.RecordAgentSighting(name, []string{"mac"}, "https", SelfReport{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = d.RecordAgentStatus(name, &models.AgentStatus{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = d.ForgetAgent(name); err != nil {
		t.Fatal(err)
	}
	if err = d.migrate(); err != nil {
		t.Fatal(err)
	}
	got, err = d.AgentNotificationPreferences(name)
	if err != nil || got.Availability {
		t.Fatal("self-report/forget/migration overwrote preference", got, err)
	}
	got, err = d.UpdateAgentNotificationPreferences(name, map[string]bool{"builds": false})
	if err != nil || got.Availability || got.Builds || !got.Enabled || !got.Health {
		t.Fatal(got, err)
	}
}

func TestAgentPreferencesMuteOnlyMatchedUnsentKindsAndFutureMaster(t *testing.T) {
	d := openTestDB(t)
	p := telegramProject(t, d, "categories")
	now := time.Now()
	for _, name := range []string{"target", "other", ""} {
		for _, kind := range []string{"availability", "health", "build", "agent", "future-kind"} {
			_, err := d.conn.Exec(`INSERT INTO notification_outbox(event_key,project_id,agent_name,event_kind,payload,created_at,next_attempt_at) VALUES(?,?,?,?,?,?,?)`, name+":"+kind, p.ID, name, kind, `{"text":"immutable"}`, now.Unix(), now.Unix())
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	_, err := d.conn.Exec(`INSERT INTO notification_outbox(event_key,project_id,agent_name,event_kind,state,relay_id,created_at,next_attempt_at) VALUES('receipt',?,'target','availability','submitted',?,?,?)`, p.ID, strings.Repeat("a", 32), now.Unix(), now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.UpdateAgentNotificationPreferences("target", map[string]bool{"availability": false}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = d.conn.QueryRow(`SELECT COUNT(*) FROM notification_outbox WHERE state='muted'`).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	if _, err = d.UpdateAgentNotificationPreferences("target", map[string]bool{"availability": true}); err != nil {
		t.Fatal(err)
	}
	if _, err = d.UpdateAgentNotificationPreferences("target", map[string]bool{"enabled": false}); err != nil {
		t.Fatal(err)
	}
	if err = d.conn.QueryRow(`SELECT COUNT(*) FROM notification_outbox WHERE state='muted'`).Scan(&count); err != nil || count != 5 {
		t.Fatal(count, err)
	}
	rows, err := d.DueNotifications(now, 25)
	if err != nil || len(rows) != 11 {
		t.Fatal(len(rows), err)
	}
	for _, n := range rows {
		can, err := d.NotificationCanSubmit(n.ID)
		if err != nil {
			t.Fatal(err)
		}
		if n.AgentName != "target" && !can {
			t.Fatal("unrelated or local muted", n)
		}
		if n.EventKey == "receipt" && n.State != "submitted" {
			t.Fatal("accepted receipt changed", n)
		}
	}
	// Even an unclassified future producer inserting while muted must fail the
	// final current-preference check, independently of suppression at save time.
	result, err := d.conn.Exec(`INSERT INTO notification_outbox(event_key,project_id,agent_name,event_kind,created_at,next_attempt_at) VALUES('late-future',?,'target','future-unclassified',?,?)`, p.ID, now.Unix(), now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	can, err := d.NotificationCanSubmit(id)
	if err != nil || can {
		t.Fatal("master did not guard future pending type", can, err)
	}
	late, err := d.GetNotification(id)
	if err != nil || late.State != "muted" {
		t.Fatal("gate did not permanently retire late event", late, err)
	}
	if _, err = d.UpdateAgentNotificationPreferences("target", map[string]bool{"enabled": true}); err != nil {
		t.Fatal(err)
	}
	if err = d.conn.QueryRow(`SELECT COUNT(*) FROM notification_outbox WHERE state='muted'`).Scan(&count); err != nil || count != 6 {
		t.Fatal("unmute revived old events", count, err)
	}
	can, err = d.NotificationCanSubmit(id)
	if err != nil || can {
		t.Fatal("unmute revived late future event", can, err)
	}
	late, err = d.GetNotification(id)
	if err != nil || late.State != "muted" {
		t.Fatal(late, err)
	}
}

func TestAgentBuildTriggerUsesActualOwnerAndRefreshes(t *testing.T) {
	d := openTestDB(t)
	p := telegramProject(t, d, "shared")
	p.Executor = "unity-windows"
	if err := d.UpdateProject(p); err != nil {
		t.Fatal(err)
	}
	if _, err := d.UpdateAgentNotificationPreferences("mac-owner", map[string]bool{"builds": false}); err != nil {
		t.Fatal(err)
	}
	// A queue name is NOT an owner; muting it cannot affect Linux or local rows.
	if _, err := d.UpdateAgentNotificationPreferences("unity-windows", map[string]bool{"enabled": false}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mac-owner", "linux-owner", ""} {
		b := &models.Build{ProjectID: p.ID, Status: models.StatusPending}
		if err := d.CreateBuild(b); err != nil {
			t.Fatal(err)
		}
		if _, err := d.conn.Exec(`UPDATE builds SET agent=?,status='success' WHERE id=?`, name, b.ID); err != nil {
			t.Fatal(err)
		}
	}
	if notificationCount(t, d) != 2 {
		t.Fatal("owner capture filtering failed")
	}
	if _, err := d.conn.Exec(`DROP TRIGGER notification_build_update; CREATE TRIGGER notification_build_update AFTER UPDATE OF status ON builds BEGIN SELECT 1; END;`); err != nil {
		t.Fatal(err)
	}
	if err := d.migrateNotifications(); err != nil {
		t.Fatal(err)
	}
	if notificationCount(t, d) != 2 {
		t.Fatal("migration replayed terminal history")
	}
	b := &models.Build{ProjectID: p.ID, Status: models.StatusPending}
	if err := d.CreateBuild(b); err != nil {
		t.Fatal(err)
	}
	if _, err := d.conn.Exec(`UPDATE builds SET agent='linux-owner',status='success' WHERE id=?`, b.ID); err != nil {
		t.Fatal(err)
	}
	if notificationCount(t, d) != 3 {
		t.Fatal("old trigger not refreshed")
	}
	rows, err := d.DueNotifications(time.Now().Add(time.Second), 25)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range rows {
		if n.EventKind != "build" || n.AgentName == "mac-owner" {
			t.Fatal(n)
		}
	}
	// Disabled result was observed too; another terminal write after unmute cannot replay it.
	if _, err = d.UpdateAgentNotificationPreferences("mac-owner", map[string]bool{"builds": true}); err != nil {
		t.Fatal(err)
	}
	if _, err = d.conn.Exec(`UPDATE builds SET status='failed' WHERE agent='mac-owner'`); err != nil {
		t.Fatal(err)
	}
	if notificationCount(t, d) != 3 {
		t.Fatal("unmute replayed suppressed result")
	}
}

func TestAgentMetadataUpgradeBackfillAndScopedBaseline(t *testing.T) {
	d := openTestDB(t)
	p := telegramProject(t, d, "legacy")
	now := time.Now()
	name := "real name 🌍 secret"
	payload := `{"text":"sanitized [redacted] NOT THE IDENTITY"}`
	if err := d.ObserveNotificationAgent(p.ID, name, "legacy-unhealthy", payload, true, now); err != nil {
		t.Fatal(err)
	}
	b := &models.Build{ProjectID: p.ID, Status: models.StatusPending}
	if err := d.CreateBuild(b); err != nil {
		t.Fatal(err)
	}
	if _, err := d.conn.Exec(`UPDATE builds SET agent=?,status='success' WHERE id=?`, name, b.ID); err != nil {
		t.Fatal(err)
	}
	// Genuine old schema had neither metadata column. Accepted rows are left alone.
	_, err := d.conn.Exec(`DROP TRIGGER notification_build_insert; DROP TRIGGER notification_build_update;
 DROP INDEX idx_notification_agent_kind; ALTER TABLE notification_outbox DROP COLUMN agent_name; ALTER TABLE notification_outbox DROP COLUMN event_kind;
 DELETE FROM app_settings WHERE key='notification_agent_metadata_v2';`)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.migrateNotifications(); err != nil {
		t.Fatal(err)
	}
	rows, err := d.DueNotifications(now.Add(time.Second), 25)
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	for _, n := range rows {
		if n.AgentName != name {
			t.Fatal("metadata identity lost", n)
		}
		if n.EventKind != "build" && n.EventKind != "agent" {
			t.Fatal(n)
		}
	}
	hash := sha256.Sum256([]byte(name))
	if !strings.Contains(rows[0].EventKey, fmt.Sprintf("%x", hash[:16])) {
		t.Fatal(rows[0])
	}
	for _, kind := range []string{"availability", "health"} {
		if err = d.ObserveNotificationAgentKind(p.ID, name, kind, "bad", payload, true, now); err != nil {
			t.Fatal(err)
		}
	}
	if notificationCount(t, d) != 2 {
		t.Fatal("legacy unhealthy replayed into new kinds")
	}
	if _, err = d.UpdateAgentNotificationPreferences(name, map[string]bool{"health": false}); err != nil {
		t.Fatal(err)
	}
	can, err := d.NotificationCanSubmit(rows[0].ID)
	if err != nil || can {
		t.Fatal("legacy combined event not conservatively suppressed", can, err)
	}
	can, err = d.NotificationCanSubmit(rows[1].ID)
	if err != nil || !can {
		t.Fatal("health mute affected build", can, err)
	}
	// New categories advance while muted, including recovery; unmute and restart do not replay.
	if err = d.ObserveNotificationAgentKind(p.ID, name, "health", "recovered", payload, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err = d.UpdateAgentNotificationPreferences(name, map[string]bool{"health": true}); err != nil {
		t.Fatal(err)
	}
	if err = d.migrate(); err != nil {
		t.Fatal(err)
	}
	if err = d.ObserveNotificationAgentKind(p.ID, name, "health", "recovered", payload, false, now); err != nil {
		t.Fatal(err)
	}
	if notificationCount(t, d) != 2 {
		t.Fatal("muted baseline replayed")
	}
	if err = d.ObserveNotificationAgentKind(p.ID, name, "health", "new incident", payload, true, now); err != nil {
		t.Fatal(err)
	}
	if notificationCount(t, d) != 3 {
		t.Fatal("new incident lost")
	}
}

func TestUnmuteRetiresLateFutureIntentBeforeAnyFinalGate(t *testing.T) {
	d := openTestDB(t)
	p := telegramProject(t, d, "late-unmute")
	now := time.Now()
	if _, err := d.UpdateAgentNotificationPreferences("target", map[string]bool{"enabled": false}); err != nil {
		t.Fatal(err)
	}
	result, err := d.conn.Exec(`INSERT INTO notification_outbox(event_key,project_id,agent_name,event_kind,payload,created_at,next_attempt_at)
 VALUES('late-before-unmute',?,'target','future-kind','{"text":"immutable"}',?,?)`, p.ID, now.Unix(), now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	// No worker or NotificationCanSubmit call occurs during the muted interval.
	if _, err = d.UpdateAgentNotificationPreferences("target", map[string]bool{"enabled": true}); err != nil {
		t.Fatal(err)
	}
	n, err := d.GetNotification(id)
	if err != nil || n.State != "muted" {
		t.Fatal("unmute replayed late future intent", n, err)
	}
	can, err := d.NotificationCanSubmit(id)
	if err != nil || can {
		t.Fatal("late future intent eligible after unmute", can, err)
	}
	if n.Payload != `{"text":"immutable"}` || n.EventKey != "late-before-unmute" {
		t.Fatal("immutable data changed", n)
	}
}
