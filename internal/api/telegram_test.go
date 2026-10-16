package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/FanDoster/Build-System/internal/auth"
	"github.com/FanDoster/Build-System/internal/models"
)

func TestTelegramProjectCreateDefaultsAndExplicitValues(t *testing.T) {
	for _, tc := range []struct {
		name     string
		value    interface{}
		supplied bool
		want     bool
	}{
		{name: "omitted", want: true},
		{name: "enabled", supplied: true, value: true, want: true},
		{name: "muted", supplied: true, value: false, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, mux := newTestServer(t)
			input := map[string]interface{}{
				"name": "telegram", "repo_url": "https://github.com/u/app", "image_name": "telegram",
				"clone_token": "private-clone-secret", "webhook_secret": "private-webhook-secret",
			}
			if tc.supplied {
				input["telegram_notifications"] = tc.value
			}
			w := doJSON(t, mux, "POST", "/api/projects", input)
			if w.Code != 201 {
				t.Fatalf("create: %d %s", w.Code, w.Body)
			}
			var p models.Project
			decodeJSON(t, w, &p)
			if p.TelegramNotifications != tc.want {
				t.Fatalf("create flag = %v, want %v", p.TelegramNotifications, tc.want)
			}
			stored, err := s.DB.GetProject(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.TelegramNotifications != tc.want {
				t.Fatalf("stored flag = %v, want %v", stored.TelegramNotifications, tc.want)
			}
			for _, path := range []string{"/api/projects", fmt.Sprintf("/api/projects/%d", p.ID)} {
				result := doJSON(t, mux, "GET", path, nil)
				if result.Code != 200 || !strings.Contains(result.Body.String(), fmt.Sprintf(`"telegram_notifications":%v`, tc.want)) {
					t.Errorf("GET %s: %d %s", path, result.Code, result.Body)
				}
				if strings.Contains(result.Body.String(), "private-clone-secret") || strings.Contains(result.Body.String(), "private-webhook-secret") {
					t.Error("response exposed project secrets")
				}
			}
		})
	}
}

