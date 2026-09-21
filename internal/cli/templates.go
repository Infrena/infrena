package cli

import (
	"os"
	"path/filepath"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/pkg/value"
)

// TemplatesDirName is the directory holding files referenced as
// ${template.NAME} and ${file.NAME}.
//
// Two places, the same shape vars/ has: <project>/templates/ is shared by
// everything, and resources/<directory>/templates/ belongs to the resources in
// that directory and wins for a name both define.
const TemplatesDirName = "templates"

// templateSource builds the resolver for ${template.NAME} and ${file.NAME}.
//
// Nearest wins: a directory's own templates/ beats the project's, exactly as
// its vars/ beats the project's variables.
//
// Files are read at evaluation rather than loaded up front, so a project with a
// hundred templates does not pay for the ninety-eight a given plan never
// references.
//
// The returned origin is the file, not the configuration line that referenced
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
// A name that escapes the templates directory is refused by returning nothing,
// so `${template.../../etc/passwd}` cannot read a file outside the project.
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
