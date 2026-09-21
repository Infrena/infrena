package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/backendhost"
	"github.com/infrena/infrena/internal/pluginhost"
	"github.com/infrena/infrena/internal/plugins"
	"github.com/infrena/infrena/internal/plugins/remote"
)

// Tests for `infrena plugins install` and `infrena plugins verify`.
//
// THE ORDER OF THE STEPS IS THE DESIGN, so most of these tests are about what
// does NOT happen: a source the user never approved does not become one, a run
// with nobody at the terminal never approves anything, two sources answering
// one name install neither, and an archive that fails its checksum is never
// opened. In every one of those, the assertion that matters is that nothing
// reached disk.

// THE SECURITY TEST. A project may NAME a source, but naming it must not be
// enough to download an executable from it. Otherwise `git clone && infrena
// plan` lets a repository introduce a place infrena fetches binaries from.
func TestInstallFromAnUntrustedOwnerRefusesWithoutApproval(t *testing.T) {
	dir := newProjectNamingSource(t, "hetzner", "github.com/someone/infrena-provider-hetzner")
	trustSources(t)
	srv := fakeGitHubServing(t, "someone", "hetzner", "1.0.0")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	// Empty stdin: nothing is there to answer, so nothing can be approved.
	_, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "hetzner")

	if code == ExitOK {
		t.Fatal("installed from an owner the user never approved")
	}
	if !strings.Contains(stderr, "someone") {
		t.Errorf("refusal does not name the owner to approve:\n%s", stderr)
	}
	if n := installedBinaries(t, dir); n != 0 {
		t.Errorf("%d binaries were written despite refusing", n)
	}
	// And the approval was not recorded either, which would have made the next
	// run succeed for a reason nobody consented to.
	if trustedFileContains(t, "someone") {
		t.Error("a refused source was written to the trusted sources file")
	}
}

// Non-interactive NEVER approves. A pipeline that silently starts trusting a
// new binary publisher is the failure this subsection exists to prevent, and
// the point of this case over the one above is that stdin SAYS YES: the answer
// is refused because nobody is reading the prompt, not because no answer came.
func TestNonInteractiveNeverApproves(t *testing.T) {
	dir := newProjectNamingSource(t, "hetzner", "github.com/someone/infrena-provider-hetzner")
	trustSources(t)
	srv := fakeGitHubServing(t, "someone", "hetzner", "1.0.0")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	_, stderr, code := runCommandWithStdin(t, dir, "yes\n", "plugins", "install", "hetzner", "--output",
		filepath.Join(t.TempDir(), "r.ndjson"))

	if code == ExitOK {
		t.Fatal("machine mode approved an owner")
	}
	if !strings.Contains(stderr, "plugins") {
		t.Errorf("refusal does not say how to approve:\n%s", stderr)
	}
	if n := installedBinaries(t, dir); n != 0 {
		t.Errorf("%d binaries were written despite refusing", n)
	}
	if trustedFileContains(t, "someone") {
		t.Error("machine mode recorded an approval")
	}
}

// The official owner is trusted from the start, so this path needs no prompt.
func TestInstallFromTheOfficialOwnerNeedsNoApproval(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)
	srv := fakeGitHubServing(t, "infrena", "aws", "0.4.0")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	stdout, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "aws")

	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if n := installedBinaries(t, dir); n != 1 {
		t.Fatalf("installed %d binaries, want 1", n)
	}

	// INTO THE DIRECTORY THE LOADER ALREADY SEARCHES. Install is not allowed to
	// invent a location: pluginhost.DefaultSearch looks in <project>/.infrena/
	// plugins, so a plugin installed anywhere else would be installed and
	// invisible.
	path := filepath.Join(dir, ".infrena", "plugins", pluginhost.BinaryName("aws"))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("nothing at %s: %v", path, err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		t.Error("the installed binary is not executable")
	}
}

// The checksum is the point of the whole exercise.
func TestAnArchiveThatFailsItsChecksumIsRefused(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)
	srv := fakeGitHubServingTamperedArchive(t, "infrena", "aws", "0.4.0")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	_, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "aws")

	if code == ExitOK {
		t.Fatal("an archive failing its checksum was installed")
	}
	if !strings.Contains(stderr, "checksum") {
		t.Errorf("refusal does not say why:\n%s", stderr)
	}
	if n := installedBinaries(t, dir); n != 0 {
		t.Errorf("%d binaries were written despite a bad checksum", n)
	}
	// Nor a lock entry, which would say a binary nobody installed is approved.
	if l, _ := plugins.ReadLockfile(dir); l != nil && len(l.Plugins) != 0 {
		t.Errorf("a lock entry was written despite refusing: %+v", l.Plugins)
	}
}

