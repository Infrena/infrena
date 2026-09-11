package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitSource builds a Source that Parse cannot produce: KindGit whose location is
// a filesystem path. That combination is unreachable from configuration —
// TestParseNeverReturnsAGitSourceForAPath pins that — and it is how this package
// tests real git fetching with no network. Everything under test below is the
// production path; only the classification is bypassed.
func gitSource(location, ref string) Source {
	return Source{Kind: KindGit, Location: location, Ref: ref}
}

func TestFetchCommitChecksOutATagAndReturnsItsCommit(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v1.0.0", false)

	dir := filepath.Join(t.TempDir(), "checkout")
	got, err := gitRunner{}.fetchCommit(context.Background(), dir, gitSource(repo.Path, "v1.0.0"))
	if err != nil {
		t.Fatalf("fetchCommit: %v", err)
	}
	if got != repo.Commit {
		t.Errorf("commit = %q, want %q", got, repo.Commit)
	}
	if _, err := os.Stat(filepath.Join(dir, "module.yml")); err != nil {
		t.Errorf("module.yml is not in the checkout: %v — a fetch that does not check out a work tree gives stage 5 nothing to load", err)
	}
}

// TestFetchCommitPeelsAnAnnotatedTag is the trap measured as line 10. An
// annotated tag is an object in its own right, and `git ls-remote <url>
// refs/tags/v2.0.0` returns THAT object's hash, not the commit's. Record the
// tag object in modules.lock and every subsequent run compares it against the
// checked-out commit, disagrees, and reports a moved tag that never moved.
// Delete the "^{commit}" suffix in fetchCommit and this fails.
func TestFetchCommitPeelsAnAnnotatedTagToItsCommit(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v2.0.0", true)

	dir := filepath.Join(t.TempDir(), "checkout")
	got, err := gitRunner{}.fetchCommit(context.Background(), dir, gitSource(repo.Path, "v2.0.0"))
	if err != nil {
		t.Fatalf("fetchCommit: %v", err)
	}
	if got != repo.Commit {
		t.Errorf("commit = %q, want the COMMIT %q; an annotated tag's own object hash is not the commit it points at", got, repo.Commit)
	}
}

// TestFetchCommitResolvesAnAbbreviatedHash covers measured line 14: an
// abbreviated hash cannot be fetched as a ref at all, so the depth-1 route does
// not work and fetchCommit must take the full-fetch-then-resolve route. PLAN.md
// §11 spells a hash pin abbreviated ("9f3c1ab"), so this is the documented form.
func TestFetchCommitResolvesAnAbbreviatedHash(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})

	dir := filepath.Join(t.TempDir(), "checkout")
	got, err := gitRunner{}.fetchCommit(context.Background(), dir, gitSource(repo.Path, repo.Commit[:7]))
	if err != nil {
		t.Fatalf("fetchCommit: %v", err)
	}
	if got != repo.Commit {
		t.Errorf("commit = %q, want %q", got, repo.Commit)
	}
}

func TestFetchCommitReportsAMissingRef(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})

	dir := filepath.Join(t.TempDir(), "checkout")
	_, err := gitRunner{}.fetchCommit(context.Background(), dir, gitSource(repo.Path, "v9.9.9"))
	if err == nil {
		t.Fatal("fetchCommit succeeded for a tag that does not exist")
	}
	if !strings.Contains(err.Error(), "v9.9.9") {
		t.Errorf("error does not name the ref: %v", err)
	}
}

// TestLsRemoteCommitReportsAMissingRefRatherThanSucceeding covers measured line
// 12: `git ls-remote` for a ref that does not exist EXITS 0 with empty output.
// Treat exit status as the answer and a missing tag becomes an empty commit
// hash, which modules.lock would then record. Delete the empty-output check and
// this fails.
func TestLsRemoteCommitReportsAMissingRefRatherThanSucceeding(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v1.0.0", false)

	if _, err := (gitRunner{}).lsRemoteCommit(context.Background(), repo.Path, "v1.0.0"); err != nil {
		t.Fatalf("lsRemoteCommit for an existing tag: %v", err)
	}
	got, err := gitRunner{}.lsRemoteCommit(context.Background(), repo.Path, "v9.9.9")
	if err == nil {
		t.Fatalf("lsRemoteCommit returned %q and no error for a tag that does not exist", got)
	}
}

