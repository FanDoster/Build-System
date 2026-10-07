package poller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FanDoster/Build-System/internal/runner"
)

// A fake git checks the exact subprocess environment without network or a real key.
func TestGitLsRemoteUsesPinnedSSHCommand(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "ssh-command")
	script := "#!/bin/sh\nprintf '%s\\n' \"$GIT_SSH_COMMAND\" > \"$SSH_CAPTURE_FILE\"\nprintf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\\trefs/heads/main\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SSH_CAPTURE_FILE", capture)
	t.Setenv("GIT_SSH_COMMAND", "ssh -o StrictHostKeyChecking=no")
	sha, err := gitLsRemote(context.Background(), "git@github.com:FanDoster/townhall.git", "main")
	if err != nil || sha != strings.Repeat("a", 40) {
		t.Fatalf("ls-remote = %q, %v", sha, err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != runner.GitSSHCommand {
		t.Fatalf("poller SSH command = %q", data)
	}
}