func TestInstallWritesTheLockfile(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)
	srv := fakeGitHubServing(t, "infrena", "aws", "0.4.0")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	stdout, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "aws")
	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	l, err := plugins.ReadLockfile(dir)
	if err != nil || l == nil {
		t.Fatalf("no lockfile written: %v", err)
	}
	e, ok := l.Plugins["aws"]
	if !ok || e.Version != "0.4.0" || len(e.Checksums) == 0 {
		t.Fatalf("lock entry = %+v", e)
	}
	if e.Source != "github.com/infrena" {
		t.Errorf("lock entry source = %q, want the source it came from", e.Source)
	}

	// THE LOCK RECORDS THE BINARY, NOT THE ARCHIVE. The launcher verifies a
	// binary on disk against this, so a checksum of the .tar.gz would make every
	// launch fail and would be caught nowhere else.
	path := filepath.Join(dir, ".infrena", "plugins", pluginhost.BinaryName("aws"))
	sum, err := plugins.FileChecksum(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Checksums[runtime.GOOS+"/"+runtime.GOARCH]; got != sum {
		t.Errorf("lock records %q, but the installed binary hashes to %q", got, sum)
	}
	if err := l.Check("aws", runtime.GOOS+"/"+runtime.GOARCH, sum); err != nil {
		t.Errorf("the lock install just wrote does not accept the binary install just wrote: %v", err)
	}
}

// Two owners answering one name: present both, install neither.
func TestInstallRefusesToChooseBetweenTwoSources(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t, "github.com/mycorp")
	srv := fakeGitHubServingTwoOwners(t, "hetzner", "1.0.0")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	_, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "hetzner")

	if code == ExitOK {
		t.Fatal("install chose between two sources")
	}
	if !strings.Contains(stderr, "does not choose") {
		t.Errorf("refusal does not explain:\n%s", stderr)
	}
	// Both are named, because the user's next act is to pick one.
	for _, want := range []string{"github.com/infrena", "github.com/mycorp"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("refusal does not name %s:\n%s", want, stderr)
		}
	}
	if n := installedBinaries(t, dir); n != 0 {
		t.Errorf("%d binaries were written despite refusing", n)
	}
}

// The other half of the trust rule: a person who IS there can approve, and the
// approval is written to their own file so it is asked once.
func TestApprovingAtThePromptInstallsAndRecordsTheSource(t *testing.T) {
	dir := newProjectNamingSource(t, "hetzner", "github.com/someone/infrena-provider-hetzner")
	trustSources(t)
	srv := fakeGitHubServing(t, "someone", "hetzner", "1.0.0")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	stdout, stderr, code := runCommandWithStdin(t, dir, "yes\n", "plugins", "install", "hetzner")

	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "someone") {
		t.Errorf("the prompt did not say who was being approved:\n%s", stdout)
	}
	if n := installedBinaries(t, dir); n != 1 {
		t.Errorf("installed %d binaries, want 1", n)
	}
	if !trustedFileContains(t, "github.com/someone/infrena-provider-hetzner") {
		t.Error("the approval was not recorded, so it would be asked again next time")
	}
}

// Approving ONE repository must not widen to its owner. The prompt approved
// what it named, and a file saying otherwise would grant a trust nobody was
// asked for.
func TestApprovingARepositoryDoesNotTrustTheWholeOwner(t *testing.T) {
	dir := newProjectNamingSource(t, "hetzner", "github.com/someone/infrena-provider-hetzner")
	trustSources(t)
	srv := fakeGitHubServing(t, "someone", "hetzner", "1.0.0")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	if _, stderr, code := runCommandWithStdin(t, dir, "yes\n", "plugins", "install", "hetzner"); code != ExitOK {
		t.Fatalf("exit = %d\nstderr:\n%s", code, stderr)
	}

	home, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := plugins.LoadTrusted(home)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := plugins.ParseSource("github.com/someone")
	if err != nil {
		t.Fatal(err)
	}
	if plugins.IsTrusted(trusted, owner) {
		t.Error("approving one repository trusted its whole owner")
	}
}

