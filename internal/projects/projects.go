package projects

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/local/jeff/internal/contexts"
)

const Root = "/home/tray/project"

type Catalog struct{ Config *contexts.ContextsConfig }

func Load(path string) (*Catalog, *contexts.QaSettings, error) {
	file, err := contexts.Load(path)
	if err != nil {
		return nil, nil, err
	}
	for name, project := range file.Contexts.Contexts {
		if !validAlias(name) {
			return nil, nil, fmt.Errorf("project %q has an invalid alias", name)
		}
		if !underRoot(project.Directory) {
			return nil, nil, fmt.Errorf("project %q directory must be under %s", name, Root)
		}
	}
	return &Catalog{Config: file.Contexts}, file.QA, nil
}

func validAlias(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && ((r >= '0' && r <= '9') || r == '_' || r == '-')) {
			continue
		}
		return false
	}
	return true
}

func underRoot(path string) bool {
	rel, err := filepath.Rel(Root, filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (c *Catalog) Names() []string {
	names := make([]string, 0, len(c.Config.Contexts))
	for name := range c.Config.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
