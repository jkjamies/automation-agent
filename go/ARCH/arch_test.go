package arch

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot resolves the repository root (the parent of the ARCH/ directory).
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("repoRoot: %v", err)
	}
	return root
}

// modulePath reads the module path from go.mod.
func modulePath(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	t.Fatal("module path not found in go.mod")
	return ""
}

type fileImports struct {
	path    string
	imports []string
}

// goFiles parses every .go file under root and returns its import paths.
func goFiles(t *testing.T, root string) []fileImports {
	t.Helper()
	var out []fileImports
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		var imps []string
		for _, is := range f.Imports {
			imps = append(imps, strings.Trim(is.Path.Value, `"`))
		}
		out = append(out, fileImports{path: p, imports: imps})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func skipDir(name string) bool {
	switch name {
	case ".git", ".claude", "node_modules", "vendor", "specs":
		return true
	}
	return false
}

func under(root, dir string, pkgs ...string) bool {
	for _, p := range pkgs {
		base := filepath.Join(root, "internal", p)
		if dir == base || strings.HasPrefix(dir, base+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func rel(root, p string) string {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return r
}

// Tooling packages are deterministic and must never depend on agent packages.
func TestToolingDoesNotImportAgents(t *testing.T) {
	root := repoRoot(t)
	agentPrefix := modulePath(t) + "/internal/agent"
	tooling := []string{"githubapi", "gitrepo", "webhook", "notify"}
	for _, fi := range goFiles(t, filepath.Join(root, "internal")) {
		if !under(root, filepath.Dir(fi.path), tooling...) {
			continue
		}
		for _, imp := range fi.imports {
			if strings.HasPrefix(imp, agentPrefix) {
				t.Errorf("%s imports agent package %s — tooling must not depend on agents", rel(root, fi.path), imp)
			}
		}
	}
}

// Provider / infrastructure SDKs (Ollama, Gemini, genai, and the SQLite session-store
// backend) may only be imported from agent/setup.
func TestProviderSDKsOnlyInSetup(t *testing.T) {
	root := repoRoot(t)
	setupDir := filepath.Join(root, "internal", "agent", "setup")
	providerPat := regexp.MustCompile(`(ollama/ollama|adk/model/gemini|google\.golang\.org/genai|adk/session/database|glebarez/sqlite|gorm\.io/gorm|cloud\.google\.com/go/firestore)`)
	for _, fi := range goFiles(t, filepath.Join(root, "internal")) {
		dir := filepath.Dir(fi.path)
		if dir == setupDir || strings.HasPrefix(dir, setupDir+string(filepath.Separator)) {
			continue
		}
		for _, imp := range fi.imports {
			if providerPat.MatchString(imp) {
				t.Errorf("%s imports provider SDK %s outside internal/agent/setup", rel(root, fi.path), imp)
			}
		}
	}
}

// Nothing may import the cmd/ entrypoint packages.
func TestNothingImportsCmd(t *testing.T) {
	root := repoRoot(t)
	cmdPrefix := modulePath(t) + "/cmd"
	for _, fi := range goFiles(t, root) {
		if strings.HasPrefix(rel(root, fi.path), "cmd"+string(filepath.Separator)) {
			continue
		}
		for _, imp := range fi.imports {
			if strings.HasPrefix(imp, cmdPrefix) {
				t.Errorf("%s imports cmd package %s", rel(root, fi.path), imp)
			}
		}
	}
}
