package api

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/FanDoster/Build-System/internal/models"
)

func sharedProfiles() map[string]models.ExecutorProfile {
	return map[string]models.ExecutorProfile{
		"mac":   {BuildCommand: "Build.Windows", BuildPath: "/Users/build/Builds/Windows", UploadScript: "Steam/UPLOAD_MAC.sh"},
		"linux": {BuildCommand: "Build.Windows", BuildPath: "/home/build/Builds/Windows", UploadScript: ""},
	}
}

func sharedProject(t *testing.T, s *Server) *models.Project {
	t.Helper()
	return createProject(t, s, models.Project{
		Name: "game-shared", RepoURL: "https://github.com/u/game", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "game-shared", Executor: "unity-windows",
		BuildCommand: "Unsafe.Base", BuildPath: "/wrong-machine", UploadScript: "unsafe-upload.sh",
		ExecutorProfiles: sharedProfiles(), CloneToken: "clone-secret",
	})
}

// These requests and response structs are deliberately the old agent protocol:
// no profiles, platform metadata or logical queue configuration on the agent.
func TestSharedQueueOldAgentsReceiveOnlyTheirCompleteRecipe(t *testing.T) {
	s, mux := newAgentServer(t)
	p := sharedProject(t, s)
	for _, executor := range []string{"mac", "linux"} {
		b := pending(t, s, p.ID)
		w := doJSON(t, mux, "POST", "/api/agents/claim", map[string]interface{}{
			"agent": executor + "-old", "executors": []string{executor},
		})
		if w.Code != 200 {
			t.Fatalf("%s claim: %d %s", executor, w.Code, w.Body)
		}
		var response struct {
			Build   models.Build `json:"build"`
			Project struct {
				Executor     string `json:"executor"`
				BuildCommand string `json:"build_command"`
				BuildPath    string `json:"build_path"`
				UploadScript string `json:"upload_script"`
				CloneToken   string `json:"clone_token"`
			} `json:"project"`
		}
		decodeJSON(t, w, &response)
		expected := sharedProfiles()[executor]
		if response.Build.ID != b.ID || response.Build.Agent != executor+"-old" ||
			response.Project.Executor != "unity-windows" || response.Project.BuildCommand != expected.BuildCommand ||
			response.Project.BuildPath != expected.BuildPath || response.Project.UploadScript != expected.UploadScript ||
			response.Project.CloneToken != "clone-secret" {
			t.Fatalf("%s response: %+v", executor, response)
		}
		if strings.Contains(w.Body.String(), "executor_profiles") || strings.Contains(w.Body.String(), "Unsafe.Base") ||
			strings.Contains(w.Body.String(), "/wrong-machine") || strings.Contains(w.Body.String(), "unsafe-upload.sh") {
			t.Fatalf("%s response leaks other recipes: %s", executor, w.Body)
		}
		otherPath := sharedProfiles()["mac"].BuildPath
		if executor == "mac" {
			otherPath = sharedProfiles()["linux"].BuildPath
		}
		if strings.Contains(w.Body.String(), otherPath) {
			t.Fatalf("leaked other path: %s", w.Body)
		}
		// Finish to free the existing one-running-build-per-project gate.
		finish := doJSON(t, mux, "POST", buildPath(b.ID, "finish"), map[string]string{
			"agent": executor + "-old", "status": "success",
		})
		if finish.Code != 200 {
			t.Fatalf("finish: %d %s", finish.Code, finish.Body)
		}
	}
	stored, err := s.DB.GetProject(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored.ExecutorProfiles, sharedProfiles()) || stored.BuildPath != "/wrong-machine" {
		t.Fatalf("claim mutated stored recipe: %+v", stored)
	}
}

func TestSharedQueueDoesNotFallbackToPrimaryOrUnknownExecutor(t *testing.T) {
	s, mux := newAgentServer(t)
	p := sharedProject(t, s)
	b := pending(t, s, p.ID)
	for _, executor := range []string{"unity-windows", "windows", "other"} {
		w := doJSON(t, mux, "POST", "/api/agents/claim", map[string]interface{}{
			"agent": executor + "-agent", "executors": []string{executor},
		})
		if w.Code != 204 {
			t.Fatalf("%s claimed incompatible profile: %d %s", executor, w.Code, w.Body)
		}
	}
	stored, _ := s.DB.GetBuild(b.ID)
	if stored.Status != models.StatusPending {
		t.Fatalf("ineligible claim changed build: %+v", stored)
	}
}

