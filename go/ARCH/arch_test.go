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
	tooling := []string{"githubapi", "gitrepo", "webhook", "notify", "tasks", "obs"}
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

// Only internal/config may read the OTEL_* environment. Tracing config flows through the
// typed Config like every other setting; obs and the rest of the tree take it as a struct,
// never os.Getenv("OTEL_..."). A stray read elsewhere would fork configuration away from the
// single source of truth (and out of the masked-secret String view). Enforced by source
// scan: the literal "OTEL_" outside internal/config flags a direct env reference.
//
// The scan covers the whole module, not just internal/. cmd/ is where a direct env read is most
// likely to appear — an entrypoint already touching os for args and signals, wanting one setting
// config doesn't expose yet — and scanning only internal/ let exactly that through unnoticed.
func TestOnlyConfigReadsOTELEnv(t *testing.T) {
	root := repoRoot(t)
	configDir := filepath.Join(root, "internal", "config")
	archDir := filepath.Join(root, "ARCH")
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
		dir := filepath.Dir(p)
		// config owns the OTEL_* env vars; ARCH is the rule's own source, where the literal is
		// the thing being searched for rather than a violation.
		if dir == configDir || strings.HasPrefix(dir, configDir+string(filepath.Separator)) || dir == archDir {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(b), `"OTEL_`) {
			t.Errorf("%s references an OTEL_ env var literal — only internal/config may read OTEL_*", rel(root, p))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
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

// ⚠️ TO BUILD: a visibility conformance test.
//
// okf/standards/go-style.md states a visibility rule — a type is exported only if it is (1)
// named in an exported signature, (2) a seam a caller holds, or (3) an optional-capability
// interface a caller type-asserts against. It is enforced by review today. It should be
// enforced here, and the work below is scoped rather than speculative.
//
// Do it as part of reconciling that file against Style Decisions and Best Practices, so the
// test encodes the surviving rules rather than a draft of them.
//
// Approach (validated as a throwaway prototype, ~130 lines, no new dependency):
//
//   - Pure go/ast, matching this suite's stdlib-only constraint. No type checker is needed:
//     within a package, a reference to a locally declared type is just an *ast.Ident with that
//     name, so collecting idents from the exported surface and diffing against exported
//     TypeSpecs is sufficient.
//   - Exported surface = params/results of exported funcs and of exported methods on exported
//     receivers, exported struct fields, exported interface method signatures, and the types of
//     exported vars/consts. An exported method on an UNEXPORTED receiver is not surface — that
//     distinction is what makes the check meaningful.
//   - Forms (2) and (3) above are invisible to this analysis, because the use is in another
//     package (a variable declaration, a type assertion). They need an explicit exemption list;
//     do not silently widen the rule to accommodate them.
//
// Validation the prototype passed, worth repeating: it flags fixflow.Driver on the tree before
// commit be3ff76 renamed it, and reports nothing for fixflow afterwards.
//
// Known findings across internal/ at the time of writing, already triaged:
//
//   tasks.Transport         form (2) — cmd/agent declares variables of it     → exempt
//   auth.IdentityResolver   form (3) — cmd/agent type-asserts against it      → exempt
//   fixflow.ResumeInput     reachable only from unexported machinery          → unexport
//   reviewer.Finding        used nowhere outside its package                  → confirm, then unexport
//   reviewer.Rule           used nowhere outside its package                  → confirm, then unexport
//
// Re-derive this list rather than trusting it; the codebase moves.
