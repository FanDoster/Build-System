package api

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/FanDoster/Build-System/internal/agents"
	"github.com/FanDoster/Build-System/internal/auth"
	"github.com/FanDoster/Build-System/internal/models"
)

func TestAgentNotificationAPIRequiresOperatorAndCSRF(t *testing.T) {
	s, mux := newTestServer(t)
	a, err := auth.New(s.DB, "operator-password", "", "")
	if err != nil {
		t.Fatal(err)
	}
	a.SetAgentToken("agent-token")
	wrapped := a.Middleware(mux)
	for _, tc := range []struct {
		method, token, csrf string
		want                int
	}{
		{"GET", "", "", 401}, {"GET", "agent-token", "", 403}, {"PUT", "agent-token", "1", 403}, {"PUT", "operator-password", "", 403}, {"PUT", "operator-password", "1", 200}, {"GET", "operator-password", "", 200},
	} {
		req := httptest.NewRequest(tc.method, "/api/agents/machine/notifications", strings.NewReader(`{"availability":false}`))
		req.Header.Set("Authorization", "Bearer "+tc.token)
		req.Header.Set("X-Builds-Csrf", tc.csrf)
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("%+v: %d %s", tc, rec.Code, rec.Body)
		}
	}
}

func TestAgentNotificationAPIStrictPartialAndFleetDefaults(t *testing.T) {
	s, mux := newTestServer(t)
	s.Agents = agents.NewRegistry()
	name := "Build Mac 🌍 new"
	path := "/api/agents/" + url.PathEscape(name) + "/notifications"
	for _, body := range []string{`null`, `[]`, `true`, `{"enabled":null}`, `{"health":"false"}`, `{"builds":0}`, `{"Enabled":false}`, `{"surprise":true}`, `{"health":false} {}`, `{"health":true}junk`} {
		req := httptest.NewRequest("PUT", path, strings.NewReader(body))
		req.Header.Set("X-Builds-Csrf", "1")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Fatalf("accepted %s: %d %s", body, rec.Code, rec.Body)
		}
	}
	get := doJSON(t, mux, "GET", path, nil)
	if get.Code != 200 || !strings.Contains(get.Body.String(), `"enabled":true,"availability":true,"health":true,"builds":true`) {
		t.Fatal(get.Code, get.Body)
	}
	for _, body := range []map[string]any{{"availability": false}, {"builds": false}, {"health": true}} {
		rec := doJSON(t, mux, "PUT", path, body)
		if rec.Code != 200 {
			t.Fatal(rec.Code, rec.Body)
		}
	}
	prefs, err := s.DB.AgentNotificationPreferences(name)
	if err != nil || prefs.Availability || prefs.Builds || !prefs.Health || !prefs.Enabled {
		t.Fatal(prefs, err)
	}
	// Registry-only and history-only agents must default on without an agents row.
	s.Agents.PollStarted("registry-only", []string{"mac"}, "https")()
	project := createProject(t, s, models.Project{Name: "history", RepoURL: "x", ImageName: "history"})
	b := &models.Build{ProjectID: project.ID, Status: models.StatusPending}
	if err = s.DB.CreateBuild(b); err != nil {
		t.Fatal(err)
	}
	project.Executor = "mac"
	if err = s.DB.UpdateProject(project); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.ClaimBuildForAgent("history-only", []string{"mac"}); err != nil {
		t.Fatal(err)
	}
	s.Agents.PollStarted(name, []string{"mac"}, "https")()
	fleet, err := agents.Build(s.DB, s.Agents, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range fleet.Agents {
		want := models.DefaultAgentNotificationPreferences()
		if agent.Name == name {
			want = prefs
		}
		if agent.TelegramNotifications != want {
			t.Fatal(agent.Name, agent.TelegramNotifications, want)
		}
	}
	if len(fleet.Agents) != 3 {
		t.Fatal(fleet.Agents)
	}
}