func TestPrepareClaimProjectFailsClosedAfterEligibilityEdit(t *testing.T) {
	for _, p := range []models.Project{
		{Executor: "local", ExecutorProfiles: sharedProfiles()},
		{Executor: "", ExecutorProfiles: sharedProfiles()},
		{Executor: "unity-windows", BuildCommand: "Unsafe.Base", ExecutorProfiles: map[string]models.ExecutorProfile{
			"mac": {BuildCommand: "Build.Windows", BuildPath: "/Users/build"},
		}},
	} {
		before := p
		if prepareClaimProject(&p, []string{"linux"}) {
			t.Fatalf("unsafe claim accepted: %+v", p)
		}
		if !reflect.DeepEqual(before, p) {
			t.Fatal("rejected resolution modified recipe")
		}
	}
}

func TestExecutorProfilesCreateUpdatePreserveAndClear(t *testing.T) {
	s, mux := newAgentServer(t)
	w := doJSON(t, mux, "POST", "/api/projects", map[string]interface{}{
		"name": "shared", "repo_url": "https://github.com/u/game", "image_name": "shared",
		"executor": "unity-windows", "executor_profiles": sharedProfiles(),
	})
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var p models.Project
	decodeJSON(t, w, &p)
	if !reflect.DeepEqual(p.ExecutorProfiles, sharedProfiles()) {
		t.Fatalf("create lost profiles: %+v", p)
	}
	path := fmt.Sprintf("/api/projects/%d", p.ID)
	for _, update := range []map[string]interface{}{
		{"build_command": "Base.New", "build_path": "/base/new", "upload_script": "base-upload.sh"},
		{"name": "renamed", "executor": "another-logical-queue"},
	} {
		w = doJSON(t, mux, "PUT", path, update)
		if w.Code != 200 {
			t.Fatalf("partial update: %d %s", w.Code, w.Body)
		}
		stored, _ := s.DB.GetProject(p.ID)
		if !reflect.DeepEqual(stored.ExecutorProfiles, sharedProfiles()) {
			t.Fatalf("omitted profiles lost: %+v", stored)
		}
	}
	// A local switch must not retain hidden remote recipes or enqueue anything.
	b := pending(t, s, p.ID)
	w = doJSON(t, mux, "PUT", path, map[string]interface{}{"executor": "local"})
	if w.Code != 400 || len(s.BuildCh) != 0 {
		t.Fatalf("unsafe local transition: %d, queued=%d", w.Code, len(s.BuildCh))
	}
	stored, _ := s.DB.GetProject(p.ID)
	if stored.Executor != "another-logical-queue" {
		t.Fatal("rejected update wrote executor")
	}
	// Explicit {} clears profiles; with a local switch it adopts pending work.
	w = doJSON(t, mux, "PUT", path, map[string]interface{}{"executor": "local", "executor_profiles": map[string]interface{}{}})
	if w.Code != 200 {
		t.Fatalf("clear: %d %s", w.Code, w.Body)
	}
	stored, _ = s.DB.GetProject(p.ID)
	if len(stored.ExecutorProfiles) != 0 {
		t.Fatalf("{} did not clear: %+v", stored)
	}
	if len(s.BuildCh) != 1 || (<-s.BuildCh).ID != b.ID {
		t.Fatal("clearing profiles/local switch failed to adopt build")
	}
}

