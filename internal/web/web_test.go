package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FanDoster/Build-System/internal/agents"
	"github.com/FanDoster/Build-System/internal/auth"
	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/models"
)

func setup(t *testing.T) (*db.DB, *http.ServeMux) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	mux := http.NewServeMux()
	New(database, "").RegisterRoutes(mux)
	return database, mux
}

func get(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

// Renders every page with a finished build, which used to break the project
// page ({{.FinishedAt.Sub .StartedAt}} on pointer receivers + %.0f on a
// Duration produced a template error mid-render).
func TestPagesRenderWithFinishedBuild(t *testing.T) {
	database, mux := setup(t)

	p := &models.Project{
		Name: "app", RepoURL: "https://github.com/u/app", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
	}
	if err := database.CreateProject(p); err != nil {
		t.Fatal(err)
	}
	b := &models.Build{ProjectID: p.ID, Status: models.StatusPending, CommitSHA: "abc123", CommitMessage: "msg"}
	if err := database.CreateBuild(b); err != nil {
		t.Fatal(err)
	}
	database.UpdateBuildStatus(b.ID, models.StatusRunning, "started\n")
	database.UpdateBuildStatus(b.ID, models.StatusSuccess, "started\ndone\n")

	for _, path := range []string{"/", fmt.Sprintf("/projects/%d", p.ID), fmt.Sprintf("/builds/%d", b.ID)} {
		w := get(t, mux, path)
		if w.Code != 200 {
			t.Errorf("GET %s: got %d", path, w.Code)
		}
		if strings.Contains(w.Body.String(), "%!") {
			t.Errorf("GET %s: template formatting error in output:\n%s", path, w.Body)
		}
	}

	// Project page must show a real duration, not an empty/broken cell.
	w := get(t, mux, fmt.Sprintf("/projects/%d", p.ID))
	if !strings.Contains(w.Body.String(), "s</td>") {
		t.Errorf("project page missing build duration:\n%s", w.Body)
	}
}

func TestNotFoundAndBadIDs(t *testing.T) {
	_, mux := setup(t)
	if w := get(t, mux, "/projects/999"); w.Code != 404 {
		t.Errorf("missing project: got %d, want 404", w.Code)
	}
	if w := get(t, mux, "/builds/abc"); w.Code != 400 {
		t.Errorf("bad build id: got %d, want 400", w.Code)
	}
}

// The settings page renders every configurable field but never a secret value.
func TestSettingsPageRendersWithoutSecretValues(t *testing.T) {
	database, mux := setup(t)
	p := &models.Project{
		Name: "app", RepoURL: "https://github.com/u/app", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app", NoCache: true,
		WebhookSecret: "super-webhook-secret", CloneToken: "super-clone-token",
	}
	if err := database.CreateProject(p); err != nil {
		t.Fatal(err)
	}

	w := get(t, mux, fmt.Sprintf("/projects/%d/settings", p.ID))
	if w.Code != 200 {
		t.Fatalf("settings: got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`name="name"`, `name="repo_url"`, `name="branch"`, `name="dockerfile_path"`,
		`name="image_name"`, `name="no_cache"`, `name="deploy_compose_path"`,
		`name="deploy_service_name"`, `name="webhook_secret"`, `name="clone_token"`,
		"Danger zone", "clear-webhook", "clear-token",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
	// no_cache checkbox reflects the stored value.
	if !strings.Contains(body, `name="no_cache" checked`) {
		t.Error("no_cache checkbox not checked for enabled project")
	}
	// Secret VALUES must never appear in the HTML.
	if strings.Contains(body, "super-webhook-secret") || strings.Contains(body, "super-clone-token") {
		t.Error("settings page leaked a secret value")
	}
	if w := get(t, mux, "/projects/999/settings"); w.Code != 404 {
		t.Errorf("missing project settings: got %d, want 404", w.Code)
	}
}

// The agents page renders the same fleet the JSON endpoint gates behind
// requireOperator: every machine, every queue, and the operator's pause notes.
// Gating one and not the other means a build machine's own token reads the lot
// by asking for the page instead of the API.
func TestAgentsPageRefusesAnAgentToken(t *testing.T) {
	database, mux := setup(t)
	h := New(database, "")
	h.Agents = agents.NewRegistry()
	mux = http.NewServeMux()
	h.RegisterRoutes(mux)

	a, err := auth.New(database, "operator-pw", "", "")
	if err != nil {
		t.Fatal(err)
	}
	a.SetAgentToken("agent-tok")
	wrapped := a.Middleware(mux)

	call := func(bearer string) int {
		req := httptest.NewRequest("GET", "/agents", nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, req)
		return w.Code
	}

	if code := call("agent-tok"); code != 403 {
		t.Errorf("agent token: %d, want 403 — the page discloses the whole fleet", code)
	}
	if code := call("operator-pw"); code != 200 {
		t.Errorf("operator password: %d, want 200", code)
	}
}
