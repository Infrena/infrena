# Plugin Install, Lock and Verify — Unit 3 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `infrena plugins install <name>` downloads a plugin the user has approved, proves it is the file its publisher released, records that in a committed lock file, and refuses everything else.

**Architecture:** Install is the first thing in infrena that writes an executable to disk, so every step is a refusal rather than a sanitisation: checksums are verified before an archive is opened, exactly one file is extracted, a path that escapes the destination fails the whole archive, and the binary is moved into place only after it has been proved. Trust is enforced here for the first time — a project may NAME a source but only the user may TRUST one, and confirming records the owner in the user's own config. `plugins.lock` is committed and is what makes a binary still trustworthy tomorrow.

**Tech Stack:** Go 1.27, Cobra, `gopkg.in/yaml.v3`, and `net/http`, `archive/tar`, `compress/gzip`, `crypto/sha256` from the standard library. **No archive or download helper may be added.**

**Spec:** `PLAN.md` §31.3, including the "Extraction is a security boundary" subsection added 2026-09-16.

## Global Constraints

- Go 1.27.0 module floor. `mise` is not active in non-interactive shells.
- **Third-party budget is two packages**: cobra, yaml.v3. Everything here is standard library.
- **THE NETWORK IS NEVER ON THE HOT PATH.** All HTTP stays in `internal/plugins/remote`; `internal/plugins`' `TestPluginsPackageCannotReachTheNetwork` must keep passing. `plugins.lock` reading and verification are OFFLINE and belong in `internal/plugins`.
- **A project may NAME a source; only a user may TRUST one.** Project configuration travels with a `git clone`, so it must never grant a download source.
- **Non-interactive never approves.** With no TTY, an unapproved owner is an error naming the owner and how to approve it. A pipeline that silently starts trusting a new binary publisher is the failure this whole subsection exists to prevent.
- **Never auto-pick** between two sources answering one name.
- **Never claim a plugin does not exist when it merely could not be seen** — §31.3, generalised 2026-09-16 after five bugs in Unit 2 all turned out to be this. A rate limit, an unauthenticated listing of private repositories, and a cached negative from a different credential are three doors into the same mistake.
- **Checksums come from the release's `SHA256SUMS`, never the manifest.** They postdate the build; §31.2 says why the manifest has none.
- **Verify before extracting.** A tampered archive is refused without being opened.
- Errors follow §44. `gofmt -l .` and `go vet ./...` clean; full `go test -count=1 ./...` green with `INFRENA_REQUIRE_PLUGIN=1`.
- Commit messages: plain English, no em-dashes, minimal, **never** mention Claude, AI or the model. Each ends with `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.

**A LESSON FROM UNIT 2, WHICH THIS PLAN REQUIRES YOU TO ACT ON.** Unit 2 shipped five bugs with a fully green suite, because every test used `httptest` fixtures that answered whatever the client asked — so they would have passed against any endpoint, right or wrong. The bugs were found by running the binary once against the real API. **Task 8 of this plan is a mandatory hand-verification against the real GitHub, and no task may be reported complete on a green suite alone.**

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/plugins/lockfile.go` *(new)* | `plugins.lock`: shape, read, write, compare. Offline. |
| `internal/plugins/lockfile_test.go` *(new)* | Round trip, version, a changed checksum. |
| `internal/plugins/remote/download.go` *(new)* | Fetching a release asset and its `SHA256SUMS`. |
| `internal/plugins/extract.go` *(new)* | The security boundary: one file out, everything else refused. Offline, so it is testable with a hand-built tarball. |
| `internal/plugins/extract_test.go` *(new)* | Traversal, symlinks, bombs, extra entries. |
| `internal/plugins/trust.go` *(new)* | Approving an owner and recording it. |
| `internal/cli/plugins_install.go` *(new)* | `install`, `verify`, and the approval prompt. |
| `internal/pluginhost/loader.go` | Verify against the lock on launch. |

---

## Task 1: `plugins.lock`

**Files:**
- Create: `internal/plugins/lockfile.go`, `internal/plugins/lockfile_test.go`

Follow the shape of `internal/modules/source/lockfile.go`, which is this project's existing committed-lockfile precedent — read it first.

