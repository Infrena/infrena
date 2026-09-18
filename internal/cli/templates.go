package cli

import (
	"os"
	"path/filepath"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/pkg/value"
)

// TemplatesDirName holds files referenced as ${template.NAME} and ${file.NAME}.
//
// Two places, the same shape vars/ has (§4.1): <project>/templates/ is shared by
// everything, and resources/<directory>/templates/ belongs to the resources in
// that directory and WINS for a name both define. A project that never uses
// templates has neither directory and never notices.
const TemplatesDirName = "templates"

// templateSource builds the resolver for ${template.NAME} and ${file.NAME}.
//
// NEAREST WINS, which is the same direction every other scoped thing in this
// project runs: a directory's own templates/ beats the project's, exactly as
// its vars/ beats the project's variables. A team that keeps one shared IAM
// policy and overrides it for one directory writes what they already know.
//
// Read at EVALUATION rather than loaded up front. A project's templates
// directory may hold a hundred files of which a plan uses two, and reading the
// other ninety-eight to answer a question nobody asked would make every command
// slower for the projects that lean on this feature hardest.
//
// The returned origin is the FILE, not the configuration line that referenced
// it, so a malformed reference inside a template reports where it actually is.
func templateSource(opts *GlobalOptions) func(dir, name string) (string, value.Origin, bool) {
	return func(dir, name string) (string, value.Origin, bool) {
		for _, path := range templateCandidates(opts.Dir, dir, name) {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			return string(data), value.Origin{File: path}, true
		}
		return "", value.Origin{}, false
	}
}

// templateCandidates lists where a template may live, nearest first.
//
// The name is joined rather than concatenated, and a name that escapes the
// templates directory is refused by returning nothing: `${template.../../etc/passwd}`
// must not read a file outside the project. Configuration is committed and
// reviewed, so this is not the strongest threat in the model — but a reference
// that silently reads an arbitrary path is worth refusing whatever the threat,
// and the refusal costs one comparison.
func templateCandidates(projectDir, resourceDir, name string) []string {
	clean := filepath.Clean(filepath.Join(TemplatesDirName, name))
	if !filepath.IsLocal(clean) {
		return nil
	}

	var out []string
	if resourceDir != "" {
		out = append(out, filepath.Join(projectDir, config.ResourcesDirName, resourceDir, clean))
	}
	return append(out, filepath.Join(projectDir, clean))
}
