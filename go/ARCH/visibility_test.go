package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// visPkg is one parsed package: its import path, its production files, and its test files.
type visPkg struct {
	path  string
	name  string
	files []*ast.File
	tests []*ast.File
}

// parsePackages parses every .go file under root, grouped by directory. Production files and
// test files are kept apart: only production files declare the surface the rule is about,
// while both kinds count when looking for a reference from another package.
func parsePackages(t *testing.T, root, module string) map[string]*visPkg {
	t.Helper()
	fset := token.NewFileSet()
	pkgs := map[string]*visPkg{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(d.Name()) || d.Name() == "ARCH" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		dir := filepath.Dir(p)
		if pkgs[dir] == nil {
			// A file directly in the module root has no subpath to append; anywhere else the
			// import path is the module path plus the directory relative to it. The package
			// name comes from whichever file is walked first, which may be an external test
			// file (package foo_test) — trim the suffix so it names the package under test.
			path := module
			if sub := filepath.ToSlash(rel(root, dir)); sub != "." {
				path += "/" + sub
			}
			pkgs[dir] = &visPkg{path: path, name: strings.TrimSuffix(f.Name.Name, "_test")}
		}
		if strings.HasSuffix(p, "_test.go") {
			pkgs[dir].tests = append(pkgs[dir].tests, f)
			return nil
		}
		pkgs[dir].files = append(pkgs[dir].files, f)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return pkgs
}

// externalRefs maps an import path to the identifiers other packages select from it —
// every `pkg.Ident` in the module outside pkg itself. A selector is enough: a caller holding
// a seam writes `var t tasks.Transport`, and a caller probing an optional capability writes
// `p.(auth.IdentityResolver)`. Both are selector expressions, so neither needs a type checker
// and neither needs an exemption list.
//
// Another package's tests count, because they are another package: they import the seam and
// compile against it like any caller. A package's own tests cannot appear here — they refer to
// their package's identifiers bare, not through a selector — which is what keeps "I need it in
// a test" from being a way to satisfy the rule. A same-directory `foo_test` package would be
// the exception, so its self-references are dropped explicitly below.
func externalRefs(pkgs map[string]*visPkg) map[string]map[string]bool {
	refs := map[string]map[string]bool{}
	for _, p := range pkgs {
		for _, f := range append(append([]*ast.File{}, p.files...), p.tests...) {
			// Local name -> import path, so a renamed import resolves to the right package.
			alias := map[string]string{}
			for _, im := range f.Imports {
				path, err := strconv.Unquote(im.Path.Value)
				if err != nil {
					continue
				}
				name := path[strings.LastIndex(path, "/")+1:]
				if im.Name != nil {
					name = im.Name.Name
				}
				alias[name] = path
			}
			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				path, ok := alias[id.Name]
				if !ok || path == p.path {
					return true
				}
				if refs[path] == nil {
					refs[path] = map[string]bool{}
				}
				refs[path][sel.Sel.Name] = true
				return true
			})
		}
	}
	return refs
}

// exportedSurface returns the identifiers a package names in its own exported API: the
// parameters, results and type parameters of exported functions, the same for exported
// methods on exported receivers, the types of exported struct fields, the signatures of
// exported interface methods, and the types of exported vars and consts.
//
// An exported method on an *unexported* receiver is deliberately not surface. That single
// distinction is what gives the check teeth: a type nothing outside can name does not become
// part of the contract by having capitalized methods.
func exportedSurface(p *visPkg) map[string]bool {
	surface := map[string]bool{}
	results := localFuncResults(p)

	// add records every bare identifier in a type expression. A qualified name (pkg.T)
	// belongs to another package, so descending into it would only add noise.
	add := func(e ast.Expr) {
		ast.Inspect(e, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				surface[id.Name] = true
			}
			_, qualified := n.(*ast.SelectorExpr)
			return !qualified
		})
	}
	addFields := func(fl *ast.FieldList, exportedOnly bool) {
		if fl == nil {
			return
		}
		for _, fld := range fl.List {
			if exportedOnly {
				// An embedded field has no names and is always part of the surface.
				visible := len(fld.Names) == 0
				for _, n := range fld.Names {
					if n.IsExported() {
						visible = true
					}
				}
				if !visible {
					continue
				}
			}
			add(fld.Type)
		}
	}

	for _, f := range p.files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !exportedSignature(d) {
					continue
				}
				addFields(d.Type.TypeParams, false)
				addFields(d.Type.Params, false)
				addFields(d.Type.Results, false)
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if !s.Name.IsExported() {
							continue
						}
						addFields(s.TypeParams, false)
						switch t := s.Type.(type) {
						case *ast.StructType:
							addFields(t.Fields, true)
						case *ast.InterfaceType:
							for _, m := range t.Methods.List {
								if ft, ok := m.Type.(*ast.FuncType); ok {
									addFields(ft.Params, false)
									addFields(ft.Results, false)
									continue
								}
								add(m.Type) // embedded interface
							}
						default:
							add(s.Type) // alias or defined type
						}
					case *ast.ValueSpec:
						exported := false
						for _, n := range s.Names {
							if n.IsExported() {
								exported = true
							}
						}
						if !exported {
							continue
						}
						// A declared type IS the contract; the initializer is implementation,
						// so it is not walked. `var Default Thing = newThing()` exposes Thing,
						// not newThing — a caller can hold Default but can never call it.
						if s.Type != nil {
							add(s.Type)
							continue
						}
						// With the type inferred, recover what the AST can prove and nothing
						// more. See inferredTypes: this deliberately does not fall back to
						// "every identifier mentioned", which would let an unreachable
						// function launder itself into the surface by appearing in a value.
						for _, v := range s.Values {
							for _, t := range inferredTypes(v, results) {
								add(t)
							}
						}
					}
				}
			}
		}
	}
	return surface
}

