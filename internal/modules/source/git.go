package source

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const defaultGitTimeout = 2 * time.Minute

// fetchRef is where a fetched ref is parked inside the cache checkout. The name
// is fixed and namespaced so that nothing a remote can name collides with it.
const fetchRef = "refs/infrena/target"

// gitRunner runs the git binary. It is the only code in this repository that
// executes another program.
type gitRunner struct {
	Timeout time.Duration
}

func (g gitRunner) timeout() time.Duration {
	if g.Timeout > 0 {
		return g.Timeout
	}
	return defaultGitTimeout
}

// run invokes git in dir (empty for a repository-less command). location is the
// remote this invocation talks to, and it decides the transport policy — not the
// arguments, which may not even contain it.
//
// Every invocation gets:
//
//   - a fresh environment built from an allowlist (see gitEnv), so an inherited
//     GIT_ASKPASS, GIT_SSH_COMMAND or GIT_ALLOW_PROTOCOL cannot change what runs;
//   - exactly one permitted transport, named in GIT_ALLOW_PROTOCOL by gitEnv,
//     because git applies url.<base>.insteadOf BEFORE choosing a transport and a
//     rewrite can therefore turn an allowlisted https:// URL into something else;
//   - an empty init.templateDir and a hooks path that does not exist, so a
//     template directory cannot install a hook that runs during checkout;
//   - a "--" before the first positional argument, because git parses an
//     argument beginning with "-" as an option and a URL is an argument.
func (g gitRunner) run(ctx context.Context, dir, location string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout())
	defer cancel()

	// No -c protocol.* here: GIT_ALLOW_PROTOCOL in gitEnv is the transport
	// policy and overrides these in both directions, so a second spelling
	// could only agree with it or contradict it.
	full := []string{
		"-c", "init.templateDir=",
		"-c", "init.defaultBranch=infrena",
		"-c", "advice.detachedHead=false",
		"-c", "core.hooksPath=" + filepath.Join(dir, ".infrena-no-hooks"),
	}
	full = append(full, args...)

	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = gitEnv(location)
	cmd.Stdin = nil

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("git %s timed out after %s: %s", args[0], g.timeout(), detail)
		}
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], detail)
	}
	return stdout.String(), nil
}

// gitEnv builds the environment for a git invocation from an allowlist.
//
// A denylist was the alternative and it is the wrong shape: the variables that
// must not be inherited (GIT_ASKPASS, GIT_SSH_COMMAND, GIT_ALLOW_PROTOCOL,
// GIT_CONFIG_*, GIT_PROTOCOL_FROM_USER, ...) are an open-ended set, and a
// denylist over an open-ended set is a list that gets updated after an incident.
//
// The passthrough list is what git needs to reach a real remote on a real
// machine: HOME (ssh keys, and the user's own gitconfig, whose insteadOf rules
// the protocol policy above neutralises), PATH, the ssh agent socket, proxy
// settings, and TLS trust stores. GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM are
// deliberately not passed through, and neither is the system config disabled: a
// corporate /etc/gitconfig carries proxy settings that the fetch needs.
//
// credential.helper is likewise left alone. A helper is non-interactive by
// design and is how a private module repository authenticates without a prompt;
// clearing it would break exactly the case GIT_ASKPASS exists to keep from
// hanging.
func gitEnv(location string) []string {
	env := map[string]string{}
	for _, k := range []string{
		"HOME", "PATH", "SSH_AUTH_SOCK", "TMPDIR", "USER", "LOGNAME",
		"http_proxy", "https_proxy", "no_proxy",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "GIT_SSL_CAINFO", "GIT_SSL_CAPATH",
	} {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}

	// Set last, and into a map, so nothing above can be inherited into them.
	env["GIT_TERMINAL_PROMPT"] = "0"
	env["GIT_ASKPASS"] = "/bin/false"
	env["SSH_ASKPASS"] = "/bin/false"
	env["SSH_ASKPASS_REQUIRE"] = "never"
	// The transport policy, and the only one. GIT_ALLOW_PROTOCOL overrides
	// -c protocol.*.allow in both directions, and setting it here overwrites
	// whatever the user's environment held. It is enforced after
	// url.<base>.insteadOf rewriting, which is what makes it — rather than
	// Parse's allowlist — the thing that stops a rewritten URL.
	env["GIT_ALLOW_PROTOCOL"] = transportOf(location)

	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out) // deterministic, and it makes a duplicate key impossible to hide
	return out
}

