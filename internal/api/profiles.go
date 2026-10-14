package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/FanDoster/Build-System/internal/models"
)

const (
	maxExecutorProfiles = 16
	maxProfileCommand   = 1024
	maxProfilePath      = 4096
)

// A raw field distinguishes an omitted update from an explicit empty object.
// Reject null and misspelled profile fields rather than silently losing routing
// or upload settings. Validation applies to the resulting whole project.
func parseExecutorProfiles(raw json.RawMessage) (map[string]models.ExecutorProfile, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, fmt.Errorf("executor_profiles must be an object; use {} to clear it")
	}
	var profiles map[string]models.ExecutorProfile
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&profiles); err != nil {
		return nil, fmt.Errorf("invalid executor_profiles: %w", err)
	}
	return profiles, nil
}

func validExecutorProfiles(p *models.Project) error {
	if len(p.ExecutorProfiles) == 0 {
		return nil
	}
	if !models.Remote(p.Executor) {
		return fmt.Errorf("executor_profiles require a remote executor, not local")
	}
	if len(p.ExecutorProfiles) > maxExecutorProfiles {
		return fmt.Errorf("at most %d executor_profiles", maxExecutorProfiles)
	}
	for name, profile := range p.ExecutorProfiles {
		if !models.Remote(name) {
			return fmt.Errorf("executor profile name %q is reserved for the local runner", name)
		}
		if err := validExecutor(name); err != nil {
			return fmt.Errorf("executor profile %q: %w", name, err)
		}
		if strings.TrimSpace(profile.BuildCommand) == "" || strings.TrimSpace(profile.BuildPath) == "" {
			return fmt.Errorf("executor profile %q requires build_command and build_path", name)
		}
		if len(profile.BuildCommand) > maxProfileCommand {
			return fmt.Errorf("executor profile %q: build_command must be %d bytes or fewer", name, maxProfileCommand)
		}
		if len(profile.BuildPath) > maxProfilePath || len(profile.UploadScript) > maxProfilePath {
			return fmt.Errorf("executor profile %q: build_path and upload_script must be %d bytes or fewer", name, maxProfilePath)
		}
	}
	return nil
}

// Old agents understand only the base fields. Resolve the entire profile, not
// individual nonempty fields: an empty upload_script means build-only, never
// "inherit the other machine's upload script". Keep other profiles off the wire.
func prepareClaimProject(p *models.Project, executors []string) bool {
	if !models.Remote(p.Executor) {
		return false
	}
	profile, ok := p.ProfileFor(executors)
	if !ok {
		return false
	}
	p.BuildCommand = profile.BuildCommand
	p.BuildPath = profile.BuildPath
	p.UploadScript = profile.UploadScript
	p.ExecutorProfiles = nil
	return true
}