**Interfaces:**
- Produces:
  - `type LockEntry struct { Version, Source string; Checksums map[string]string }`
  - `type Lockfile struct { Version int; Plugins map[string]LockEntry }`
  - `const LockVersion = 1`, `const LockfileName = "plugins.lock"`
  - `func ReadLockfile(dir string) (*Lockfile, error)` — missing file is `nil, nil`
  - `func (*Lockfile) Write(dir string) error`
  - `func (*Lockfile) Check(name, platform, sum string) error`

- [ ] **Step 1: Write the failing test**

```go
func TestLockfileRoundTrips(t *testing.T) {
	dir := t.TempDir()
	l := &Lockfile{Version: LockVersion, Plugins: map[string]LockEntry{
		"aws": {
			Version: "0.4.0",
			Source:  "github.com/infrena/infrena-provider-aws",
			Checksums: map[string]string{
				"linux/amd64":  "sha256:aaaa",
				"darwin/arm64": "sha256:bbbb",
			},
		},
	}}
	if err := l.Write(dir); err != nil {
		t.Fatal(err)
	}

	got, err := ReadLockfile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, l) {
		t.Errorf("round trip changed the lockfile:\n got %+v\nwant %+v", got, l)
	}
}

// It is COMMITTED and read in a diff, so a rewrite that reorders keys is a
// diff nobody can review. Go map order is randomised, so this is a real risk.
func TestWritingTwiceProducesIdenticalBytes(t *testing.T) {
	dir := t.TempDir()
	l := &Lockfile{Version: LockVersion, Plugins: map[string]LockEntry{
		"zeta": {Version: "1.0.0", Source: "github.com/a/infrena-provider-zeta", Checksums: map[string]string{"linux/amd64": "sha256:1", "darwin/arm64": "sha256:2"}},
		"alpha": {Version: "1.0.0", Source: "github.com/a/infrena-provider-alpha", Checksums: map[string]string{"linux/arm64": "sha256:3", "linux/amd64": "sha256:4"}},
	}}
	for i := 0; i < 20; i++ {
		if err := l.Write(dir); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dir, LockfileName))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			t.Setenv("first", string(b))
			continue
		}
		if string(b) != os.Getenv("first") {
			t.Fatalf("write %d differs from the first", i)
		}
	}
}

// A missing lockfile is the ordinary case for a project that has not installed
// anything, and must not be an error.
func TestAMissingLockfileIsNotAnError(t *testing.T) {
	got, err := ReadLockfile(t.TempDir())
	if err != nil {
		t.Fatalf("ReadLockfile with no file: %v", err)
	}
	if got != nil {
		t.Errorf("ReadLockfile = %+v, want nil", got)
	}
}

// The whole point of the file: a binary swapped on disk after install is
// caught at the next command rather than never.
func TestCheckRefusesAChangedChecksumAndNamesThePlugin(t *testing.T) {
	l := &Lockfile{Version: LockVersion, Plugins: map[string]LockEntry{
		"aws": {Version: "0.4.0", Source: "github.com/infrena/infrena-provider-aws",
			Checksums: map[string]string{"linux/amd64": "sha256:aaaa"}},
	}}

	if err := l.Check("aws", "linux/amd64", "sha256:aaaa"); err != nil {
		t.Errorf("a matching checksum was refused: %v", err)
	}
	err := l.Check("aws", "linux/amd64", "sha256:dddd")
	if err == nil {
		t.Fatal("a changed checksum was accepted")
	}
	if !strings.Contains(err.Error(), "aws") {
		t.Errorf("error does not name the plugin: %v", err)
	}
}

// A platform the lock does not record is NOT a silent pass. The lock is what
// makes a binary trustworthy, and "no entry for your machine" must be visible
// rather than read as approval.
func TestCheckRefusesAPlatformTheLockDoesNotRecord(t *testing.T) {
	l := &Lockfile{Version: LockVersion, Plugins: map[string]LockEntry{
		"aws": {Version: "0.4.0", Checksums: map[string]string{"linux/amd64": "sha256:aaaa"}},
	}}
	if err := l.Check("aws", "darwin/arm64", "sha256:aaaa"); err == nil {
		t.Error("a platform with no recorded checksum was accepted")
	}
}

// A plugin with no entry at all is NOT an error: a hand-placed binary keeps
// working (section 31.3), and the lock governs what install put there.
func TestCheckIgnoresAPluginTheLockDoesNotMention(t *testing.T) {
	l := &Lockfile{Version: LockVersion, Plugins: map[string]LockEntry{}}
	if err := l.Check("handplaced", "linux/amd64", "sha256:whatever"); err != nil {
		t.Errorf("a plugin absent from the lock was refused: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestLockfile|TestWritingTwice|TestAMissingLockfile|TestCheck' ./internal/plugins/ -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Write minimal implementation**

JSON like `modules.lock`, with sorted keys and `MarshalIndent` so the bytes are stable. `Version` is never omitted; a future version it does not understand is an error naming the file and the two versions.

Document the two asymmetric cases, because they look inconsistent and are not:

```go
// Check reports whether a binary is the one the lock recorded.
//
// TWO ABSENCES, TWO ANSWERS, and the difference is the whole design. A plugin
// the lock does not mention passes: a hand-placed binary keeps working (§31.3),
// and the lock governs what INSTALL put on disk rather than claiming authority
// over everything. But a plugin the lock DOES mention, on a platform it does
// not record, FAILS: something installed this and recorded a checksum for
// another machine, so "no entry for yours" is a gap to see rather than
// permission to run whatever is there.
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/plugins/ -v 2>&1 | tail -5`
Expected: PASS, network test included.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/plugins/...
git add internal/plugins/
git commit -m "Record what install put on disk

A committed lock naming the resolved version, where it came from, and a
checksum per platform, so a binary replaced afterwards is caught at the
next command rather than never."
```