// Declining is not a failure of nerve to be worked around: nothing is
// downloaded and nothing is remembered.
func TestDecliningTheApprovalInstallsNothing(t *testing.T) {
	dir := newProjectNamingSource(t, "hetzner", "github.com/someone/infrena-provider-hetzner")
	trustSources(t)
	srv := fakeGitHubServing(t, "someone", "hetzner", "1.0.0")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	_, stderr, code := runCommandWithStdin(t, dir, "no\n", "plugins", "install", "hetzner")

	if code == ExitOK {
		t.Fatal("a declined approval installed anyway")
	}
	if !strings.Contains(stderr, "cancelled") {
		t.Errorf("refusal does not say the install was cancelled:\n%s", stderr)
	}
	if n := installedBinaries(t, dir); n != 0 {
		t.Errorf("%d binaries were written after declining", n)
	}
	if trustedFileContains(t, "someone") {
		t.Error("a declined source was recorded as trusted")
	}
}

// A forge that would not answer must never be reported as a plugin that does
// not exist: that sends a user to check a spelling that was right and hides a
// condition clearing itself within the hour.
func TestInstallDoesNotReportARateLimitAsAMissingPlugin(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)
	srv := fakeGitHubRateLimited(t)
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	_, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "aws")

	if code == ExitOK {
		t.Fatal("install succeeded against a forge that answered nothing")
	}
	if !strings.Contains(stderr, "rate limit") {
		t.Errorf("stderr does not report the rate limit:\n%s", stderr)
	}
	if strings.Contains(stderr, "in any source") {
		t.Errorf("a rate limit was rendered as the plugin not existing:\n%s", stderr)
	}
}

// A release that publishes no archive for this machine is told apart from a
// release with no checksums at all, and from a plugin that does not exist.
func TestInstallRefusesWhenTheReleasePublishesNoArchiveForThisPlatform(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)
	srv := fakeGitHubServingOtherPlatformOnly(t, "infrena", "aws", "0.4.0")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	_, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "aws")

	if code == ExitOK {
		t.Fatal("install succeeded with no archive for this machine")
	}
	if !strings.Contains(stderr, remote.AssetName(providerBinary("aws"), "0.4.0", currentPlatform())) {
		t.Errorf("refusal does not name the archive that is missing:\n%s", stderr)
	}
	if n := installedBinaries(t, dir); n != 0 {
		t.Errorf("%d binaries were written despite refusing", n)
	}
}

// A version nobody publishes is answered with what IS published, rather than
// with a bare failure.
func TestInstallingAVersionNobodyPublishesSaysWhatIsPublished(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)
	srv := fakeGitHubServing(t, "infrena", "aws", "0.4.0")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	_, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "aws@9.9.9")

	if code == ExitOK {
		t.Fatal("a version nobody publishes was installed")
	}
	if !strings.Contains(stderr, "0.4.0") {
		t.Errorf("refusal does not say what is published:\n%s", stderr)
	}
}

// `verify` re-reads the lock and re-hashes what is on disk. This is the whole
// reason the lock is committed: a binary replaced afterwards is caught at the
// next command rather than never.
func TestVerifyPassesAfterInstallAndFailsOnceTheBinaryChanges(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)
	srv := fakeGitHubServing(t, "infrena", "aws", "0.4.0")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	if _, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "aws"); code != ExitOK {
		t.Fatalf("install: exit = %d\n%s", code, stderr)
	}

	stdout, stderr, code := runCommand(t, dir, "plugins", "verify")
	if code != ExitOK {
		t.Fatalf("verify after install: exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "aws") {
		t.Errorf("verify does not mention the plugin it checked:\n%s", stdout)
	}

	// Somebody replaces the binary.
	path := filepath.Join(dir, ".infrena", "plugins", pluginhost.BinaryName("aws"))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("x"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	_, stderr, code = runCommand(t, dir, "plugins", "verify")
	if code == ExitOK {
		t.Fatal("verify passed a binary that had been replaced")
	}
	if !strings.Contains(stderr, "aws") {
		t.Errorf("the failure does not name the plugin:\n%s", stderr)
	}
}

// A project that has installed nothing has nothing to verify, and that is an
// answer rather than a failure.
func TestVerifyWithNoLockfileSaysSoAndExitsZero(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)

	stdout, _, code := runCommand(t, dir, "plugins", "verify")

	if code != ExitOK {
		t.Errorf("exit = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(stdout, "nothing to verify") {
		t.Errorf("output does not say there is nothing to verify:\n%s", stdout)
	}
}

// A plugin the lock records and the machine no longer has is a failure rather
// than a skip: something removed it, and reporting the project as verified
// would be reporting on a binary that is not there.
func TestVerifyFailsWhenALockedBinaryIsMissing(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)
	l := &plugins.Lockfile{Version: plugins.LockVersion, Plugins: map[string]plugins.LockEntry{
		"ghost": {Version: "1.0.0", Source: "github.com/infrena", Checksums: map[string]string{
			runtime.GOOS + "/" + runtime.GOARCH: "sha256:whatever",
		}},
	}}
	if err := l.Write(dir); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runCommand(t, dir, "plugins", "verify")

	if code == ExitOK {
		t.Fatal("verify passed with a recorded binary missing")
	}
	if !strings.Contains(stderr, "ghost") {
		t.Errorf("the failure does not name the plugin:\n%s", stderr)
	}
}

