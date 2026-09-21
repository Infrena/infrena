package integration

// The local -> S3 -> local state round trip.
//
// THIS IS THE ONE TEST A FAKE CANNOT STAND IN FOR, and the reason is the last
// assertion rather than the first. Either backend alone can be shown to read
// its own writing, which proves nothing about the pair: a backend that dropped
// a field on the way out and invented it again on the way back would pass such
// a test perfectly. Going OUT to a real object store and BACK is what makes
// the two disagree if they disagree, because the bytes that come home were
// written by one implementation and read by the other.
//
// EVERY ASSERTION ABOUT THE BUCKET READS THE BUCKET, over a signed request
// this file builds itself. infrena reporting that it wrote state is infrena's
// opinion of infrena, and the question here is what is actually in the store.
//
// It SKIPS without a store or without an infrena-backend-s3 checkout, and
// REQUIRE_LIVE_STORE=1 turns that skip into a failure -- a suite that silently
// skips reports green for a test that never ran, which is worse than no test
// because somebody trusts it.
//
//	docker run -d --name infrena-minio -p 127.0.0.1:9000:9000 \
//	  -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
//	  quay.io/minio/minio:latest server /data
//
// `server /data` is an ARGUMENT TO THE IMAGE, which is why this is a container
// run rather than a CI service container: a service container can set an image
// and an environment and cannot pass that, and MinIO without it exits.

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	liveEndpointEnv = "INFRENA_LIVE_S3_ENDPOINT"
	liveAccessEnv   = "INFRENA_LIVE_S3_ACCESS_KEY"
	liveSecretEnv   = "INFRENA_LIVE_S3_SECRET_KEY"
	liveRegionEnv   = "INFRENA_LIVE_S3_REGION"
	backendRepoEnv  = "INFRENA_BACKEND_S3_REPO"
	// requireLiveStoreEnv turns every skip in this file into a failure. CI sets
	// it; a laptop without docker does not.
	requireLiveStoreEnv = "REQUIRE_LIVE_STORE"
)

const (
	defaultLiveEndpoint = "http://127.0.0.1:9000"
	defaultLiveKey      = "minioadmin"
	defaultLiveRegion   = "us-east-1"
	// statePrefix is the `path:` the project's backend block asks for, so the
	// keys under test are not at the bucket root and a prefix bug has
	// somewhere to show itself.
	statePrefix = "state"
)

// store is where the live store is and how to talk to it.
type store struct {
	endpoint string
	region   string
	access   string
	secret   string
}