---

## Task 2: Extraction is a security boundary

**Files:**
- Create: `internal/plugins/extract.go`, `internal/plugins/extract_test.go`

Offline, so it is fully testable against tarballs built in the test. No network.

**Interfaces:**
- Produces: `func ExtractBinary(r io.Reader, wantName string, dest string, maxBytes int64) error`

- [ ] **Step 1: Write the failing test**

```go
func TestExtractsExactlyTheNamedBinary(t *testing.T) {
	tarball := buildTar(t, []tarEntry{
		{name: "README.md", body: "docs"},
		{name: "infrena-plugin-aws", body: "ELF", mode: 0o755},
		{name: "LICENSE", body: "text"},
	})
	dest := filepath.Join(t.TempDir(), "infrena-plugin-aws")

	if err := ExtractBinary(bytes.NewReader(tarball), "infrena-plugin-aws", dest, 1<<20); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "ELF" {
		t.Fatalf("binary not extracted: %q %v", got, err)
	}
	// Everything else is ignored, not written.
	entries, _ := os.ReadDir(filepath.Dir(dest))
	if len(entries) != 1 {
		t.Errorf("extracted %d files, want only the binary", len(entries))
	}
	fi, _ := os.Stat(dest)
	if fi.Mode().Perm()&0o111 == 0 {
		t.Error("extracted binary is not executable")
	}
}

// THE ESCAPE. An entry whose path leaves the destination fails the whole
// archive - not sanitised, not skipped. An extractor that quietly drops this
// will happily extract whatever came next.
func TestATraversingEntryFailsTheWholeArchive(t *testing.T) {
	for _, name := range []string{
		"../../../.ssh/authorized_keys",
		"/etc/cron.d/evil",
		"sub/../../escape",
	} {
		t.Run(name, func(t *testing.T) {
			tarball := buildTar(t, []tarEntry{
				{name: name, body: "pwned"},
				{name: "infrena-plugin-aws", body: "ELF", mode: 0o755},
			})
			dir := t.TempDir()

			err := ExtractBinary(bytes.NewReader(tarball), "infrena-plugin-aws", filepath.Join(dir, "infrena-plugin-aws"), 1<<20)
			if err == nil {
				t.Fatal("an escaping entry was accepted")
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error does not name the entry: %v", err)
			}
			// And nothing was written, including the legitimate binary.
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("wrote %d files despite refusing", len(entries))
			}
		})
	}
}

// A symlink is the same escape wearing different clothes.
func TestASymlinkEntryIsRefused(t *testing.T) {
	tarball := buildTarWithLink(t, "infrena-plugin-aws", "/etc/passwd")
	dir := t.TempDir()

	if err := ExtractBinary(bytes.NewReader(tarball), "infrena-plugin-aws", filepath.Join(dir, "infrena-plugin-aws"), 1<<20); err == nil {
		t.Fatal("a symlink was accepted")
	}
}

// A few hundred kilobytes can expand to gigabytes. An install that fills the
// disk is a denial of service that survives a reboot.
func TestExtractionStopsAtTheByteCap(t *testing.T) {
	tarball := buildTar(t, []tarEntry{
		{name: "infrena-plugin-aws", body: strings.Repeat("A", 10<<20), mode: 0o755},
	})
	dir := t.TempDir()

	err := ExtractBinary(bytes.NewReader(tarball), "infrena-plugin-aws", filepath.Join(dir, "infrena-plugin-aws"), 1<<20)
	if err == nil {
		t.Fatal("an oversized entry was accepted")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left %d files behind after refusing", len(entries))
	}
}

// The archive not containing what the manifest promised is an error naming
// both, not a silent success with nothing written.
func TestAnArchiveWithoutTheNamedBinaryIsAnError(t *testing.T) {
	tarball := buildTar(t, []tarEntry{{name: "something-else", body: "x", mode: 0o755}})
	dir := t.TempDir()

	err := ExtractBinary(bytes.NewReader(tarball), "infrena-plugin-aws", filepath.Join(dir, "infrena-plugin-aws"), 1<<20)
	if err == nil {
		t.Fatal("an archive missing the binary was accepted")
	}
	if !strings.Contains(err.Error(), "infrena-plugin-aws") {
		t.Errorf("error does not name what was expected: %v", err)
	}
}
```

