package reviewer

import (
	"testing"

	"automation-agent/internal/githubapi"
)

func TestCommentableLines(t *testing.T) {
	// Two hunks. New side starts at 1 then 12; '-' lines have no head-side line.
	patch := "@@ -1,3 +1,4 @@\n ctx1\n-old\n+new1\n+new2\n ctx2\n@@ -10,1 +12,2 @@\n+added12\n ctx13\n"
	got := commentableLines(patch)
	want := []int{1, 2, 3, 4, 12, 13}
	for _, w := range want {
		if !got[w] {
			t.Errorf("line %d should be commentable; got %v", w, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d commentable lines, want %d: %v", len(got), len(want), got)
	}
}

func TestCommentableLinesMalformed(t *testing.T) {
	for _, patch := range []string{"", "no hunk header\n+stuff", "@@ bad header @@\n+x"} {
		if got := commentableLines(patch); len(got) != 0 {
			t.Errorf("commentableLines(%q) = %v, want empty", patch, got)
		}
	}
}

func TestDiffIndexInDiff(t *testing.T) {
	idx := newDiffIndex([]githubapi.PRFile{{Path: "a.go", Patch: "@@ -1,1 +1,2 @@\n a\n+b\n"}})
	if !idx.inDiff("a.go", 2) {
		t.Error("a.go:2 (added) should be in diff")
	}
	if idx.inDiff("a.go", 9) {
		t.Error("a line outside the hunk must not be in diff")
	}
	if idx.inDiff("b.go", 1) {
		t.Error("an unknown file must not be in diff")
	}
}