// THE KIND COMES FROM THE PROJECT, because the project already said.
//
// A name declared only as a backend installs the backend, without asking and
// without a flag. Asking a user to repeat what infrena.yml already says is asking
// them to keep two places in step.
func TestInstallResolvesTheKindFromTheProject(t *testing.T) {
	dir := newProjectWithBackend(t, "s3")
	trustSources(t)
	srv := fakeGitHubServingBothKinds(t, "s3")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	stdout, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "s3")

	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	// It must say which it chose, because it chose without being told.
	if !strings.Contains(stdout, "backend") {
		t.Errorf("output does not say which kind was installed:\n%s", stdout)
	}
	if !installedBinaryExists(t, dir, "infrena-backend-s3") {
		t.Error("the backend binary was not installed")
	}
	if installedBinaryExists(t, dir, "infrena-plugin-s3") {
		t.Error("a provider was installed and nothing asked for one")
	}
}

// The lock entry `plugins list` reads a backend's version from is the
// repository's name, not the bare one: a provider s3 and a backend s3 are two
// binaries and cannot share a key.
func TestInstallingABackendRecordsItUnderItsOwnLockKey(t *testing.T) {
	dir := newProjectWithBackend(t, "s3")
	trustSources(t)
	srv := fakeGitHubServingBothKinds(t, "s3")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	if _, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "s3"); code != ExitOK {
		t.Fatalf("exit = %d\n%s", code, stderr)
	}

	l, err := plugins.ReadLockfile(dir)
	if err != nil || l == nil {
		t.Fatalf("no lockfile written: %v", err)
	}
	e, ok := l.Plugins[backendhost.LockKey("s3")]
	if !ok || e.Version != "1.0.0" {
		t.Fatalf("lock entry under %q = %+v", backendhost.LockKey("s3"), e)
	}
	if _, ok := l.Plugins["s3"]; ok {
		t.Error("a backend was recorded under the bare name, where a provider of that name belongs")
	}
}

// `verify` has to accept what install just wrote, whichever kind it was. The
// lock key is the repository's name and the binary on disk is
// infrena-backend-s3, so a verify that looked up every entry as a provider
// would report a backend it had just installed as missing.
func TestVerifyPassesAfterInstallingABackend(t *testing.T) {
	dir := newProjectWithBackend(t, "s3")
	trustSources(t)
	srv := fakeGitHubServingBothKinds(t, "s3")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	if _, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "s3"); code != ExitOK {
		t.Fatalf("install: exit = %d\n%s", code, stderr)
	}

	stdout, stderr, code := runCommand(t, dir, "plugins", "verify")
	if code != ExitOK {
		t.Fatalf("verify after installing a backend: exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "s3") {
		t.Errorf("verify does not mention the backend it checked:\n%s", stdout)
	}
}

// A project naming both needs both. That is not choosing between them, it is
// doing what was asked.
func TestInstallInstallsBothWhenTheProjectNeedsBoth(t *testing.T) {
	dir := newProjectWithProviderAndBackend(t, "s3")
	trustSources(t)
	srv := fakeGitHubServingBothKinds(t, "s3")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	_, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "s3")

	if code != ExitOK {
		t.Fatalf("exit = %d\n%s", code, stderr)
	}
	if !installedBinaryExists(t, dir, "infrena-backend-s3") || !installedBinaryExists(t, dir, "infrena-plugin-s3") {
		t.Error("a project naming both did not get both")
	}
}

// With no project to ask and both kinds published, infrena must NOT pick. It
// is the same rule as two owners answering one name: choosing silently gives
// the user a thing they did not name.
func TestInstallRefusesToGuessWhenThereIsNoProjectToAsk(t *testing.T) {
	dir := t.TempDir() // deliberately not a project
	trustSources(t)
	srv := fakeGitHubServingBothKinds(t, "s3")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	_, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "s3", "--global")

	if code == ExitOK {
		t.Fatal("install guessed a kind with nothing to go on")
	}
	for _, want := range []string{"provider", "backend", "--kind"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("refusal does not offer %q:\n%s", want, stderr)
		}
	}
}

