package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryDirHasAgentsDoc asserts that every meaningful directory carries an
// AGENTS.md. docs/ and specs/ and hidden dirs (except .agents) are exempt.
func TestEveryDirHasAgentsDoc(t *testing.T) {
	root := repoRoot(t)
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if p != root && skipDocDir(d.Name()) {
			return filepath.SkipDir
		}
		if _, statErr := os.Stat(filepath.Join(p, "AGENTS.md")); statErr != nil {
			r := rel(root, p)
			if r == "." {
				r = "(root)"
			}
			t.Errorf("missing AGENTS.md in %s", r)
		}
		// The .agents tree is documented by a single top-level AGENTS.md; its
		// subdirectories are intentionally exempt, so don't descend into them.
		if d.Name() == ".agents" {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

func skipDocDir(base string) bool {
	switch base {
	case ".git", ".claude", "node_modules", "vendor", "specs", "docs":
		return true
	// Content subdirs of an agent are documented by the agent's shared AGENTS.md.
	case "prompts", "models", "tasks", "testdata":
		return true
	}
	// Hidden directories are exempt, except the .agents open-standard dir.
	if strings.HasPrefix(base, ".") && base != ".agents" {
		return true
	}
	return false
}