func liveStore() store {
	return store{
		endpoint: strings.TrimSuffix(envOr(liveEndpointEnv, defaultLiveEndpoint), "/"),
		region:   envOr(liveRegionEnv, defaultLiveRegion),
		access:   envOr(liveAccessEnv, defaultLiveKey),
		secret:   envOr(liveSecretEnv, defaultLiveKey),
	}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// TestStateSurvivesARoundTripThroughAnotherBackend.
//
// One test rather than six, because every step depends on the state the one
// before it left behind and a suite of independent cases would have to rebuild
// the world each time -- against a real store, from a real apply. The steps are
// numbered in the output so a failure says which one broke.
func TestStateSurvivesARoundTripThroughAnotherBackend(t *testing.T) {
	s := liveStore()
	s.require(t)
	backend := buildS3Backend(t)

	// Nothing on the developer's machine may sign a request in this test, and
	// nothing may reach an instance metadata service. The credentials the
	// backend uses are the ones set here and no others, which is also how the
	// backend documents a non-AWS store being reached.
	t.Setenv("AWS_ACCESS_KEY_ID", s.access)
	t.Setenv("AWS_SECRET_ACCESS_KEY", s.secret)
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "no-such-credentials"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "no-such-config"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	bucket := s.newBucket(t)
	environments := []string{"dev", "production"}

	// ---------------------------------------------------------------
	// 1. A project with local state and two environments, each holding
	//    real resources after an apply.
	// ---------------------------------------------------------------
	dir := project(t, localProject)
	installBackendBinary(t, dir, backend)

	for _, environment := range environments {
		// The REPORT, not the exit code. An apply that made changes exits 2,
		// which says there were changes and not whether they worked -- the
		// same reason m3_convergence_test.go's `applied` reads the output.
		r := run(t, dir, "apply", environment, "--auto-approve")
		if !strings.Contains(r.combined(), "Apply complete") || !strings.Contains(r.combined(), "0 failed") {
			t.Fatalf("step 1: apply %s did not complete (exit %d):\n%s",
				environment, r.ExitCode, r.combined())
		}
	}
	// The bytes the whole test is about. Read before anything else touches
	// them, because from here on every command may rewrite them.
	original := map[string][]byte{}
	for _, environment := range environments {
		original[environment] = readBytes(t, localStatePath(dir, environment))
		if len(original[environment]) == 0 {
			t.Fatalf("step 1: %s has no local state after an apply", environment)
		}
	}

	// ---------------------------------------------------------------
	// 2. Point `backend:` at S3, `migrate_from:` at local, migrate.
	// ---------------------------------------------------------------
	writeProject(t, dir, s.projectMigratingTo(bucket))
	if r := run(t, dir, "state", "migrate"); r.ExitCode != 0 {
		t.Fatalf("step 2: state migrate exit = %d:\n%s", r.ExitCode, r.combined())
	}

	// ---------------------------------------------------------------
	// 3. The objects are in the bucket, READ FROM THE BUCKET.
	// ---------------------------------------------------------------
	listing := s.list(t, bucket, statePrefix+"/")
	for _, environment := range environments {
		key := statePrefix + "/" + environment + ".json"
		if !strings.Contains(listing, "<Key>"+key+"</Key>") {
			t.Errorf("step 3: %s is not in the bucket listing:\n%s", key, listing)
		}
		stored := s.get(t, bucket, key)
		t.Logf("step 3: %s holds %d bytes in the bucket", key, len(stored))
		if !sameStateContent(t, original[environment], stored) {
			t.Errorf("step 3: the object in the bucket is not the state that was migrated\n"+
				"local:\n%s\nbucket:\n%s", original[environment], stored)
		}
	}

	// THE LOCAL COPY GOES NOW, and it is not tidying. It is doing two jobs.
	//
	// It makes step 4 mean something: with the old state still on disk, a
	// `plan` that had quietly fallen back to local would be just as clean as
	// one that really read the bucket, and the step would pass either way.
	//
	// And it makes step 6 possible at all: left in place, local and the bucket
	// hold the same state, the return migration compares them, says "already
	// migrated" and correctly writes nothing -- so step 6 would compare a file
	// with itself and pass without a round trip ever having happened.
	for _, environment := range environments {
		if err := os.Remove(localStatePath(dir, environment)); err != nil {
			t.Fatalf("step 3: removing the local copy: %v", err)
		}
	}

	// ---------------------------------------------------------------
	// 4. A plan against the migrated state is CLEAN, and there is nothing
	//    left on disk for it to read. Exit 0, not 2: the migrated state is
	//    UNDERSTOOD, not merely stored, and a backend that round-tripped an
	//    attribute badly shows up here as drift rather than as an error.
	// ---------------------------------------------------------------
	for _, environment := range environments {
		r := run(t, dir, "plan", environment)
		if r.ExitCode != 0 {
			t.Fatalf("step 4: plan %s over migrated state exit = %d, want 0 (no changes):\n%s",
				environment, r.ExitCode, r.combined())
		}
	}

	// ---------------------------------------------------------------
	// 5. Swap the blocks and migrate back.
	// ---------------------------------------------------------------
	writeProject(t, dir, s.projectMigratingFrom(bucket))
	if r := run(t, dir, "state", "migrate"); r.ExitCode != 0 {
		t.Fatalf("step 5: the return migration exit = %d:\n%s", r.ExitCode, r.combined())
	}

	// ---------------------------------------------------------------
	// 6. THE POINT OF THE WHOLE TEST. The state that came home is the state
	//    that left, to the byte, once the two fields a WRITE stamps are set
	//    aside -- and the second assertion is what makes the first one mean
	//    something: those two fields are the ONLY difference, so nothing else
	//    moved, was reordered, or was re-encoded on the way.
	// ---------------------------------------------------------------
	for _, environment := range environments {
		returned := readBytes(t, localStatePath(dir, environment))
		if bytes.Equal(original[environment], returned) {
			t.Logf("step 6: %s came back byte-identical", environment)
			continue
		}
		if !sameStateContent(t, original[environment], returned) {
			t.Errorf("step 6: %s did not survive the round trip\nbefore:\n%s\nafter:\n%s",
				environment, original[environment], returned)
			continue
		}
		fields := differingFields(t, original[environment], returned)
		// LOGGED RATHER THAN LEFT IMPLICIT. A difference is a finding even
		// when it is an allowed one, and a reader of a green run should be
		// able to see exactly which bytes moved and what they were.
		t.Logf("step 6: %s came back identical except %v\nbefore: %s\nafter:  %s",
			environment, fields,
			writeStampsOf(t, original[environment]), writeStampsOf(t, returned))
		if !onlyWriteStamps(fields) {
			t.Errorf("step 6: %s came back differing in %v, and only serial and updated_at may differ\n"+
				"before:\n%s\nafter:\n%s", environment, fields, original[environment], returned)
		}
	}
}

// localProject is step 1's configuration: two resources rather than one, and
// one of them REFERRING to the other, so the state under test holds a resolved
// reference and not just a scalar.
const localProject = `
project: roundtrip
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
  database:
    type: fake.database
    engine: postgres
    network: ${network.id}
`

// projectMigratingTo is step 2: state lives in the bucket now, and local is
// named rather than left out, because leaving it out already means "no
// migration".
func (s store) projectMigratingTo(bucket string) string {
	return localProject + fmt.Sprintf(`
backend:
  plugin: s3
  bucket: %s
  path: /%s/
  endpoint: %s
  region: %s
  path_style: true

migrate_from:
  plugin: local
`, bucket, statePrefix, s.endpoint, s.region)
}

// projectMigratingFrom is step 5, which is the same two blocks the other way
// round. Nothing else in the file changes.
func (s store) projectMigratingFrom(bucket string) string {
	return localProject + fmt.Sprintf(`
backend:
  plugin: local

migrate_from:
  plugin: s3
  bucket: %s
  path: /%s/
  endpoint: %s
  region: %s
  path_style: true
`, bucket, statePrefix, s.endpoint, s.region)
}

// localStatePath is where the built-in backend keeps an environment's state:
// `.infrena/state/<environment>.json`, which this file names directly rather
// than asking infrena, for the reason it reads the bucket directly.
func localStatePath(dir, environment string) string {
	return filepath.Join(dir, ".infrena", "state", environment+".json")
}

// readBytes is examples_test.go's readFile for a caller that needs the bytes
// rather than a string, which is this whole file's subject.
func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// ---------------------------------------------------------------------------
// Comparing two states
// ---------------------------------------------------------------------------

// sameStateContent reports whether two encoded states hold the same thing,
// with the two fields a write stamps set aside.
//
// Serial and UpdatedAt are stamped by whichever backend performed the WRITE,
// so a faithful copy necessarily carries different values for both and a raw
// byte comparison of a round trip can never pass. They are the only two fields
// cleared; every resource, attribute and key ordering crosses into the
// comparison exactly as it was stored, which is what makes this a comparison of
// the state rather than of a summary of it.
func sameStateContent(t *testing.T, a, b []byte) bool {
	t.Helper()
	return bytes.Equal(withoutWriteStamps(t, a), withoutWriteStamps(t, b))
}

func withoutWriteStamps(t *testing.T, encoded []byte) []byte {
	t.Helper()
	doc := decodeState(t, encoded)
	delete(doc, "serial")
	delete(doc, "updated_at")
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// differingFields names the top-level keys whose values are not identical, so
// a failure says WHICH field moved rather than printing two documents and
// leaving the reader to diff them.
func differingFields(t *testing.T, a, b []byte) []string {
	t.Helper()
	left, right := decodeState(t, a), decodeState(t, b)
	seen := map[string]bool{}
	var names []string
	for _, doc := range []map[string]json.RawMessage{left, right} {
		for name := range doc {
			if seen[name] {
				continue
			}
			seen[name] = true
			if !bytes.Equal(left[name], right[name]) {
				names = append(names, name)
			}
		}
	}
	// Sorted, so a failure message says the same thing twice running. Map
	// iteration order would otherwise make one report read differently from
	// the next for an identical difference.
	sort.Strings(names)
	return names
}

// onlyWriteStamps reports whether a difference is entirely bookkeeping about
// the write itself. Anything else is a backend disagreeing about what state is.
func onlyWriteStamps(fields []string) bool {
	for _, name := range fields {
		if name != "serial" && name != "updated_at" {
			return false
		}
	}
	return true
}

// writeStampsOf renders just the two fields a write may change, for a log line
// that says what differed rather than making a reader diff two documents.
func writeStampsOf(t *testing.T, encoded []byte) string {
	t.Helper()
	doc := decodeState(t, encoded)
	return fmt.Sprintf("serial=%s updated_at=%s", doc["serial"], doc["updated_at"])
}

func decodeState(t *testing.T, encoded []byte) map[string]json.RawMessage {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &doc); err != nil {
		t.Fatalf("state is not JSON: %v\n%s", err, encoded)
	}
	return doc
}

// ---------------------------------------------------------------------------
// Building the backend under test
// ---------------------------------------------------------------------------

// buildS3Backend compiles infrena-backend-s3 from its own checkout.
//
// From ITS module, not this one: it is a separate repository with its own
// go.mod, and `go build` of a package outside the main module is refused.
func buildS3Backend(t *testing.T) string {
	t.Helper()
	repo := os.Getenv(backendRepoEnv)
	if repo == "" {
		repo = filepath.Join(filepath.Dir(repoRoot(t)), "infrena-backend-s3")
	}
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		skipOrFail(t, fmt.Sprintf("no infrena-backend-s3 checkout at %s: %v\n"+
			"Clone github.com/infrena/infrena-backend-s3 beside this repository, or set %s.",
			repo, err, backendRepoEnv))
	}

	out := filepath.Join(t.TempDir(), "infrena-backend-s3")
	// The BINARY NAME is how infrena finds it: a backend is looked up as
	// infrena-backend-<name> for the `plugin:` the block names.
	cmd := exec.Command("go", "build", "-o", out, "./cmd/infrena-backend-s3")
	cmd.Dir = repo
	if built, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building infrena-backend-s3 in %s: %v\n%s", repo, err, built)
	}
	return out
}