// localFuncResults maps each package-local plain function to its result type expressions, so
// the type of `var Default = newThing()` can be recovered without a type checker.
func localFuncResults(p *visPkg) map[string][]ast.Expr {
	out := map[string][]ast.Expr{}
	for _, f := range p.files {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || fd.Type.Results == nil {
				continue
			}
			var res []ast.Expr
			for _, r := range fd.Type.Results.List {
				res = append(res, r.Type)
			}
			out[fd.Name.Name] = res
		}
	}
	return out
}

// inferredTypes returns the type expressions an initializer proves, for a var or const whose
// type is inferred rather than declared.
//
// The distinction that matters is between a *type* position and a *value* position. In
// `var Default = newClient()`, `newClient` sits in a value position: it is how the value is
// produced, not what the value is. Treating it as surface would mean an exported function that
// nothing can reach could excuse itself from this check by appearing in some exported var's
// initializer — so only the function's declared results are taken, never its name.
//
// What the AST can prove, it proves; what it cannot, it declines to guess:
//
//   - a composite literal (`T{…}`, `&T{…}`, `[]T{…}`) names its type outright
//   - a function literal's signature is its type, so `func(T) {}` exposes T — a caller
//     holding the var has to supply one
//   - `new(T)` and `make([]T, n)` take a *type* as their first argument, and the spec fixes
//     that meaning, so it is readable without resolving anything
//   - a call to a package-local function contributes that function's result types
//   - a call to a local type is a conversion, which names the type outright
//   - a call to anything else (another package, a method) yields nothing. Resolving those
//     needs a type checker, and this suite is stdlib AST only.
//
// Declining to guess is the safe direction, but note which way that cuts: a missed type
// *under*-reports the surface, so a type that is genuinely reachable looks unreachable and the
// gate fails on correct code. Over-reporting is the quiet failure. So the cases above are the
// ones worth resolving precisely — each is a type position the grammar guarantees, not an
// inference — while anything needing real type information is left alone.
func inferredTypes(v ast.Expr, results map[string][]ast.Expr) []ast.Expr {
	switch e := v.(type) {
	case *ast.UnaryExpr: // &T{…}
		if e.Op == token.AND {
			return inferredTypes(e.X, results)
		}
	case *ast.CompositeLit:
		if e.Type != nil {
			return []ast.Expr{e.Type}
		}
	case *ast.FuncLit:
		// The signature is the var's type: its parameters and results are all reachable
		// through the value a caller holds.
		return []ast.Expr{e.Type}
	case *ast.CallExpr:
		id, ok := e.Fun.(*ast.Ident)
		if !ok {
			return nil
		}
		// A local declaration of this name shadows any builtin, so it is checked first.
		if res, ok := results[id.Name]; ok {
			return res
		}
		// new(T) / make(T, …): the first argument is a type, per the spec. Everything
		// reachable through it — []T, map[K]V, chan T — is named right there.
		if (id.Name == "new" || id.Name == "make") && len(e.Args) > 0 {
			return []ast.Expr{e.Args[0]}
		}
		// A conversion T(x) parses identically to a call; if the name is a local type
		// rather than a local function, it is still the type of the value either way.
		return []ast.Expr{id}
	}
	return nil
}

// exportedSignature reports whether a function declaration contributes to the package's
// exported API: an exported plain function, or an exported method whose receiver is itself
// an exported type.
func exportedSignature(d *ast.FuncDecl) bool {
	if d.Recv == nil {
		return d.Name.IsExported()
	}
	if !d.Name.IsExported() || len(d.Recv.List) != 1 {
		return false
	}
	t := d.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	// A generic receiver is written Foo[T]; the name is the indexed expression's operand.
	switch idx := t.(type) {
	case *ast.IndexExpr:
		t = idx.X
	case *ast.IndexListExpr:
		t = idx.X
	}
	id, ok := t.(*ast.Ident)
	return ok && id.IsExported()
}