func TestTelegramProjectUpdatePreservesOtherSettings(t *testing.T) {
	s, mux := newTestServer(t)
	p := createProject(t, s, models.Project{
		Name: "telegram", RepoURL: "https://github.com/u/app", Branch: "main", ImageName: "telegram",
		DockerfilePath: "Dockerfile", Executor: "unity-windows", ExecutorProfiles: sharedProfiles(),
		CloneToken: "private-clone-secret", WebhookSecret: "private-webhook-secret",
		PollEnabled: true, PollIntervalSecs: 90, TelegramNotifications: true,
	})
	if err := s.DB.UpdatePollState(p.ID, "baseline-sha", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.UpdatePollState(p.ID, "", "probe failure"); err != nil {
		t.Fatal(err)
	}
	polled, err := s.DB.GetProject(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	baseline := polled.LastPolledAt
	path := fmt.Sprintf("/api/projects/%d", p.ID)
	for _, tc := range []struct {
		name   string
		update map[string]interface{}
		want   bool
	}{
		{"omitted-enabled", map[string]interface{}{"no_cache": true}, true},
		{"explicit-false", map[string]interface{}{"telegram_notifications": false}, false},
		{"omitted-muted", map[string]interface{}{"no_cache": false}, false},
		{"explicit-true", map[string]interface{}{"telegram_notifications": true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doJSON(t, mux, "PUT", path, tc.update)
			if w.Code != 200 {
				t.Fatalf("update: %d %s", w.Code, w.Body)
			}
			var response models.Project
			decodeJSON(t, w, &response)
			got, err := s.DB.GetProject(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.TelegramNotifications != tc.want || response.TelegramNotifications != tc.want {
				t.Fatalf("flag = %v (response %v), want %v", got.TelegramNotifications, response.TelegramNotifications, tc.want)
			}
			if !reflect.DeepEqual(got.ExecutorProfiles, p.ExecutorProfiles) || got.Executor != p.Executor ||
				got.CloneToken != p.CloneToken || got.WebhookSecret != p.WebhookSecret ||
				!got.PollEnabled || got.PollIntervalSecs != 90 || got.LastPolledSHA != "baseline-sha" ||
				got.LastPollError != "probe failure" || got.LastPolledAt == nil || !got.LastPolledAt.Equal(*baseline) {
				t.Fatalf("notification update changed other settings: %+v", got)
			}
			if strings.Contains(w.Body.String(), "private-clone-secret") || strings.Contains(w.Body.String(), "private-webhook-secret") {
				t.Error("update response exposed secrets")
			}
		})
	}
}

func TestTelegramProjectSettingsRequireOperator(t *testing.T) {
	s, mux := newTestServer(t)
	p := createProject(t, s, models.Project{Name: "telegram", RepoURL: "https://github.com/u/app", ImageName: "telegram", TelegramNotifications: true})
	a, err := auth.New(s.DB, "operator-password", "", "")
	if err != nil {
		t.Fatal(err)
	}
	a.SetAgentToken("agent-token")
	wrapped := a.Middleware(mux)
	for _, tc := range []struct{ method, path, body string }{
		{"PUT", fmt.Sprintf("/api/projects/%d", p.ID), `{"telegram_notifications":false}`},
		{"POST", "/api/projects", `{"name":"agent-project","repo_url":"https://github.com/u/app","image_name":"agent-project","telegram_notifications":false}`},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer agent-token")
		req.Header.Set("X-Builds-Csrf", "1")
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("agent %s %s: %d %s", tc.method, tc.path, w.Code, w.Body)
		}
	}
	stored, _ := s.DB.GetProject(p.ID)
	if !stored.TelegramNotifications {
		t.Fatal("agent muted project")
	}
	req := httptest.NewRequest("PUT", fmt.Sprintf("/api/projects/%d", p.ID), strings.NewReader(`{"telegram_notifications":false}`))
	req.Header.Set("Authorization", "Bearer operator-password")
	req.Header.Set("X-Builds-Csrf", "1")
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("operator update: %d %s", w.Code, w.Body)
	}
	stored, _ = s.DB.GetProject(p.ID)
	if stored.TelegramNotifications {
		t.Fatal("operator could not mute project")
	}
}

func TestTelegramStatusIsOperatorOnlyAndContainsOnlyReadiness(t *testing.T) {
	s, mux := newTestServer(t)
	a, err := auth.New(s.DB, "operator-password", "", "")
	if err != nil {
		t.Fatal(err)
	}
	a.SetAgentToken("agent-token")
	wrapped := a.Middleware(mux)
	called := 0
	s.TelegramConfigured = func() bool { called++; return true }
	for _, token := range []string{"", "agent-token"} {
		req := httptest.NewRequest("GET", "/api/notifications/telegram", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, req)
		want := 401
		if token != "" {
			want = 403
		}
		if w.Code != want {
			t.Fatalf("token %q: %d %s", token, w.Code, w.Body)
		}
	}
	if called != 0 {
		t.Fatal("unauthorized request ran status callback")
	}
	for _, tc := range []struct {
		name     string
		callback func() bool
		want     string
	}{
		{"connected", func() bool { return true }, `{"configured":true}`},
		{"disconnected", func() bool { return false }, `{"configured":false}`},
		{"no-callback", nil, `{"configured":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.TelegramConfigured = tc.callback
			req := httptest.NewRequest("GET", "/api/notifications/telegram", nil)
			req.Header.Set("Authorization", "Bearer operator-password")
			w := httptest.NewRecorder()
			wrapped.ServeHTTP(w, req)
			if w.Code != 200 || strings.TrimSpace(w.Body.String()) != tc.want {
				t.Fatalf("status: %d %s", w.Code, w.Body)
			}
		})
	}
}