Write `buildTar` and `buildTarWithLink` with `archive/tar` and `compress/gzip`. Releases ship `.tar.gz` — confirmed against `infrena_0.7.1_linux_amd64.tar.gz`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestExtract|TestATraversing|TestASymlink|TestAnArchiveWithout' ./internal/plugins/ -v`
Expected: FAIL — `ExtractBinary` undefined.

- [ ] **Step 3: Write minimal implementation**

Walk entries. For each: refuse anything that is not `tar.TypeReg`; refuse a name that is absolute or whose cleaned path escapes; skip regular files that are not `wantName`; copy `wantName` through an `io.LimitedReader` at `maxBytes+1` and refuse if it exceeds. **Write to a temporary file in the destination directory and rename only on success**, so nothing runnable is left behind by a refusal. Missing binary at the end is an error naming it.

Refuse the whole archive on the first bad entry — do not continue looking for the binary.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/plugins/ -v 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/plugins/...
git add internal/plugins/
git commit -m "Take one binary out of an archive and refuse the rest

An archive is data someone else controls. An entry whose path leaves the
destination fails the whole thing rather than being tidied up, symlinks
and oversized entries are refused, and nothing is left on disk when an
archive is rejected."
```

---

## Task 3: Downloading a release and its checksums

**Files:**
- Create: `internal/plugins/remote/download.go`, `internal/plugins/remote/download_test.go`

**Interfaces:**
- Produces:
  - `(*Client) ReleaseAsset(ctx, owner, repo, tag, asset string) ([]byte, error)`
  - `(*Client) Checksums(ctx, owner, repo, tag string) (map[string]string, error)` — parses `SHA256SUMS`
  - `func AssetName(plugin, version string, p pluginmanifest.Platform) string`

- [ ] **Step 1: Write the failing test**

```go
// The convention mirrors infrena's own releases, verified against
// infrena_0.7.1_linux_amd64.tar.gz and
// infrena-plugin-aws_0.4.0_linux_amd64.tar.gz.
func TestAssetNameFollowsTheReleaseConvention(t *testing.T) {
	got := AssetName("aws", "0.4.0", pluginmanifest.Platform{OS: "linux", Arch: "amd64"})
	if got != "infrena-plugin-aws_0.4.0_linux_amd64.tar.gz" {
		t.Errorf("AssetName = %q", got)
	}
}

func TestChecksumsParsesSHA256SUMS(t *testing.T) {
	body := "aaaa  infrena-plugin-aws_0.4.0_linux_amd64.tar.gz\nbbbb  infrena-plugin-aws_0.4.0_darwin_arm64.tar.gz\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL, RawURL: srv.URL}

	got, err := c.Checksums(context.Background(), "infrena", "infrena-provider-aws", "v0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if got["infrena-plugin-aws_0.4.0_linux_amd64.tar.gz"] != "aaaa" {
		t.Errorf("Checksums = %v", got)
	}
}

