package runner

import (
	"net/url"
	"strings"
	"testing"
)

func TestInjectToken(t *testing.T) {
	cases := []struct {
		url, token, want string
	}{
		// token:token, not token@ — git >= 2.45 rejects username-only Basic
		// auth and prompts for a password, which GIT_TERMINAL_PROMPT=0 turns
		// into "could not read Password".
		{"https://github.com/u/repo.git", "abc123", "https://abc123:abc123@github.com/u/repo.git"},
		// Special characters must be percent-encoded so the URL stays valid.
		{"https://github.com/u/repo.git", "a:b@c/d", "https://a%3Ab%40c%2Fd:a%3Ab%40c%2Fd@github.com/u/repo.git"},
		// Non-HTTP(S) URLs are left untouched.
		{"git@github.com:u/repo.git", "abc123", "git@github.com:u/repo.git"},
	}
	for _, c := range cases {
		if got := injectToken(c.url, c.token); got != c.want {
			t.Errorf("injectToken(%q, %q) = %q, want %q", c.url, c.token, got, c.want)
		}
	}
}

// A token now appears twice in a clone URL; scrubbing has to catch both
// halves, in raw and percent-encoded form, or a failed clone leaks it into a
// stored log.
func TestScrubSecretMasksInjectedURL(t *testing.T) {
	for _, token := range []string{"ghp_abc123", "a:b@c/d"} {
		injected := injectToken("https://github.com/u/repo.git", token)
		out := scrubSecret("fatal: unable to access '"+injected+"': 403", token)
		if strings.Contains(out, token) {
			t.Errorf("raw token leaked for %q: %s", token, out)
		}
		if enc := url.User(token).String(); enc != token && strings.Contains(out, enc) {
			t.Errorf("encoded token leaked for %q: %s", token, out)
		}
	}
}

func TestScrubSecret(t *testing.T) {
	out := "fatal: unable to access 'https://tok:en@github.com/u/repo.git/': 403"
	got := scrubSecret(out, "tok:en")
	if strings.Contains(got, "tok:en") {
		t.Errorf("raw secret leaked: %q", got)
	}

	// The percent-encoded form (as it appears in an injected URL) must also be masked.
	encoded := "fatal: unable to access 'https://tok%3Aen@github.com/u/repo.git/'"
	got = scrubSecret(encoded, "tok:en")
	if strings.Contains(got, "tok%3Aen") {
		t.Errorf("encoded secret leaked: %q", got)
	}

	// Empty secret is a no-op.
	if got := scrubSecret("unchanged", ""); got != "unchanged" {
		t.Errorf("empty secret altered output: %q", got)
	}
}

func TestResolveDockerfile(t *testing.T) {
	workDir := t.TempDir()

	ok := []string{"Dockerfile", "docker/Dockerfile", "./sub/../Dockerfile"}
	for _, p := range ok {
		if _, err := resolveDockerfile(workDir, p); err != nil {
			t.Errorf("resolveDockerfile(%q) unexpectedly rejected: %v", p, err)
		}
	}

	bad := []string{"../outside/Dockerfile", "../../etc/passwd", "sub/../../escape"}
	for _, p := range bad {
		if _, err := resolveDockerfile(workDir, p); err == nil {
			t.Errorf("resolveDockerfile(%q) should have been rejected", p)
		}
	}
}

func TestResolveComposePath(t *testing.T) {
	workDir := t.TempDir()

	// Absolute paths are server-managed and pass through verbatim.
	for _, p := range []string{"/opt/docker/app/docker-compose.yml", "/etc/compose.yml"} {
		got, err := resolveComposePath(workDir, p)
		if err != nil {
			t.Errorf("resolveComposePath(%q) unexpectedly rejected: %v", p, err)
		}
		if got != p {
			t.Errorf("resolveComposePath(%q) = %q, want it unchanged", p, got)
		}
	}

	// Relative paths resolve inside the repo checkout.
	for _, p := range []string{"docker-compose.yml", "deploy/compose.yml", "./sub/../compose.yml"} {
		got, err := resolveComposePath(workDir, p)
		if err != nil {
			t.Errorf("resolveComposePath(%q) unexpectedly rejected: %v", p, err)
		}
		if !strings.HasPrefix(got, workDir) {
			t.Errorf("resolveComposePath(%q) = %q, want it under %q", p, got, workDir)
		}
	}

	// Relative paths that escape the checkout are rejected.
	for _, p := range []string{"../outside/compose.yml", "../../etc/compose.yml", "sub/../../escape.yml"} {
		if _, err := resolveComposePath(workDir, p); err == nil {
			t.Errorf("resolveComposePath(%q) should have been rejected", p)
		}
	}
}