// --kind is the fallback for exactly that case.
func TestKindFlagResolvesTheAmbiguity(t *testing.T) {
	dir := t.TempDir()
	trustSources(t)
	srv := fakeGitHubServingBothKinds(t, "s3")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	_, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "s3", "--global", "--kind", "backend")

	if code != ExitOK {
		t.Fatalf("exit = %d\n%s", code, stderr)
	}
}

// An unambiguous name needs no flag and no project: only one thing is called
// hetzner, so there is nothing to disambiguate.
func TestAnUnambiguousNameNeedsNoProjectAndNoFlag(t *testing.T) {
	dir := t.TempDir()
	trustSources(t)
	srv := fakeGitHubServingOneBackend(t, "hetzner")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	_, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "hetzner", "--global")

	if code != ExitOK {
		t.Fatalf("a name with only one candidate was refused: exit %d\n%s", code, stderr)
	}
}

// --kind naming something that does not exist is an error saying so, not a
// silent fall back to the other kind.
func TestKindFlagForAKindThatDoesNotExistIsAnError(t *testing.T) {
	dir := t.TempDir()
	trustSources(t)
	srv := fakeGitHubServingOneBackend(t, "hetzner")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	_, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "hetzner", "--global", "--kind", "provider")

	if code == ExitOK {
		t.Fatal("asking for a provider installed a backend")
	}
	if !strings.Contains(stderr, "backend") {
		t.Errorf("error does not say what DOES exist:\n%s", stderr)
	}
}

// A --kind nobody can act on is refused before a single request, naming the two
// words that are accepted.
func TestAKindThatIsNeitherWordIsRefused(t *testing.T) {
	dir := t.TempDir()
	trustSources(t)

	_, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "s3", "--global", "--kind", "plugin")

	if code == ExitOK {
		t.Fatal("an unrecognised --kind was accepted")
	}
	for _, want := range []string{"provider", "backend"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("refusal does not name %q as a choice:\n%s", want, stderr)
		}
	}
}

// newProjectWithBackend is a project whose state lives in a backend of this
// name and which declares no provider at all, so the only kind it can mean is
// the backend.
func newProjectWithBackend(t *testing.T, name string) string {
	t.Helper()
	return projectDir(t, fmt.Sprintf(`
project: myapp
backend:
  plugin: %s
  bucket: acme-state
`, name))
}

// newProjectWithProviderAndBackend names one plugin twice, once as each kind,
// which is a project that needs two binaries rather than a project that is
// ambiguous.
func newProjectWithProviderAndBackend(t *testing.T, name string) string {
	t.Helper()
	return projectDir(t, fmt.Sprintf(`
project: myapp
providers:
  - plugin: %s
backend:
  plugin: %s
  bucket: acme-state
`, name, name))
}

// installedBinaryExists reports whether one named binary reached the project's
// plugin directory, which is where the loader was already looking.
func installedBinaryExists(t *testing.T, dir, binary string) bool {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, ".infrena", "plugins", binary))
	return err == nil && !info.IsDir()
}

// A fresh clone needs every plugin the project names. One command, and it is
// unambiguous by construction, because the project says both the names and
// the kinds.
func TestBareInstallInstallsEverythingTheProjectDeclares(t *testing.T) {
	dir := newProjectNeeding(t, []string{"aws", "fake"}, "s3")
	trustSources(t)
	srv := fakeGitHubServingAll(t)
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	stdout, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install")

	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	for _, bin := range []string{"infrena-plugin-aws", "infrena-plugin-fake", "infrena-backend-s3"} {
		if !installedBinaryExists(t, dir, bin) {
			t.Errorf("%s was not installed:\n%s", bin, stdout)
		}
	}
}

// Already installed is not an error and is not a reinstall: the point is to
// reach a working state, and a second run must be safe.
func TestBareInstallSkipsWhatIsAlreadyInstalled(t *testing.T) {
	dir := newProjectNeeding(t, []string{"aws"}, "")
	trustSources(t)
	srv := fakeGitHubServingAll(t)
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	if _, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install"); code != ExitOK {
		t.Fatalf("first run: exit = %d\n%s", code, stderr)
	}
	stdout, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install")

	if code != ExitOK {
		t.Fatalf("second run exit = %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "already installed") {
		t.Errorf("second run does not say it skipped:\n%s", stdout)
	}
}

