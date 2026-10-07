package runner

import (
	"context"
	"strings"
	"testing"
)

func TestGitSSHCommandPinsHostAndDedicatedKey(t *testing.T) {
	for _, part := range []string{
		"-i /run/builds-ssh/townhall_key",
		"-o IdentitiesOnly=yes",
		"-o BatchMode=yes",
		"-o StrictHostKeyChecking=yes",
		"-o UserKnownHostsFile=/run/builds-ssh/known_hosts",
		"-o GlobalKnownHostsFile=/dev/null",
	} {
		if !strings.Contains(GitSSHCommand, part) {
			t.Errorf("missing SSH restriction %s", part)
		}
	}
	if strings.Contains(GitSSHCommand, "StrictHostKeyChecking=no") || strings.Contains(GitSSHCommand, "accept-new") {
		t.Fatal("SSH command must reject unknown or changed host keys")
	}
	t.Setenv("GIT_SSH_COMMAND", "ssh -o StrictHostKeyChecking=no")
	cmd := newCmd(context.Background(), nil, "git", "clone", "--depth", "1", "git@github.com:FanDoster/townhall.git", "/tmp/checkout")
	var got string
	for _, field := range cmd.Environ() {
		if strings.HasPrefix(field, "GIT_SSH_COMMAND=") {
			got = strings.TrimPrefix(field, "GIT_SSH_COMMAND=")
		}
	}
	if got != GitSSHCommand {
		t.Fatalf("runner SSH command = %q, want %q", got, GitSSHCommand)
	}
}