// A release with no SHA256SUMS cannot be installed, and must say that rather
// than install something unverified.
func TestAReleaseWithNoChecksumsIsRefusedNotInstalled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL, RawURL: srv.URL}

	_, err := c.Checksums(context.Background(), "infrena", "infrena-provider-aws", "v0.4.0")
	if err == nil {
		t.Fatal("a release with no SHA256SUMS was accepted")
	}
	if !strings.Contains(err.Error(), "SHA256SUMS") {
		t.Errorf("error does not name what is missing: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/plugins/remote/ -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Write minimal implementation**

Reuse the existing `classify` so a rate limit and a missing asset stay distinguishable here too — the §31.3 rule applies on this path as much as on search. Cap the asset body with an `io.LimitedReader`; an unbounded download is the same hazard as an unbounded extraction.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/plugins/... -v 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/plugins/...
git add internal/plugins/remote/
git commit -m "Fetch a release archive and the checksums beside it

A release without a checksums file cannot be installed and says so, rather
than installing something nothing can vouch for."
```

---

## Task 4: Approving an owner

**Files:**
- Create: `internal/plugins/trust.go`, `internal/plugins/trust_test.go`

**Interfaces:**
- Produces:
  - `func IsTrusted(trusted []Source, s Source) bool` — an owner source trusts every repository under it
  - `func Approve(configHome string, s Source) error` — appends to the user's own file, creating it

- [ ] **Step 1: Write the failing test**

```go
// Trusting an owner trusts the repositories under it: the approval is "I
// trust this publisher", which is the unit a person can actually reason
// about. One mechanism, asked once.
func TestTrustingAnOwnerTrustsItsRepositories(t *testing.T) {
	trusted := []Source{mustSource(t, "github.com/mycorp")}

	if !IsTrusted(trusted, mustSource(t, "github.com/mycorp/infrena-provider-hetzner")) {
		t.Error("a repository under a trusted owner was not trusted")
	}
	if IsTrusted(trusted, mustSource(t, "github.com/someone/infrena-provider-hetzner")) {
		t.Error("a repository under an untrusted owner was trusted")
	}
}

// Trusting ONE repository does not trust its owner. The narrower approval must
// not widen by itself.
func TestTrustingOneRepositoryDoesNotTrustTheOwner(t *testing.T) {
	trusted := []Source{mustSource(t, "github.com/someone/infrena-provider-hetzner")}

	if IsTrusted(trusted, mustSource(t, "github.com/someone/infrena-provider-other")) {
		t.Error("trusting one repository trusted a sibling")
	}
	if IsTrusted(trusted, mustSource(t, "github.com/someone")) {
		t.Error("trusting one repository trusted the whole owner")
	}
}

// Approving records it in the USER's own file, so it is asked once and is
// visible afterwards.
func TestApproveRecordsTheOwnerAndSurvivesAReload(t *testing.T) {
	home := t.TempDir()

	if err := Approve(home, mustSource(t, "github.com/mycorp")); err != nil {
		t.Fatal(err)
	}

	got, err := LoadTrusted(home)
	if err != nil {
		t.Fatal(err)
	}
	if !IsTrusted(got, mustSource(t, "github.com/mycorp/infrena-provider-hetzner")) {
		t.Error("an approved owner was not trusted after reloading")
	}
}

// Approving twice must not write it twice.
func TestApproveIsIdempotent(t *testing.T) {
	home := t.TempDir()
	s := mustSource(t, "github.com/mycorp")
	for i := 0; i < 3; i++ {
		if err := Approve(home, s); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := LoadTrusted(home)
	n := 0
	for _, x := range got {
		if x.String() == "github.com/mycorp" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("approved owner appears %d times, want 1", n)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestTrusting|TestApprove' ./internal/plugins/ -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Write minimal implementation**

`Approve` reads the existing file (or starts empty), appends if absent, and writes atomically. It must not lose a comment a user wrote if that is cheap to preserve; if it is not, say so in a comment rather than silently discarding.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/plugins/ -v 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/plugins/...
git add internal/plugins/
git commit -m "Approve an owner once and remember it

Trusting a publisher is the unit a person can reason about, so an owner
covers the repositories under it, while trusting one repository stays
narrow and does not widen to its owner."
```

---

## Task 5: `infrena plugins install`

**Files:**
- Create: `internal/cli/plugins_install.go`, `internal/cli/plugins_install_test.go`

**Interfaces:**
- Consumes: everything above, plus Unit 2's `Search`.
- Produces: `install <name>[@version]` with `--global`, and `verify`.

- [ ] **Step 1: Write the failing test**

```go
// THE SECURITY TEST. A project may NAME a source, but naming it must not be
// enough to download an executable from it. Otherwise `git clone && infrena
// plan` lets a repository introduce a place infrena fetches binaries from.
func TestInstallFromAnUntrustedOwnerRefusesWithoutApproval(t *testing.T) {
	dir := newProjectNamingSource(t, "hetzner", "github.com/someone/infrena-provider-hetzner")
	srv := fakeGitHubServing(t, "hetzner", "1.0.0")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	// Empty stdin: no TTY, so nothing can be approved.
	_, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "hetzner")

	if code == ExitOK {
		t.Fatal("installed from an owner the user never approved")
	}
	if !strings.Contains(stderr, "someone") {
		t.Errorf("refusal does not name the owner to approve:\n%s", stderr)
	}
	if installedBinaries(t, dir) != 0 {
		t.Error("a binary was written despite refusing")
	}
}

// Non-interactive NEVER approves. A pipeline that silently starts trusting a
// new binary publisher is the failure this subsection exists to prevent.
func TestNonInteractiveNeverApproves(t *testing.T) {
	dir := newProjectNamingSource(t, "hetzner", "github.com/someone/infrena-provider-hetzner")
	srv := fakeGitHubServing(t, "hetzner", "1.0.0")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	_, stderr, code := runCommandWithStdin(t, dir, "yes\n", "plugins", "install", "hetzner", "--output",
		filepath.Join(t.TempDir(), "r.ndjson"))

	if code == ExitOK {
		t.Fatal("machine mode approved an owner")
	}
	if !strings.Contains(stderr, "plugins") {
		t.Errorf("refusal does not say how to approve:\n%s", stderr)
	}
}

// The official owner is trusted from the start, so this path needs no prompt.
func TestInstallFromTheOfficialOwnerNeedsNoApproval(t *testing.T) {
	dir := newProjectFixture(t)
	srv := fakeGitHubServing(t, "aws", "0.4.0")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	_, _, code := runCommandWithStdin(t, dir, "", "plugins", "install", "aws")

	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if installedBinaries(t, dir) != 1 {
		t.Error("nothing was installed")
	}
}

// The checksum is the point of the whole exercise.
func TestAnArchiveThatFailsItsChecksumIsRefused(t *testing.T) {
	dir := newProjectFixture(t)
	srv := fakeGitHubServingTamperedArchive(t, "aws", "0.4.0")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	_, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "aws")

	if code == ExitOK {
		t.Fatal("an archive failing its checksum was installed")
	}
	if !strings.Contains(stderr, "checksum") {
		t.Errorf("refusal does not say why:\n%s", stderr)
	}
	if installedBinaries(t, dir) != 0 {
		t.Error("a binary was written despite a bad checksum")
	}
}

func TestInstallWritesTheLockfile(t *testing.T) {
	dir := newProjectFixture(t)
	srv := fakeGitHubServing(t, "aws", "0.4.0")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	runCommandWithStdin(t, dir, "", "plugins", "install", "aws")

	l, err := plugins.ReadLockfile(dir)
	if err != nil || l == nil {
		t.Fatalf("no lockfile written: %v", err)
	}
	e, ok := l.Plugins["aws"]
	if !ok || e.Version != "0.4.0" || len(e.Checksums) == 0 {
		t.Errorf("lock entry = %+v", e)
	}
}

// Two owners answering one name: present both, install neither.
func TestInstallRefusesToChooseBetweenTwoSources(t *testing.T) {
	dir := newProjectFixture(t)
	srv := fakeGitHubServingTwoOwners(t, "hetzner")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)
	approveOwner(t, "github.com/mycorp")

	_, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "hetzner")

	if code == ExitOK {
		t.Fatal("install chose between two sources")
	}
	if !strings.Contains(stderr, "does not choose") {
		t.Errorf("refusal does not explain:\n%s", stderr)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestInstall|TestNonInteractive|TestAnArchiveThat' ./internal/cli/ -v`
Expected: FAIL — unknown subcommand.

- [ ] **Step 3: Write minimal implementation**

Order matters and is itself the design: search → filter to usable → refuse if zero or more than one → check trust → prompt only if interactive → fetch `SHA256SUMS` → download → **verify** → extract → move into place → write the lock. A refusal at any step leaves nothing behind.

Install writes to `<project>/.infra/plugins/`, or `~/.local/share/infrena/plugins/` with `--global` — the directories `pluginhost.DefaultSearch` already searches, so nothing moves and a hand-placed binary keeps winning.

`verify` re-reads the lock and re-hashes what is on disk.

- [ ] **Step 4: Run tests to verify they pass**

Run: `INFRENA_REQUIRE_PLUGIN=1 go test -count=1 ./... 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/cli/
git commit -m "Add plugins install and plugins verify

Naming a source in a project is not permission to download from it, so an
owner the user has not approved is refused and a run with no terminal
never approves one. The archive is checked against the published
checksums before it is opened."
```

---

## Task 6: Verify on launch

**Files:**
- Modify: `internal/pluginhost/loader.go`
- Test: `internal/pluginhost/`

- [ ] **Step 1: Write the failing test**

```go
// Section 31.3: "The host verifies on every launch once a lock exists." A
// binary replaced on disk after install is caught at the next command rather
// than never.
func TestALockedPluginWhoseBinaryChangedIsRefusedAtLaunch(t *testing.T) { /* … */ }

// A hand-placed binary the lock does not mention keeps working.
func TestAnUnlockedPluginStillLoads(t *testing.T) { /* … */ }
```

Write these against the loader's existing test fixtures — read `internal/pluginhost/*_test.go` first and follow what is there.

- [ ] **Step 2–5:** as the pattern above. The check must add no network and no measurable startup cost beyond hashing a file that is about to be executed anyway.

Commit message:

```
Check a locked plugin against its checksum before running it

A binary replaced on disk after it was installed is caught at the next
command rather than never.
```

---

## Task 7: The interactive offer

**Files:**
- Modify: `internal/cli/` where `pluginhost.NotFoundError` surfaces

Per §31.3: when a plugin is missing **and** stdin is a terminal **and** searching is not disabled, search, print every survivor with owner/version/description, ask, install, then **stop and say so** — it does not continue the command. With no TTY, print the existing error plus the one-line `infrena plugins install` that would fix it, and never block on input that cannot come.

A test must assert the command does NOT continue after installing, and one must assert the no-TTY path never prompts.

Commit message:

```
Offer to install a plugin a project needs and the machine lacks

The command stops after installing rather than continuing, so the first
half of a run never happens under different conditions from the second.
```

---

## Task 8: Verify against the real GitHub, then document

**MANDATORY. Unit 2 shipped five bugs with a green suite because every test answered whatever the client asked. Do not skip this, and do not report success without pasting what you actually saw.**

- [ ] **Step 1: Build and run against the real API**

```bash
go build -o /tmp/infrena ./cmd/infrena
rm -rf ~/.cache/infrena/plugins
```

Then, in a scratch project directory, run and record the real output of each:

1. `INFRENA_GITHUB_TOKEN="$(gh auth token)" /tmp/infrena plugins install aws` — must install `infrena-plugin-aws` and write `plugins.lock`.
2. `/tmp/infrena plugins list` — must show it.
3. `/tmp/infrena plugins verify` — must pass.
4. Corrupt the installed binary (`echo x >> …`), then `/tmp/infrena plugins verify` — **must fail and name the plugin**.
5. `/tmp/infrena plan dev` with that corrupted binary — must refuse to run it.

- [ ] **Step 2: Report what you saw**, verbatim, including anything that did not behave as the plan expects. A step that fails is a finding, not a failure to hide.

- [ ] **Step 3: Update `PLAN.md` §31.3 and `CLAUDE.md`** with what shipped.

- [ ] **Step 4: Full verification**

Run: `INFRENA_REQUIRE_PLUGIN=1 go test -count=1 ./... && go vet ./... && gofmt -l .`

- [ ] **Step 5: Commit**

```bash
git add PLAN.md CLAUDE.md
git commit -m "Document plugin install, the lock file and verification"
```