// A project declaring nothing is not a failure. It is a project that needs
// no plugins, which is a real state - `init` produces one.
//
// NOT newProjectFixture, which declares a `fake.network` resource: a resource
// type's prefix IS a plugin the project needs, so that fixture needs one.
func TestBareInstallOnAProjectNeedingNothingSaysSo(t *testing.T) {
	dir := newProjectNeeding(t, nil, "")
	trustSources(t)

	stdout, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install")

	if code != ExitOK {
		t.Errorf("exit = %d, want %d\n%s", code, ExitOK, stderr)
	}
	if !strings.Contains(stdout, "nothing") && !strings.Contains(stdout, "No plugins") {
		t.Errorf("output does not say there was nothing to do:\n%s", stdout)
	}
}

// With no project there is nothing to enumerate, so the error says that
// rather than reporting an empty list as success.
func TestBareInstallWithNoProjectIsAnError(t *testing.T) {
	dir := t.TempDir()
	trustSources(t)

	_, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "--global")

	if code == ExitOK {
		t.Fatal("bare install succeeded with no project to read")
	}
	if !strings.Contains(stderr, "infrena.yml") {
		t.Errorf("error does not name what is missing:\n%s", stderr)
	}
}

// One failure must not silently lose the others. Report what happened to
// each and fail overall - a partial result a reader mistakes for a complete
// one is the failure this project avoids everywhere.
func TestBareInstallReportsEveryPluginEvenWhenOneFails(t *testing.T) {
	dir := newProjectNeeding(t, []string{"aws", "nonesuch"}, "")
	trustSources(t)
	srv := fakeGitHubServingAll(t)
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	stdout, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install")

	if code == ExitOK {
		t.Fatal("a failed install reported success")
	}
	if !installedBinaryExists(t, dir, "infrena-plugin-aws") {
		t.Error("the plugin that could be installed was not")
	}
	if !strings.Contains(stdout+stderr, "nonesuch") {
		t.Errorf("the failure is not named:\n%s\n%s", stdout, stderr)
	}
}

// newProjectNeeding is a project that NAMES the plugins it needs and declares
// nothing else. No resources: a resource type's prefix is a plugin needed too,
// and these fixtures are about the list being exactly what was asked for.
func newProjectNeeding(t *testing.T, providers []string, backend string) string {
	t.Helper()
	body := "project: myapp\n"
	if len(providers) > 0 {
		body += "providers:\n"
		for _, p := range providers {
			body += "  - plugin: " + p + "\n"
		}
	}
	if backend != "" {
		body += "backend:\n  plugin: " + backend + "\n  bucket: acme-state\n"
	}
	return projectDir(t, body)
}

// newProjectNamingSource is a project that NAMES a source for a plugin, which
// is the case the trust rule exists for: the file is committed and travels with
// a `git clone`.
func newProjectNamingSource(t *testing.T, name, source string) string {
	t.Helper()
	return projectDir(t, fmt.Sprintf(`
project: myapp
plugins:
  %s:
    source: %s
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
`, name, source))
}

// installedBinaries counts what actually reached the project's plugin
// directory, which is the assertion every refusal above ends with.
func installedBinaries(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, ".infrena", "plugins"))
	if err != nil {
		// No directory at all is the strongest form of "nothing was written".
		return 0
	}
	return len(entries)
}

// trustedFileContains reports whether the user's own trusted-sources file
// mentions something. Every refusal asserts it does not: an approval recorded
// for an install that did not happen would make the next run succeed for a
// reason nobody consented to.
func trustedFileContains(t *testing.T, want string) bool {
	t.Helper()
	home, err := os.UserConfigDir()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(plugins.TrustedPath(home))
	if err != nil {
		return false
	}
	return strings.Contains(string(data), want)
}

// fakeGitHubServing is one owner publishing one plugin, with a real release
// behind it: an archive for this machine and the SHA256SUMS that vouches for
// it.
func fakeGitHubServing(t *testing.T, owner, name, version string) *httptest.Server {
	t.Helper()
	return fakeForge(t, installableForge(t, owner, name, version, archiveFor(t, name, version), true))
}