// installBackendBinary puts the backend on the project's own plugin search
// path, which is where an installed one lands.
func installBackendBinary(t *testing.T, dir, built string) {
	t.Helper()
	plugins := filepath.Join(dir, ".infrena", "plugins")
	if err := os.MkdirAll(plugins, 0o755); err != nil {
		t.Fatal(err)
	}
	body := readBytes(t, built)
	if err := os.WriteFile(filepath.Join(plugins, "infrena-backend-s3"), body, 0o755); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// Reading the bucket, beside the backend rather than through it
// ---------------------------------------------------------------------------

// require skips unless the store is reachable, or fails when REQUIRE_LIVE_STORE
// says a skip is not acceptable.
func (s store) require(t *testing.T) {
	t.Helper()
	res, err := s.do(t, "GET", "/", "", nil)
	if err != nil {
		skipOrFail(t, fmt.Sprintf("no S3-compatible store at %s: %v\n"+
			"Start one with `docker run -d -p 127.0.0.1:9000:9000 "+
			"-e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin "+
			"quay.io/minio/minio:latest server /data`, or point %s at another store.",
			s.endpoint, err, liveEndpointEnv))
	}
	if res.status != http.StatusOK {
		skipOrFail(t, fmt.Sprintf("the store at %s would not list buckets: %d\n%s",
			s.endpoint, res.status, res.body))
	}
}

// skipOrFail is the one place this file decides between the two, so a skip
// cannot be added later that REQUIRE_LIVE_STORE does not turn into a failure.
func skipOrFail(t *testing.T, message string) {
	t.Helper()
	if os.Getenv(requireLiveStoreEnv) != "" {
		t.Fatalf("%s\n%s is set, so this is a failure rather than a skip.", message, requireLiveStoreEnv)
	}
	t.Skip(message)
}

// newBucket creates a bucket named for this run and removes it afterwards.
func (s store) newBucket(t *testing.T) string {
	t.Helper()
	suffix := make([]byte, 5)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	bucket := "infrena-roundtrip-" + hex.EncodeToString(suffix)

	res, err := s.do(t, "PUT", "/"+bucket, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.status != http.StatusOK {
		t.Fatalf("creating bucket %s: %d\n%s", bucket, res.status, res.body)
	}
	t.Cleanup(func() { s.emptyBucket(t, bucket) })
	return bucket
}

// emptyBucket deletes everything this run put in the bucket and then the
// bucket, best effort: a store left with a stray bucket is untidy, and a test
// that failed for that reason would be reporting the wrong thing.
func (s store) emptyBucket(t *testing.T, bucket string) {
	t.Helper()
	listing, err := s.do(t, "GET", "/"+bucket, "list-type=2", nil)
	if err != nil || listing.status != http.StatusOK {
		return
	}
	for _, key := range keysIn(string(listing.body)) {
		_, _ = s.do(t, "DELETE", "/"+bucket+"/"+key, "", nil)
	}
	_, _ = s.do(t, "DELETE", "/"+bucket, "", nil)
}

// keysIn pulls the <Key> elements out of a ListObjectsV2 response. A reader
// rather than a parser, because the only thing wanted from that document is
// the keys and an XML decoder here would be a schema to maintain.
func keysIn(listing string) []string {
	var keys []string
	for rest := listing; ; {
		_, after, found := strings.Cut(rest, "<Key>")
		if !found {
			return keys
		}
		key, tail, found := strings.Cut(after, "</Key>")
		if !found {
			return keys
		}
		keys = append(keys, key)
		rest = tail
	}
}

func (s store) list(t *testing.T, bucket, prefix string) string {
	t.Helper()
	res, err := s.do(t, "GET", "/"+bucket, "list-type=2&prefix="+url.QueryEscape(prefix), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.status != http.StatusOK {
		t.Fatalf("listing %s: %d\n%s", bucket, res.status, res.body)
	}
	return string(res.body)
}

func (s store) get(t *testing.T, bucket, key string) []byte {
	t.Helper()
	res, err := s.do(t, "GET", "/"+bucket+"/"+key, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.status != http.StatusOK {
		t.Fatalf("reading %s/%s from the bucket: %d\n%s", bucket, key, res.status, res.body)
	}
	return res.body
}

type response struct {
	status int
	body   []byte
}

// do signs and sends one request.
//
// SIGNED HERE, with the standard library and nothing else. The alternative was
// an S3 client library, and this repository's whole third-party budget is two
// packages -- neither of them one. Fifty lines of SigV4 is the cost of reading
// the bucket with something that is not the code under test.
func (s store) do(t *testing.T, method, path, rawQuery string, body []byte) (response, error) {
	t.Helper()
	target := s.endpoint + escapePath(path)
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		return response{}, err
	}

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	payload := sha256Hex(body)

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payload)

	host := req.URL.Host
	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + payload + "\n" +
		"x-amz-date:" + amzDate + "\n"
	const signedHeaders = "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{
		method, escapePath(path), rawQuery, canonicalHeaders, signedHeaders, payload,
	}, "\n")

	scope := day + "/" + s.region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	key := hmacSHA256(hmacSHA256(hmacSHA256(hmacSHA256(
		[]byte("AWS4"+s.secret), day), s.region), "s3"), "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.access+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+signature)

	res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return response{}, err
	}
	defer res.Body.Close()
	read, err := io.ReadAll(res.Body)
	if err != nil {
		return response{}, err
	}
	return response{status: res.StatusCode, body: read}, nil
}

// escapePath encodes each path segment the way SigV4 requires, which is
// RFC 3986 with `/` left alone. Every path this file signs is already
// unreserved, and the encoding is here so that stops being something the test
// relies on.
func escapePath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		parts[i] = strings.ReplaceAll(url.QueryEscape(part), "+", "%20")
	}
	return strings.Join(parts, "/")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}
