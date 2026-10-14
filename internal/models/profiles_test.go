package models

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestProjectProfileFor(t *testing.T) {
	base := ExecutorProfile{BuildCommand: "base command", BuildPath: "base path", UploadScript: "base upload"}
	linux := ExecutorProfile{BuildCommand: "linux command", BuildPath: "linux path"}
	mac := ExecutorProfile{BuildCommand: "mac command", BuildPath: "mac path", UploadScript: "mac upload"}
	p := Project{Executor: "unity-windows", BuildCommand: base.BuildCommand, BuildPath: base.BuildPath, UploadScript: base.UploadScript,
		ExecutorProfiles: map[string]ExecutorProfile{"linux": linux, "mac": mac}}
	for _, tc := range []struct {
		name      string
		executors []string
		want      ExecutorProfile
		ok        bool
	}{
		{"linux build-only", []string{"linux"}, linux, true},
		{"mac recipe", []string{"mac"}, mac, true},
		{"request order", []string{"windows", "mac", "linux"}, mac, true},
		{"reverse request order", []string{"linux", "mac"}, linux, true},
		{"logical queue cannot fall back", []string{"unity-windows"}, ExecutorProfile{}, false},
		{"unmatched", []string{"windows"}, ExecutorProfile{}, false},
		{"no executors", nil, ExecutorProfile{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := p.ProfileFor(tc.executors)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("ProfileFor(%v) = %+v, %v; want %+v, %v", tc.executors, got, ok, tc.want, tc.ok)
			}
		})
	}
	for _, profiles := range []map[string]ExecutorProfile{nil, {}} {
		p.ExecutorProfiles = profiles
		got, ok := p.ProfileFor([]string{"linux", "unity-windows"})
		if !ok || got != base {
			t.Errorf("legacy profile = %+v, %v; want %+v, true", got, ok, base)
		}
		if _, ok := p.ProfileFor([]string{"linux", "mac"}); ok {
			t.Error("legacy project matched a different executor")
		}
	}
}

func TestProjectEligibleExecutors(t *testing.T) {
	for _, tc := range []struct {
		project Project
		want    []string
	}{
		{Project{Executor: "mac"}, []string{"mac"}},
		{Project{Executor: "mac", ExecutorProfiles: map[string]ExecutorProfile{}}, []string{"mac"}},
		{Project{Executor: "unity-windows", ExecutorProfiles: map[string]ExecutorProfile{"mac": {}, "linux": {}}}, []string{"linux", "mac"}},
	} {
		got := tc.project.EligibleExecutors()
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("EligibleExecutors = %v, want %v", got, tc.want)
		}
	}
}

func TestExecutorProfileJSONIncludesEmptyFields(t *testing.T) {
	data, err := json.Marshal(ExecutorProfile{})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"build_command":"","build_path":"","upload_script":""}` {
		t.Errorf("complete recipe JSON = %s", data)
	}
}
