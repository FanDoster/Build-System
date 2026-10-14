package web

import (
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"testing"

	"github.com/FanDoster/Build-System/internal/models"
)

func TestSettingsQueueDropdownAndSharedProfiles(t *testing.T) {
	database, mux := setup(t)
	profiles := map[string]models.ExecutorProfile{
		"mac":   {BuildCommand: "Build.Windows", BuildPath: "/Users/build/Windows", UploadScript: "Steam/UPLOAD_MAC.sh"},
		"linux": {BuildCommand: "Build.Windows", BuildPath: "/home/build/Windows", UploadScript: ""},
	}
	projects := []*models.Project{
		{Name: "shared", Executor: "unity-windows", ExecutorProfiles: profiles},
		{Name: "legacy", Executor: "mac"},
		{Name: "web", Executor: "local"},
	}
	for _, p := range projects {
		p.RepoURL = "https://github.com/u/" + p.Name
		p.ImageName = p.Name
		if err := database.CreateProject(p); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range projects {
		w := get(t, mux, fmt.Sprintf("/projects/%d/settings", p.ID))
		if w.Code != 200 {
			t.Fatalf("settings %s: %d %s", p.Name, w.Code, w.Body)
		}
		body := w.Body.String()
		for _, want := range []string{
			`<select id="f-executor" name="executor"`, `<option value="local"`, `<option value="mac"`,
			`<option value="unity-windows"`, fmt.Sprintf(`<option value="%s" selected>`, p.Executor),
			`<option value=":new:">New queue…</option>`, `id="new-queue-field" hidden`,
			`name="new_queue"`, `name="executor_profiles"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("settings %s missing %q", p.Name, want)
			}
		}
		if strings.Contains(body, `<input id="f-executor"`) {
			t.Error("executor is still a free-text field")
		}
	}
	shared := projects[0]
	body := get(t, mux, fmt.Sprintf("/projects/%d/settings", shared.ID)).Body.String()
	start := strings.Index(body, `<textarea id="f-executor-profiles"`)
	content := body[start:]
	content = content[strings.Index(content, ">")+1 : strings.Index(content, "</textarea>")]
	var parsed map[string]models.ExecutorProfile
	if err := json.Unmarshal([]byte(html.UnescapeString(content)), &parsed); err != nil {
		t.Fatalf("invalid rendered profile JSON: %v", err)
	}
	if parsed["linux"].BuildPath != "/home/build/Windows" || parsed["linux"].UploadScript != "" {
		t.Fatalf("Linux recipe not shown accurately: %+v", parsed)
	}
	dashboard := get(t, mux, "/").Body.String()
	if !strings.Contains(dashboard, "Queue: unity-windows · shared") {
		t.Errorf("shared queue missing on dashboard: %s", dashboard)
	}
	page := get(t, mux, fmt.Sprintf("/projects/%d", shared.ID)).Body.String()
	if !strings.Contains(page, "unity-windows") || !strings.Contains(page, "shared by linux, mac") {
		t.Error("shared queue eligibility not shown on project page")
	}
}

func TestProfileTextareaCannotInjectHTML(t *testing.T) {
	database, mux := setup(t)
	p := &models.Project{Name: "escape", RepoURL: "https://github.com/u/game", ImageName: "escape", Executor: "unity-windows",
		ExecutorProfiles: map[string]models.ExecutorProfile{"linux": {BuildCommand: "Build.Windows", BuildPath: `</textarea><script>alert(1)</script>`}},
	}
	if err := database.CreateProject(p); err != nil {
		t.Fatal(err)
	}
	body := get(t, mux, fmt.Sprintf("/projects/%d/settings", p.ID)).Body.String()
	if strings.Contains(body, `</textarea><script>alert(1)</script>`) {
		t.Fatal("profile escaped textarea and injected HTML")
	}
	if strings.Count(body, "</textarea>") != 1 {
		t.Fatal("profile corrupted textarea markup")
	}
}

func TestSharedQueueSettingsScriptValidatesBeforeSaving(t *testing.T) {
	_, mux := setup(t)
	body := get(t, mux, "/static/js/app.js").Body.String()
	for _, want := range []string{
		"JSON.parse(f.executor_profiles.value)", "Array.isArray(payload.executor_profiles)",
		"Profiles require a remote build queue.", "Unknown field in profile ", "f.executor_profiles.focus()",
		"executor: f.executor.value === ':new:' ? f.new_queue.value.trim() : f.executor.value",
		"newQueue.disabled = !creating", "newQueue.required = creating",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings script missing %q", want)
		}
	}
}