// TestLsRemoteCommitPeelsAnAnnotatedTag is the same trap as the fetch side, on
// the cheap path that 13.6 uses to detect a moved tag WITHOUT fetching. If this
// one regresses, every annotated-tag module reports a moved tag on every run.
func TestLsRemoteCommitPeelsAnAnnotatedTag(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v2.0.0", true)

	got, err := gitRunner{}.lsRemoteCommit(context.Background(), repo.Path, "v2.0.0")
	if err != nil {
		t.Fatalf("lsRemoteCommit: %v", err)
	}
	if got != repo.Commit {
		t.Errorf("commit = %q, want the commit %q, not the tag object", got, repo.Commit)
	}
}

// TestGitRefusesATransportTheLocationDoesNotName pins the second of Amendment
// 10c's guards. Measured: git applies url.<base>.insteadOf BEFORE choosing a
// transport, so a source that Parse approved as https:// can arrive at the
// transport layer as something else entirely — here, a local path. The
// per-invocation protocol policy is what refuses it.
//
// This test is the one that proves GIT_ALLOW_PROTOCOL is doing the work rather
// than Parse's allowlist, because Parse approved this URL. Delete the
// GIT_ALLOW_PROTOCOL entry in gitEnv and it fails: the rewrite succeeds and the
// fetch returns a commit.
//
// It is also the canary for the one risk in having a single knob. If a future
// git stops honouring GIT_ALLOW_PROTOCOL, this test starts failing on the day
// that happens rather than on the day someone exploits it.
func TestGitRefusesATransportTheLocationDoesNotName(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v1.0.0", false)

	home := t.TempDir()
	cfg := "[url \"" + repo.Path + "\"]\n\tinsteadOf = https://evil.example/repo\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write .gitconfig: %v", err)
	}
	t.Setenv("HOME", home)

	dir := filepath.Join(t.TempDir(), "checkout")
	got, err := gitRunner{}.fetchCommit(context.Background(), dir,
		gitSource("https://evil.example/repo", "v1.0.0"))
	if err == nil {
		t.Fatalf("fetchCommit returned %q; a source claiming https:// must not be served over another transport, however the user's gitconfig rewrites it", got)
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("error does not read as a refused transport: %v", err)
	}
}

// TestTransportOfNamesExactlyWhatTheLocationClaims is the unit half. The policy
// must be derived per invocation rather than fixed, so that the set of permitted
// transports is never wider than the one the source names.
func TestTransportOfNamesExactlyWhatTheLocationClaims(t *testing.T) {
	for location, want := range map[string]string{
		"https://github.com/acme/repo": "https",
		"ssh://git@host/acme/repo":     "ssh",
		"git@github.com:acme/repo":     "ssh",
		"/srv/repos/bare.git":          "file",
		"./bare.git":                   "file",
	} {
		if got := transportOf(location); got != want {
			t.Errorf("transportOf(%q) = %q, want %q", location, got, want)
		}
	}
}