// fakeGitHubServingTamperedArchive publishes a correct SHA256SUMS and a
// DIFFERENT archive, which is exactly what a substituted download looks like
// from here.
func fakeGitHubServingTamperedArchive(t *testing.T, owner, name, version string) *httptest.Server {
	t.Helper()
	f := installableForge(t, owner, name, version, archiveFor(t, name, version), true)
	release := fmt.Sprintf("%s/%s%s@v%s", owner, plugins.RepoPrefix, name, version)
	asset := realAssetName(providerBinary(name), version)
	// The checksums file already covers the honest archive; only the bytes are
	// swapped, so the mismatch is discovered by verification and nowhere else.
	f.releases[release][asset] = tarGz(t, map[string]string{
		fmt.Sprintf("%s-%s/%s", name, version, pluginhost.BinaryName(name)): "not what was published",
	})
	return fakeForge(t, f)
}

// fakeGitHubServingOtherPlatformOnly publishes a release whose SHA256SUMS
// covers some other machine's archive and not this one's.
func fakeGitHubServingOtherPlatformOnly(t *testing.T, owner, name, version string) *httptest.Server {
	t.Helper()
	f := installableForge(t, owner, name, version, archiveFor(t, name, version), true)
	release := fmt.Sprintf("%s/%s%s@v%s", owner, plugins.RepoPrefix, name, version)
	delete(f.releases[release], realAssetName(providerBinary(name), version))
	f.releases[release][remote.ChecksumsName] = []byte(
		"0000000000000000000000000000000000000000000000000000000000000000  ./" +
			name + "_" + version + "_elsewhere_amd64.tar.gz\n")
	return fakeForge(t, f)
}

// fakeGitHubServingTwoOwners is the case infrena refuses to resolve: one name,
// two publishers, both of them installable.
func fakeGitHubServingTwoOwners(t *testing.T, name, version string) *httptest.Server {
	t.Helper()
	a := installableForge(t, "infrena", name, version, archiveFor(t, name, version), true)
	b := installableForge(t, "mycorp", name, version, archiveFor(t, name, version), false)
	return fakeForge(t, mergeForges(a, b))
}

// fakeGitHubServingBothKinds is the collision these tests exist for: ONE owner
// publishing a provider and a backend of one name, both released. The two live
// in different repositories, so nothing about it is ambiguous to the forge -
// only to a user typing a bare name.
func fakeGitHubServingBothKinds(t *testing.T, name string) *httptest.Server {
	t.Helper()
	p := installableRepoForge(t, "infrena", plugins.ProviderRepoPrefix+name, providerBinary(name), name, "2.0.0",
		archiveOf(t, providerBinary(name), "2.0.0"), true)
	b := installableRepoForge(t, "infrena", plugins.BackendRepoPrefix+name, backendBinary(name), name, "1.0.0",
		archiveOf(t, backendBinary(name), "1.0.0"), true)
	return fakeForge(t, mergeForges(p, b))
}

// mergeForges folds one fake forge into another, so a test can publish more
// than one owner or more than one repository without the handler knowing.
func mergeForges(a, b forge) forge {
	for owner, repos := range b.repos {
		a.repos[owner] = append(a.repos[owner], repos...)
	}
	for owner, isOrg := range b.orgs {
		a.orgs[owner] = isOrg
	}
	for k, v := range b.tags {
		a.tags[k] = v
	}
	for k, v := range b.files {
		a.files[k] = v
	}
	for k, v := range b.releases {
		a.releases[k] = v
	}
	return a
}

// installableForge is one owner, one provider repository, one release, and the
// manifest that says the release can run here.
func installableForge(t *testing.T, owner, name, version string, archive []byte, isOrg bool) forge {
	t.Helper()
	return installableRepoForge(t, owner, plugins.ProviderRepoPrefix+name, providerBinary(name), name, version, archive, isOrg)
}

// installableRepoForge is installableForge with the repository and the binary
// it ships named explicitly, which is what lets a backend repository be
// published the same way. The REPOSITORY NAME carries the role and the BINARY
// NAME follows from it; everything else here is identical, because a release is
// a release.
func installableRepoForge(t *testing.T, owner, repo, binary, name, version string, archive []byte, isOrg bool) forge {
	t.Helper()
	tag := "v" + version
	here := fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)
	asset := realAssetName(binary, version)

	f := forge{
		orgs:     map[string]bool{},
		repos:    map[string][]string{owner: {repo}},
		tags:     map[string]string{owner + "/" + repo: tag},
		files:    map[string]string{owner + "/" + repo + "@" + tag: manifestYAML(name, version, here)},
		releases: map[string]map[string][]byte{},
	}
	if isOrg {
		f.orgs[owner] = true
	}

	sum := sha256.Sum256(archive)
	f.releases[owner+"/"+repo+"@"+tag] = map[string][]byte{
		asset: archive,
		// THE `./` PREFIX IS WHAT A REAL RELEASE WRITES: release.yml runs
		// `sha256sum ./*`, so a parser keying on the raw field would find a
		// checksum for no asset at all. Serving the honest form is what keeps
		// that a tested property rather than a remembered one.
		remote.ChecksumsName: []byte(hex.EncodeToString(sum[:]) + "  ./" + asset + "\n"),
	}
	return f
}

