package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ProjectFileName is the root configuration file.
const ProjectFileName = "infra.yml"

// File is a parsed source file. The node tree keeps line and column
// information, which every diagnostic depends on.
type File struct {
	Path string
	Root *yaml.Node
}

// Load reads the project file from dir.
//
// M4 extends this to variables.yml and environments/*.yml, when the systems
// that consume them exist.
func Load(dir string) ([]File, error) {
	path := filepath.Join(dir, ProjectFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no %s found in %s; run `infra init` to create one", ProjectFileName, dir)
		}
		return nil, err
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return []File{{Path: path, Root: &root}}, nil
}