// TestFetchDoesNotHangWhenTheRemoteAsksForAPassword is Amendment 10d's
// end-to-end half. The server is a loopback httptest server returning 401, so
// there is no network access; the environment carries a GIT_ASKPASS that blocks
// for a minute, which is what a private repository plus an interactive
// credential path looks like from the inside.
//
// What this test does NOT prove, measured during Task 12: deleting the
// GIT_ASKPASS override in gitEnv does not make it fail. gitEnv builds cmd.Env
// from a fixed allowlist that never contained GIT_ASKPASS, and os/exec replaces
// the child environment wholesale when Env is non-nil — so the t.Setenv above
// cannot reach git whether the override is there or not. The protection is real
// and is stronger than an override; it is just not the thing this test pins.
//
// TestGitEnvIsAnAllowlistNotTheParentEnvironment pins it, and can fail.
func TestFetchDoesNotHangWhenTheRemoteAsksForAPassword(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	blocking := filepath.Join(t.TempDir(), "askpass.sh")
	if err := os.WriteFile(blocking, []byte("#!/bin/sh\nsleep 60\necho nobody\n"), 0o755); err != nil {
		t.Fatalf("write askpass: %v", err)
	}
	t.Setenv("GIT_ASKPASS", blocking)
	t.Setenv("GIT_TERMINAL_PROMPT", "1")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := time.Now()
	dir := filepath.Join(t.TempDir(), "checkout")
	_, err := gitRunner{Timeout: 15 * time.Second}.fetchCommit(ctx, dir,
		gitSource(srv.URL+"/repo.git", "v1.0.0"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("fetchCommit succeeded against a server that demands credentials")
	}
	if elapsed > 10*time.Second {
		t.Errorf("fetchCommit took %s; it must fail rather than wait on a prompt nothing will answer", elapsed)
	}
}

// TestGitEnvDisablesPromptingAndPinsTheTransport asserts on the environment
// gitEnv builds rather than on git's behaviour, and that is deliberate: with no
// controlling terminal git fails fast whether or not GIT_TERMINAL_PROMPT is set
// (measured), so the terminal-prompt half of Amendment 10d has no end-to-end
// failure mode under `go test`. This test does have one — delete the line and it
// fails — and 12.4 covers the half that can be proved from the outside.
func TestGitEnvDisablesPromptingAndPinsTheTransport(t *testing.T) {
	t.Setenv("GIT_ASKPASS", "/usr/bin/inherited-askpass")
	t.Setenv("GIT_ALLOW_PROTOCOL", "ext:file:https")
	t.Setenv("GIT_SSH_COMMAND", "sh -c 'touch /tmp/pwned'")
	t.Setenv("GIT_CONFIG_GLOBAL", "/tmp/attacker.gitconfig")

	env := gitEnv("https://github.com/acme/repo")
	seen := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := seen[k]; dup {
			t.Errorf("%s appears twice in the environment; which one wins is then an accident of ordering", k)
		}
		seen[k] = v
	}

	for k, want := range map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_ASKPASS":         "/bin/false",
		"SSH_ASKPASS":         "/bin/false",
		"GIT_ALLOW_PROTOCOL":  "https",
	} {
		if got := seen[k]; got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	for _, k := range []string{"GIT_SSH_COMMAND", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM"} {
		if v, ok := seen[k]; ok {
			t.Errorf("%s was inherited as %q; the environment is built from an allowlist so that an open-ended set of git variables cannot be inherited", k, v)
		}
	}
	if seen["HOME"] == "" || seen["PATH"] == "" {
		t.Error("HOME and PATH must be passed through: ssh keys and the git binary itself are found through them")
	}
}

// TestGitEnvIsAnAllowlistNotTheParentEnvironment pins the property that actually
// protects the child process: gitEnv composes cmd.Env from a fixed allowlist, so
// nothing hostile in the parent environment reaches git at all.
//
// Two plausible edits break it, one per half. Replace the map with
// append(os.Environ(), ...) and the "must not appear" assertions fail. Drop a
// name from the allowlist loop and the "carried through" assertion fails.
//
// GIT_CONFIG_GLOBAL is the pointed case: it is how a hostile parent would inject
// url.<base>.insteadOf rewrite rules, which git applies BEFORE its transport
// check. Keeping it out of the child is not the whole defence — a rule in the
// user's own ~/.gitconfig still applies, because HOME is allowlisted — which is
// precisely why GIT_ALLOW_PROTOCOL is set here rather than relying on Parse.
func TestGitEnvIsAnAllowlistNotTheParentEnvironment(t *testing.T) {
	t.Setenv("GIT_ASKPASS", "/tmp/hostile-askpass")
	t.Setenv("GIT_ALLOW_PROTOCOL", "file:ext")
	t.Setenv("GIT_CONFIG_GLOBAL", "/tmp/hostile-gitconfig")
	t.Setenv("HOME", "/tmp/home-under-test")

	get := func(env []string, k string) (string, bool) {
		for _, kv := range env {
			if name, v, ok := strings.Cut(kv, "="); ok && name == k {
				return v, true
			}
		}
		return "", false
	}
	env := gitEnv("https://example.invalid/repo")

	if v, _ := get(env, "GIT_ASKPASS"); v != "/bin/false" {
		t.Errorf("GIT_ASKPASS = %q, want \"/bin/false\": the parent's value reached git", v)
	}
	if v, _ := get(env, "GIT_ALLOW_PROTOCOL"); v != "https" {
		t.Errorf("GIT_ALLOW_PROTOCOL = %q, want \"https\": the parent's value reached git, which would reopen ext::", v)
	}
	if v, ok := get(env, "GIT_CONFIG_GLOBAL"); ok {
		t.Errorf("GIT_CONFIG_GLOBAL = %q reached git; gitEnv is not an allowlist", v)
	}
	if v, ok := get(env, "HOME"); !ok || v != "/tmp/home-under-test" {
		t.Errorf("HOME = %q (present=%v), want it carried through the allowlist", v, ok)
	}
}
