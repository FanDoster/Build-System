package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/logbus"
	"github.com/FanDoster/Build-System/internal/models"
)

// lfsRepo builds a real local repository whose one large file is tracked by
// Git LFS, and returns its path. A file:// remote is enough: LFS resolves its
// endpoint from the remote, and for a local one the objects are copied.
func lfsRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs not available")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("lfs", "install", "--local")
	git("lfs", "track", "*.bin")
	// Recognisable content, and big enough that a pointer file is obviously
	// not it: an LFS pointer is about 130 bytes.
	if err := os.WriteFile(filepath.Join(dir, "asset.bin"),
		[]byte(strings.Repeat("REAL-CONTENT-", 4096)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-qm", "an asset tracked by lfs")
	return dir
}

// The failure this exists to prevent is silent. A repository using LFS clones
// perfectly well without git-lfs, and every large file arrives as a ~130-byte
// text pointer that Docker then bakes into an image nothing reports as wrong.
func TestACloneOfAnLFSRepoGetsRealContentNotPointers(t *testing.T) {
	repoDir := lfsRepo(t)

	// Clone exactly as the runner does.
	workDir := t.TempDir()
	dest := filepath.Join(workDir, "checkout")
	cmd := exec.Command("git", "clone", "--depth", "1", "--branch", "main", "file://"+repoDir, dest)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}

	got, err := os.ReadFile(filepath.Join(dest, "asset.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(string(got), "version https://git-lfs") {
		t.Fatalf("the asset arrived as an LFS pointer, not content:\n%s", got[:min(len(got), 200)])
	}
	if !strings.HasPrefix(string(got), "REAL-CONTENT-") {
		t.Errorf("unexpected content: %.60q", got)
	}

	// And the guard agrees this repository needs LFS at all.
	if !usesLFS(dest) {
		t.Error("usesLFS did not recognise a repository that tracks files with lfs")
	}
}

// The guard reads .gitattributes rather than asking git-lfs, because the case
// it exists for is git-lfs being absent — a check needing the missing tool to
// notice the tool is missing would never fire.
func TestUsesLFSNeedsNoGitLFS(t *testing.T) {
	dir := t.TempDir()
	if usesLFS(dir) {
		t.Error("reported LFS for a checkout with no .gitattributes")
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"),
		[]byte("*.txt text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if usesLFS(dir) {
		t.Error("reported LFS for a .gitattributes that declares none")
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"),
		[]byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !usesLFS(dir) {
		t.Error("did not recognise a real LFS declaration")
	}
}

// A build of an LFS repository must reach the docker step rather than being
// refused, on a machine that has git-lfs.
func TestAnLFSProjectBuildsThroughTheRunner(t *testing.T) {
	repoDir := lfsRepo(t)

	binDir := t.TempDir()
	script := "#!/bin/sh\necho \"stub docker $*\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	p := &models.Project{
		Name: "lfsapp", RepoURL: "file://" + repoDir, Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "lfsapp",
	}
	if err := database.CreateProject(p); err != nil {
		t.Fatal(err)
	}
	b := &models.Build{ProjectID: p.ID, Status: models.StatusPending, CommitSHA: "manual"}
	if err := database.CreateBuild(b); err != nil {
		t.Fatal(err)
	}

	r := New(database, make(chan *models.Build), logbus.New())
	r.runBuild(context.Background(), b, time.Now().UTC())

	got, err := database.GetBuild(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.StatusSuccess {
		t.Fatalf("status = %q, want success\n%s", got.Status, got.Log)
	}
	if strings.Contains(got.Log, "git-lfs is not installed") {
		t.Error("the LFS guard refused a build on a machine that has git-lfs")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// The guard, exercised against a machine that genuinely lacks git-lfs — which
// is the state the build image was in until 2026-07-30.
//
// Without the guard this build goes GREEN: the clone succeeds, asset.bin is a
// 130-byte text pointer, Docker bakes it in, and the image ships wrong with
// nothing anywhere reporting a problem. The test asserts the opposite: a loud,
// specific failure naming the fix.
func TestABuildIsRefusedWhenLFSIsNeededAndMissing(t *testing.T) {
	repoDir := lfsRepo(t) // built while git-lfs is still on PATH

	// A PATH with real git and a stub docker, but no git-lfs at all.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	binDir := t.TempDir()
	if err := os.Symlink(realGit, filepath.Join(binDir, "git")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "docker"),
		[]byte("#!/bin/sh\necho \"stub docker $*\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	// An empty HOME so this machine's own `git lfs install` cannot leak in and
	// configure the filter the container would not have.
	t.Setenv("HOME", t.TempDir())

	if _, err := exec.LookPath("git-lfs"); err == nil {
		t.Fatal("git-lfs is still reachable; this test proves nothing")
	}

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	p := &models.Project{
		Name: "lfsapp", RepoURL: "file://" + repoDir, Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "lfsapp",
	}
	if err := database.CreateProject(p); err != nil {
		t.Fatal(err)
	}
	b := &models.Build{ProjectID: p.ID, Status: models.StatusPending, CommitSHA: "manual"}
	if err := database.CreateBuild(b); err != nil {
		t.Fatal(err)
	}

	r := New(database, make(chan *models.Build), logbus.New())
	r.runBuild(context.Background(), b, time.Now().UTC())

	got, err := database.GetBuild(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == models.StatusSuccess {
		t.Fatal("the build went green against LFS pointer files — this is the silent wrong image the guard exists to prevent")
	}
	if !strings.Contains(got.Log, "git-lfs is not installed") {
		t.Errorf("failed, but not with the explanation an operator needs:\n%s", got.Log)
	}
	if !strings.Contains(got.Log, "apk add git-lfs") {
		t.Error("the failure does not say how to fix it")
	}
}
