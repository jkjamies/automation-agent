package reviewer

import (
	"testing"

	"automation-agent/internal/githubapi"
)

func TestFileFilterExcludes(t *testing.T) {
	f := newFileFilter([]string{"go.sum", "*.min.js", "vendor/**", "*.png"})
	cases := map[string]bool{
		"go.sum":                 true,  // basename lockfile
		"internal/app/go.sum":    true,  // basename match anywhere in the tree
		"web/bundle.min.js":      true,  // basename glob
		"vendor/x/y/z.go":        true,  // path prefix (** crosses separators)
		"assets/logo.png":        true,  // binary basename
		"internal/app/server.go": false, // real source, kept
		"vendormix/util.go":      false, // "vendor/**" must not match "vendormix/..."
		"app.min.js.go":          false, // "*.min.js" anchored at end, no match
	}
	for path, want := range cases {
		if got := f.excluded(path); got != want {
			t.Errorf("excluded(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestFileFilterApplyAndSize(t *testing.T) {
	f := newFileFilter([]string{"go.sum", "vendor/**"})
	files := []githubapi.PRFile{
		{Path: "main.go", Patch: "12345"},           // kept, 5 bytes
		{Path: "go.sum", Patch: "xxxxxxxxxxxxxxxx"}, // excluded
		{Path: "vendor/lib/x.go", Patch: "yyyy"},    // excluded
		{Path: "util.go", Patch: "123"},             // kept, 3 bytes
	}
	kept, bytes := f.apply(files)
	if len(kept) != 2 {
		t.Fatalf("kept %d files, want 2", len(kept))
	}
	if kept[0].Path != "main.go" || kept[1].Path != "util.go" {
		t.Errorf("kept = %+v", kept)
	}
	if bytes != 8 {
		t.Errorf("diff bytes = %d, want 8 (filtered set only)", bytes)
	}
}

func TestFileFilterOmittedPatchCharged(t *testing.T) {
	// GitHub omits the patch for oversized text files but still reports line counts. Such a
	// file must be charged conservatively (avgDiffLineBytes per changed line), not undercounted
	// as zero, so a huge PR cannot slip past the byte cap. A binary file (no patch, no line
	// counts) costs nothing.
	f := newFileFilter(nil)
	files := []githubapi.PRFile{
		{Path: "small.go", Patch: "123"},                               // 3 bytes (exact)
		{Path: "huge.go", Patch: "", Additions: 4000, Deletions: 1000}, // omitted text diff
		{Path: "logo.png", Patch: "", Additions: 0, Deletions: 0},      // binary, no charge
	}
	kept, bytes := f.apply(files)
	if len(kept) != 3 {
		t.Fatalf("kept %d files, want 3", len(kept))
	}
	want := 3 + 5000*avgDiffLineBytes
	if bytes != want {
		t.Errorf("diff bytes = %d, want %d (exact patch + estimated omitted diff, binary free)", bytes, want)
	}
}

func TestFileFilterBlankEntries(t *testing.T) {
	// Blank entries are tolerated (a trailing comma in the env); nothing is excluded.
	f := newFileFilter([]string{"", "   "})
	if f.excluded("anything.go") {
		t.Error("an all-blank filter should exclude nothing")
	}
}

func TestFileFilterMetacharsAreLiteral(t *testing.T) {
	// A glob full of regexp metacharacters must compile and match literally — never as a
	// regexp (e.g. the '.' is a literal dot, not "any char").
	f := newFileFilter([]string{"a.b(c)[d]+e.go"})
	if !f.excluded("a.b(c)[d]+e.go") {
		t.Error("metachar glob should match its literal path")
	}
	if f.excluded("aXbXcXdXeXgo") {
		t.Error("metachar glob must not match as a regexp")
	}
}