// transportOf names the one transport a location claims. The policy is derived
// per invocation rather than fixed at a set of allowed schemes, so the permitted
// set is never wider than the single source being fetched.
func transportOf(location string) string {
	if scheme, _, ok := splitScheme(location); ok {
		return scheme
	}
	if _, _, ok := splitSCP(location); ok {
		return "ssh"
	}
	return "file"
}

// lsRemoteCommit resolves ref to the commit it names without transferring any
// objects. It is how a tag pin is checked for movement on a run that would
// otherwise not touch the network.
//
// Both peeling routes are used, one per path: this function asks the remote to
// peel ("^{}"), and checkout asks the local object store ("^{commit}") after
// fetching. They are not alternatives — the ls-remote path has no objects to
// read and the fetch path has no reason to make a second round trip — and they
// must produce the same kind of hash, because the moved-tag comparison in
// modules.lock puts one against the other. Comparing a tag object against a
// commit would report every annotated tag as moved on every run.
//
// Two behaviours of ls-remote drive the shape here:
//
//   - an annotated tag's own line carries the tag object's hash, and the commit
//     appears only on the peeled "^{}" line, which is printed only if asked for;
//   - a ref that does not exist is exit status 0 with no output.
func (g gitRunner) lsRemoteCommit(ctx context.Context, location, ref string) (string, error) {
	out, err := g.run(ctx, "", location, "ls-remote", "--", location,
		"refs/tags/"+ref+"^{}", "refs/tags/"+ref, "refs/heads/"+ref)
	if err != nil {
		return "", err
	}

	var peeled, plain string
	for line := range strings.SplitSeq(out, "\n") {
		hash, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		if strings.HasSuffix(name, "^{}") {
			peeled = hash
			continue
		}
		if plain == "" {
			plain = hash
		}
	}
	if peeled != "" {
		return peeled, nil
	}
	if plain == "" {
		return "", fmt.Errorf("the remote has no tag or branch named %q", ref)
	}
	return plain, nil
}

// fetchCommit creates a checkout of s at dir and returns the commit it is at.
//
// Two routes, because an abbreviated hash cannot be fetched as a ref at all,
// while a tag or a full hash can be fetched at depth 1:
//
//   - depth 1 for a tag or a 40-character hash;
//   - a full fetch followed by a local rev-parse for an abbreviated hash, and as
//     the fallback when the depth-1 route fails for a full hash, since a server
//     need not serve an arbitrary object by name.
func (g gitRunner) fetchCommit(ctx context.Context, dir string, s Source) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if _, err := g.run(ctx, dir, s.Location, "init", "-q"); err != nil {
		return "", err
	}

	if !s.PinnedToHash() || len(s.Ref) == 40 {
		// The refspec begins with "+", so the ref can never be read as an
		// option however it was written; Parse refuses a leading "-" as well.
		commit, err := g.fetchShallow(ctx, dir, s)
		if err == nil {
			return commit, nil
		}
		if !s.PinnedToHash() {
			return "", err
		}
	}
	return g.fetchFull(ctx, dir, s)
}

func (g gitRunner) fetchShallow(ctx context.Context, dir string, s Source) (string, error) {
	if _, err := g.run(ctx, dir, s.Location, "fetch", "-q", "--depth=1", "--",
		s.Location, "+"+s.Ref+":"+fetchRef); err != nil {
		return "", err
	}
	return g.checkout(ctx, dir, s.Location, fetchRef)
}

func (g gitRunner) fetchFull(ctx context.Context, dir string, s Source) (string, error) {
	if _, err := g.run(ctx, dir, s.Location, "fetch", "-q", "--tags", "--",
		s.Location, "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return "", err
	}
	return g.checkout(ctx, dir, s.Location, s.Ref)
}

// checkout resolves rev to a commit and checks it out detached. The "^{commit}"
// suffix is load-bearing: rev may name an annotated tag, whose own hash is not
// the commit's, and the hash recorded in modules.lock must be the commit or
// every later run compares two different kinds of object.
func (g gitRunner) checkout(ctx context.Context, dir, location, rev string) (string, error) {
	out, err := g.run(ctx, dir, location, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolving %q: %w", rev, err)
	}
	commit := trimLine(out)
	if _, err := g.run(ctx, dir, location, "checkout", "-q", "--detach", commit); err != nil {
		return "", err
	}
	return commit, nil
}

func trimLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
