package source

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// testRepo is a real git repository on disk, used as a remote by the tests in
// this package. It is created with an isolated HOME and no system or global
// config, so the developer's own gitconfig — a signing key, a hooks path, a
// default branch — cannot change what these tests exercise.
type testRepo struct {
	Path   string // the bare repository, usable as a git remote
	work   string
	Commit string // HEAD of the work tree at the time it was published
}

func newTestRepo(t *testing.T, files map[string]string) *testRepo {
	t.Helper()
	root := t.TempDir()
	r := &testRepo{Path: filepath.Join(root, "remote.git"), work: filepath.Join(root, "work")}

	if err := os.MkdirAll(r.work, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	r.git(t, r.work, "init", "-q", "-b", "main", ".")
	r.write(t, files)
	r.git(t, r.work, "add", "-A", ".")
	r.commitAll(t, "initial")
	r.git(t, root, "clone", "-q", "--bare", r.work, r.Path)
	return r
}

// write replaces the work tree's files.
func (r *testRepo) write(t *testing.T, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(r.work, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

func (r *testRepo) commitAll(t *testing.T, msg string) {
	t.Helper()
	r.git(t, r.work, "-c", "user.email=test@infrena.invalid", "-c", "user.name=test",
		"-c", "commit.gpgsign=false", "commit", "-q", "-m", msg)
	r.Commit = r.gitOut(t, r.work, "rev-parse", "HEAD")
}

// Tag places a tag on the current HEAD and publishes it. annotated matters:
// an annotated tag's ls-remote line carries the TAG OBJECT's hash, not the
// commit's, which is the trap the fetch tests pin.
func (r *testRepo) Tag(t *testing.T, name string, annotated bool) {
	t.Helper()
	if annotated {
		r.git(t, r.work, "-c", "user.email=test@infrena.invalid", "-c", "user.name=test",
			"tag", "-f", "-a", name, "-m", name)
	} else {
		r.git(t, r.work, "tag", "-f", name)
	}
	r.git(t, r.Path, "fetch", "-q", "--force", r.work, "+refs/tags/*:refs/tags/*")
}

// Commit adds a commit to the work tree and publishes main.
func (r *testRepo) CommitFiles(t *testing.T, files map[string]string, msg string) {
	t.Helper()
	r.write(t, files)
	r.git(t, r.work, "add", "-A", ".")
	r.commitAll(t, msg)
	r.git(t, r.Path, "fetch", "-q", "--force", r.work, "+refs/heads/*:refs/heads/*")
}

func (r *testRepo) git(t *testing.T, dir string, args ...string) {
	t.Helper()
	r.gitOut(t, dir, args...)
}

func (r *testRepo) gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return trimLine(string(out))
}