// declaredExports lists the exported types and plain functions a package declares, which are
// the identifiers the rule applies to.
func declaredExports(p *visPkg) map[string]string {
	out := map[string]string{}
	for _, f := range p.files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.IsExported() {
						out[ts.Name.Name] = "type"
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.IsExported() {
					out[d.Name.Name] = "func"
				}
			}
		}
	}
	return out
}

// Export the seam, not the machinery: an exported identifier must be part of its package's
// contract with the rest of the service. It qualifies one of two ways —
//
//  1. the package names it in its own exported API (a parameter, a result, an exported
//     struct field, an exported interface method), or
//  2. another package selects it — which covers both a seam a caller holds a variable of and
//     an optional-capability interface a caller type-asserts against, neither of which
//     appears in any signature.
//
// Anything else is exported but unreachable: it documents a contract in godoc that no caller
// can enter. See okf/standards/go-style.md.
//
// This is a usage question as much as a structural one, and it is answerable only because the
// module is closed: every package here lives under internal/ or cmd/, so this tree contains
// all the callers there will ever be. The same check on a published library would be wrong.
//
// A package's own tests are not a reason to export anything: they sit in the same package and
// see unexported identifiers already. Tests in a *different* package are a reason, because
// they are a different package — see externalRefs.
//
// Scope: exported types and plain functions. Exported *methods* are deliberately out, and not
// for lack of effort — a method frequently exists only to satisfy an interface (tasks.CloudTasks
// has an Enqueue because tasks.Transport declares one), and no caller names it directly. Telling
// that apart from a genuinely unreachable method means resolving interface satisfaction, which
// needs a type checker rather than the AST this suite is built on. So a method that only its own
// package's tests call is a review question, not a gate failure.
func TestExportedIdentifiersAreReachable(t *testing.T) {
	root := repoRoot(t)
	pkgs := parsePackages(t, root, modulePath(t))
	refs := externalRefs(pkgs)

	dirs := make([]string, 0, len(pkgs))
	for d := range pkgs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	for _, dir := range dirs {
		p := pkgs[dir]
		// package main exports nothing anyone can import; TestNothingImportsCmd covers it.
		if p.name == "main" {
			continue
		}
		surface := exportedSurface(p)
		declared := declaredExports(p)

		names := make([]string, 0, len(declared))
		for n := range declared {
			names = append(names, n)
		}
		sort.Strings(names)

		for _, n := range names {
			if surface[n] || refs[p.path][n] {
				continue
			}
			t.Errorf("%s: exported %s %s is unreachable from outside the package — "+
				"it is in no exported signature and no other package refers to it; unexport it",
				rel(root, dir), declared[n], n)
		}
	}
}

// The initializer of an exported var is implementation, not contract. This pins the
// distinction with synthetic source, because the tree itself does not currently contain an
// exported var built by a local call — so nothing else would notice if it regressed.
func TestInitializersDoNotLaunderUnreachableIdentifiers(t *testing.T) {
	const src = `package p

type Client struct{}

// newClient is unreachable from outside: nothing names it in an exported signature.
func newClient() *Client { return &Client{} }

// Unreachable is the identifier under test — exported, but reachable from nowhere.
func Unreachable() string { return "" }

type Registry struct{}

type Named interface{ Do() }

type Allocation struct{}

type Element struct{}

type Mapped2 struct{}

type Param struct{}

type Returned struct{}

var (
	Default   = newClient()
	Direct    = &Registry{}
	Explicit  Named = nil
	Laundered = Unreachable

	// Type positions the grammar guarantees, each reachable only through this var.
	Allocated = new(Allocation)
	Slice     = make([]Element, 0)
	Mapped    = make(map[string]Mapped2)
	Callback  = func(Param) Returned { return Returned{} }
)
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	surface := exportedSurface(&visPkg{path: "example/p", name: "p", files: []*ast.File{f}})

	// Proved by the AST, so genuinely part of the surface.
	for _, want := range []string{
		"Client",     // newClient's declared result — Default is a *Client
		"Registry",   // named outright by the &Registry{} composite literal
		"Named",      // Explicit's declared type
		"Allocation", // new(T): first argument is a type, per the spec
		"Element",    // make([]T, n): likewise, through the slice type
		"Mapped2",    // make(map[K]V): likewise, through the map type
		"Param",      // a func literal's parameters are reachable via the value
		"Returned",   // and so are its results
	} {
		if !surface[want] {
			t.Errorf("%s should be exported surface", want)
		}
	}

	// Value positions, not type positions: counting these would excuse an unreachable
	// identifier from the check purely because some exported var mentions it.
	for _, notWant := range []string{
		"newClient",   // how Default is built, not what Default is
		"Unreachable", // assigned as a func value; still reachable from nowhere
		"new",         // the builtin itself is not a type
		"make",        // nor is this one
	} {
		if surface[notWant] {
			t.Errorf("%s must not count as exported surface — it is a value, not a type", notWant)
		}
	}
}