// archiveFor builds the archive a release actually ships: the binary nested
// inside the versioned directory, beside a README, with the directory itself an
// entry. An archive of one top-level file would pass an extractor that no real
// release could satisfy.
func archiveFor(t *testing.T, name, version string) []byte {
	t.Helper()
	return archiveOf(t, providerBinary(name), version)
}

// archiveOf is archiveFor for whichever binary the release ships, so a backend
// release can be built the same way a provider one is. The directory inside is
// named after the BINARY, which is what both release scripts do:
// infrena-backend-s3_1.0.0_linux_amd64/infrena-backend-s3.
func archiveOf(t *testing.T, binary, version string) []byte {
	t.Helper()
	dir := fmt.Sprintf("%s_%s_%s_%s", binary, version, runtime.GOOS, runtime.GOARCH)
	file := binary
	if runtime.GOOS == "windows" {
		file += ".exe"
	}
	return tarGz(t, map[string]string{
		dir + "/README.md": "docs",
		dir + "/" + file:   "#!/bin/sh\nexit 0\n",
	})
}

// providerBinary and backendBinary are the executables the two kinds of release
// ship, and realAssetName is the archive that carries one.
//
// WRITTEN OUT HERE RATHER THAN ASKED OF THE CODE UNDER TEST. remote.AssetName
// is precisely what these tests check, so a fake that named its assets by
// calling it would serve whatever the code asked for and pass whatever the code
// did - which is how a hardcoded infrena-plugin- prefix went on constructing a
// backend archive no release publishes.
//
// These names follow the release scripts: .github/workflows/release.yml for a
// provider, and a backend repository's own scripts/build-release, which names
// its archives infrena-backend-<name>_<version>_<goos>_<goarch> and puts the
// binary inside a directory of that same name. Windows is a .zip in both.
func providerBinary(name string) string { return "infrena-plugin-" + name }

func backendBinary(name string) string { return "infrena-backend-" + name }

func realAssetName(binary, version string) string {
	ext := ".tar.gz"
	if runtime.GOOS == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("%s_%s_%s_%s%s", binary, version, runtime.GOOS, runtime.GOARCH, ext)
}

// backendForge is one owner publishing one backend repository, released the
// same way a provider is.
func backendForge(t *testing.T, owner, name, version string) forge {
	t.Helper()
	return installableRepoForge(t, owner, plugins.BackendRepoPrefix+name, backendBinary(name), name, version,
		archiveOf(t, backendBinary(name), version), true)
}

// fakeGitHubServingOneBackend is a name only ONE thing answers to, and that one
// thing is a backend. There is nothing to disambiguate, so nothing may be asked.
func fakeGitHubServingOneBackend(t *testing.T, name string) *httptest.Server {
	t.Helper()
	return fakeForge(t, backendForge(t, "infrena", name, "1.0.0"))
}

// fakeGitHubServingAll is the official owner publishing everything the bare
// install fixtures name: two providers and one backend.
func fakeGitHubServingAll(t *testing.T) *httptest.Server {
	t.Helper()
	f := installableForge(t, "infrena", "aws", "0.4.0", archiveFor(t, "aws", "0.4.0"), true)
	f = mergeForges(f, installableForge(t, "infrena", "fake", "0.4.0", archiveFor(t, "fake", "0.4.0"), true))
	f = mergeForges(f, backendForge(t, "infrena", "s3", "1.0.0"))
	return fakeForge(t, f)
}

// tarGz builds a gzipped tarball, writing an entry for every directory the
// names imply, as `tar -czf` of a directory does.
func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)

	seen := map[string]bool{}
	for _, name := range sortedFileNames(files) {
		if dir := filepath.ToSlash(filepath.Dir(name)); dir != "." && !seen[dir] {
			seen[dir] = true
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir, Name: dir + "/", Mode: 0o755, Format: tar.FormatPAX,
			}); err != nil {
				t.Fatal(err)
			}
		}
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg, Name: name, Mode: 0o755,
			Size: int64(len(body)), Format: tar.FormatPAX,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// sortedFileNames keeps the order entries are written in stable, so a failing
// archive is the same archive on the next run.
func sortedFileNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
