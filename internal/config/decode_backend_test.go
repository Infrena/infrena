package config

import (
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/diag"
)

// renderBackendDiags is the rendered text of a diagnostic set, which is what a
// user reads. Asserting against the rendered form rather than a Summary field
// keeps a test honest about whether the explanation actually reaches them.
func renderBackendDiags(ds diag.Diagnostics) string {
	var sb strings.Builder
	ds.Render(&sb)
	return sb.String()
}

func TestBackendBlockDecodes(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + `
backend:
  plugin: s3
  bucket: some-bucket-name
  profile: my-bucket-profile
  path: /infrena/
`,
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", renderBackendDiags(ds))
	}
	if p.Backend.Plugin != "s3" {
		t.Errorf("Plugin = %q, want s3", p.Backend.Plugin)
	}
	// plugin: is the ONLY reserved key. Everything else reaches the backend
	// untouched, so the engine never learns what a bucket is.
	for k, want := range map[string]any{"bucket": "some-bucket-name", "profile": "my-bucket-profile", "path": "/infrena/"} {
		if p.Backend.Config[k] != want {
			t.Errorf("Config[%q] = %v, want %v", k, p.Backend.Config[k], want)
		}
	}
	if _, reserved := p.Backend.Config["plugin"]; reserved {
		t.Error("plugin: was passed through to the backend as configuration")
	}
}

// An unrecognised key is NOT an error here, unlike every other block in this
// language. The engine cannot know which keys an s3 backend accepts, so the
// only honest thing it can do with one it does not recognise is hand it over.
// Nested shapes and non-string scalars go across whole, for the same reason.
func TestEveryKeyButPluginReachesTheBackendUntouched(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + `
backend:
  plugin: s3
  retries: 3
  verify: true
  endpoints:
    - one
    - two
  sse:
    algorithm: aws:kms
`,
	})
	if ds.HasErrors() {
		t.Fatalf("an unrecognised backend key was refused:\n%s", renderBackendDiags(ds))
	}
	if p.Backend.Config["retries"] != 3 {
		t.Errorf("Config[retries] = %#v, want 3", p.Backend.Config["retries"])
	}
	if p.Backend.Config["verify"] != true {
		t.Errorf("Config[verify] = %#v, want true", p.Backend.Config["verify"])
	}
	list, ok := p.Backend.Config["endpoints"].([]any)
	if !ok || len(list) != 2 || list[0] != "one" {
		t.Errorf("Config[endpoints] = %#v, want [one two]", p.Backend.Config["endpoints"])
	}
	nested, ok := p.Backend.Config["sse"].(map[string]any)
	if !ok || nested["algorithm"] != "aws:kms" {
		t.Errorf("Config[sse] = %#v, want map with algorithm", p.Backend.Config["sse"])
	}
}

// No backend block means local, which is what every project does today.
func TestNoBackendBlockMeansLocal(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{ProjectFileName: projectWithNoResources})
	if ds.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", renderBackendDiags(ds))
	}
	if p.Backend.Plugin != "" {
		t.Errorf("Plugin = %q, want empty", p.Backend.Plugin)
	}
}

// THE CYCLE. You need state before you can compile, and compiling is what
// resolves variables, so a variable here can never be resolved. The
// diagnostic must explain that rather than saying "unknown variable", which
// would send a reader off to declare one.
func TestAVariableInTheBackendBlockIsRefusedWithTheReason(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + "backend:\n  plugin: s3\n  bucket: ${var.bucket}\n",
	})
	if !ds.HasErrors() {
		t.Fatal("a variable in the backend block was accepted")
	}
	msg := renderBackendDiags(ds)
	if !strings.Contains(msg, "before") {
		t.Errorf("diagnostic does not explain the ordering: %s", msg)
	}
	if !strings.Contains(msg, "bucket") {
		t.Errorf("diagnostic does not name the key: %s", msg)
	}
	// "unknown variable" is the message this one exists instead of. A reader
	// told that goes and declares `bucket`, and the next run fails identically.
	if strings.Contains(msg, "unknown variable") || strings.Contains(msg, "undeclared") {
		t.Errorf("diagnostic sends the reader off to declare a variable: %s", msg)
	}
}

// A variable buried inside a nested value is the same cycle, and reaches the
// plugin the same way. Scanning only the top level would let it through.
func TestAVariableNestedInTheBackendBlockIsAlsoRefused(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources +
			"backend:\n  plugin: s3\n  sse:\n    key: ${var.kms_key}\n",
	})
	if !ds.HasErrors() {
		t.Fatal("a nested variable in the backend block was accepted")
	}
	if msg := renderBackendDiags(ds); !strings.Contains(msg, "sse.key") {
		t.Errorf("diagnostic does not name the nested key: %s", msg)
	}
}

// A missing `plugin:` IS an error, because that is the one key the engine
// owns. Without it there is no backend to hand the rest of the block to.
func TestAMissingPluginKeyIsAnError(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + "backend:\n  bucket: acme\n",
	})
	if !ds.HasErrors() {
		t.Fatal("a backend block with no plugin was accepted")
	}
	if msg := renderBackendDiags(ds); !strings.Contains(msg, "plugin") {
		t.Errorf("error does not name the missing key: %s", msg)
	}
}

// `backend:` is a mapping. A scalar or a list is refused rather than read as
// an empty block, which would silently mean local.
func TestABackendBlockThatIsNotAMappingIsRefused(t *testing.T) {
	for _, body := range []string{"backend: s3\n", "backend:\n  - plugin: s3\n", "backend:\n"} {
		_, ds := decodeTree(t, map[string]string{ProjectFileName: projectWithNoResources + body})
		if !ds.HasErrors() {
			t.Errorf("accepted a backend block written as %q", body)
		}
	}
}
