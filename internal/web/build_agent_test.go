package web

import (
	"fmt"
	"html"
	"strings"
	"testing"

	"github.com/FanDoster/Build-System/internal/models"
)

func TestBuildAgentVisibleOnDashboardProjectAndBuild(t *testing.T) {
	for _, tc := range []struct {
		name, executor, agent, want string
		status                      models.BuildStatus
	}{
		{"running remote", "unity-windows", "mac-m4max-dan", "mac-m4max-dan", models.StatusRunning},
		{"finished remote", "unity-windows", "linux-fandoster", "linux-fandoster", models.StatusSuccess},
		{"queued remote", "unity-windows", "", "Not assigned", models.StatusPending},
		{"historical unrecorded", "unity-windows", "", "Not recorded", models.StatusFailed},
		{"local runner", "local", "", "Local runner", models.StatusRunning},
		{"escaped owner", "unity-windows", `<img src=x onerror="alert(1)">`, `<img src=x onerror="alert(1)">`, models.StatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, mux := setup(t)
			p := &models.Project{Name: "app", RepoURL: "https://example.invalid/app", ImageName: "app", Executor: tc.executor}
			if err := database.CreateProject(p); err != nil {
				t.Fatal(err)
			}
			b := &models.Build{ProjectID: p.ID, Status: models.StatusPending, CommitSHA: "abc"}
			if err := database.CreateBuild(b); err != nil {
				t.Fatal(err)
			}
			if tc.agent != "" {
				if claimed, err := database.ClaimBuildForAgent(tc.agent, []string{tc.executor}); err != nil || claimed == nil {
					t.Fatalf("claim = %+v, %v", claimed, err)
				}
			}
			if tc.status != models.StatusPending {
				if err := database.UpdateBuildStatus(b.ID, tc.status, ""); err != nil {
					t.Fatal(err)
				}
			}
			for _, route := range []string{"/", fmt.Sprintf("/projects/%d", p.ID), fmt.Sprintf("/builds/%d", b.ID)} {
				w := get(t, mux, route)
				if w.Code != 200 {
					t.Fatalf("GET %s: %d", route, w.Code)
				}
				body := w.Body.String()
				if !strings.Contains(body, `class="build-agent-name">`+html.EscapeString(tc.want)+`</span>`) &&
					!strings.Contains(body, `id="meta-agent">`+html.EscapeString(tc.want)+`</div>`) {
					t.Errorf("GET %s does not display actual agent %q", route, tc.want)
				}
				if strings.Contains(body, `<img src=x onerror=`) {
					t.Fatalf("GET %s rendered unescaped agent HTML", route)
				}
				if strings.Contains(route, "/builds/") {
					if !strings.Contains(body, `data-agent="`+html.EscapeString(tc.agent)+`"`) ||
						!strings.Contains(body, `data-executor="`+tc.executor+`"`) {
						t.Errorf("GET %s missing initial live owner data", route)
					}
				}
			}
		})
	}
}