func TestExecutorProfileValidationCreateAndUpdate(t *testing.T) {
	valid := models.ExecutorProfile{BuildCommand: "Build.Windows", BuildPath: "/output"}
	many := map[string]models.ExecutorProfile{}
	for i := 0; i < 17; i++ {
		many[fmt.Sprintf("agent-%d", i)] = valid
	}
	cases := []struct {
		name     string
		value    interface{}
		executor string
	}{
		{"local-project", sharedProfiles(), "local"},
		{"empty-project", sharedProfiles(), ""},
		{"null", nil, "unity-windows"},
		{"array", []interface{}{}, "unity-windows"},
		{"scalar", "mac", "unity-windows"},
		{"reserved-name", map[string]models.ExecutorProfile{"local": valid}, "unity-windows"},
		{"empty-name", map[string]models.ExecutorProfile{"": valid}, "unity-windows"},
		{"invalid-name", map[string]models.ExecutorProfile{"Linux": valid}, "unity-windows"},
		{"long-name", map[string]models.ExecutorProfile{strings.Repeat("m", 33): valid}, "unity-windows"},
		{"too-many", many, "unity-windows"},
		{"missing-command", map[string]models.ExecutorProfile{"linux": {BuildPath: "/output"}}, "unity-windows"},
		{"missing-path", map[string]models.ExecutorProfile{"linux": {BuildCommand: "Build.Windows"}}, "unity-windows"},
		{"blank-command", map[string]models.ExecutorProfile{"linux": {BuildCommand: "   ", BuildPath: "/output"}}, "unity-windows"},
		{"long-command", map[string]models.ExecutorProfile{"linux": {BuildCommand: strings.Repeat("a", maxProfileCommand+1), BuildPath: "/output"}}, "unity-windows"},
		{"long-path", map[string]models.ExecutorProfile{"linux": {BuildCommand: "Build.Windows", BuildPath: strings.Repeat("a", maxProfilePath+1)}}, "unity-windows"},
		{"long-upload", map[string]models.ExecutorProfile{"linux": {BuildCommand: "Build.Windows", BuildPath: "/output", UploadScript: strings.Repeat("a", maxProfilePath+1)}}, "unity-windows"},
		{"unknown-field", map[string]interface{}{"linux": map[string]string{"build_command": "Build.Windows", "build_path": "/output", "upload_scrit": "unsafe.sh"}}, "unity-windows"},
		{"wrong-type", map[string]interface{}{"linux": map[string]interface{}{"build_command": 42, "build_path": "/output"}}, "unity-windows"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, mux := newAgentServer(t)
			p := sharedProject(t, s)
			w := doJSON(t, mux, "POST", "/api/projects", map[string]interface{}{
				"name": "invalid", "repo_url": "https://github.com/u/game", "image_name": "invalid",
				"executor": tc.executor, "executor_profiles": tc.value,
			})
			if w.Code != 400 {
				t.Fatalf("create accepted invalid profiles: %d %s", w.Code, w.Body)
			}
			w = doJSON(t, mux, "PUT", fmt.Sprintf("/api/projects/%d", p.ID), map[string]interface{}{
				"executor": tc.executor, "executor_profiles": tc.value,
			})
			if w.Code != 400 {
				t.Fatalf("update accepted invalid profiles: %d %s", w.Code, w.Body)
			}
			stored, _ := s.DB.GetProject(p.ID)
			if stored.Executor != p.Executor || !reflect.DeepEqual(stored.ExecutorProfiles, p.ExecutorProfiles) {
				t.Fatal("invalid update mutated project")
			}
		})
	}
}

func TestProfileLimitsAndEmptyUploadAccepted(t *testing.T) {
	s, mux := newAgentServer(t)
	profiles := map[string]models.ExecutorProfile{}
	for i := 0; i < 16; i++ {
		profiles[fmt.Sprintf("agent-%d", i)] = models.ExecutorProfile{
			BuildCommand: strings.Repeat("a", maxProfileCommand), BuildPath: strings.Repeat("p", maxProfilePath), UploadScript: "",
		}
	}
	w := doJSON(t, mux, "POST", "/api/projects", map[string]interface{}{
		"name": "boundary", "repo_url": "https://github.com/u/game", "image_name": "boundary",
		"executor": "unity-windows", "executor_profiles": profiles,
	})
	if w.Code != 201 {
		t.Fatalf("valid boundary profiles rejected: %d %s", w.Code, w.Body)
	}
	var p models.Project
	decodeJSON(t, w, &p)
	stored, _ := s.DB.GetProject(p.ID)
	if !reflect.DeepEqual(stored.ExecutorProfiles, profiles) {
		t.Fatal("valid profiles not persisted")
	}
}
