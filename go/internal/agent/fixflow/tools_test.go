package fixflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFileAndSafeJoin(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := ReadFile(dir, "a.txt")
	if err != nil || c != "hello" {
		t.Fatalf("read = %q, %v", c, err)
	}
	if _, err := ReadFile(dir, "../../etc/passwd"); err == nil {
		t.Error("path escape should be refused")
	}
}

func TestListDirEntries(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "f.go"), []byte("x"), 0o644)

	ents, err := listDirEntries(dir, ".")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(ents, ",")
	if !strings.Contains(got, "f.go") || !strings.Contains(got, "sub/") {
		t.Errorf("entries = %v (want f.go + sub/)", ents)
	}
	if strings.Contains(got, ".git") {
		t.Errorf(".git should be hidden: %v", ents)
	}
	// path-escape attempts are rejected
	if _, err := listDirEntries(dir, "../.."); err == nil {
		t.Error("escape should be rejected")
	}
}

func TestSafeJoinRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	for _, bad := range []string{"../escape", "../../etc/cron.d/x", "/etc/passwd", "a/../../b"} {
		if _, err := safeJoin(root, bad); err == nil {
			t.Errorf("safeJoin should reject %q (path traversal / absolute)", bad)
		}
	}
	for _, ok := range []string{"a.go", "sub/dir/b_test.go", "."} {
		if _, err := safeJoin(root, ok); err != nil {
			t.Errorf("safeJoin rejected a safe path %q: %v", ok, err)
		}
	}
}

func TestSafeJoinRejectsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlinked directory inside the checkout that points outside it.
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := safeJoin(root, "link/x.txt"); err == nil {
		t.Error("safeJoin should reject a path that escapes via a symlinked directory")
	}
}

func TestSafeJoinRejectsDanglingSymlink(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlink that exists but points to a non-existent path outside the root; writing
	// through it would escape, so it must be rejected (not treated as a new in-repo file).
	if err := os.Symlink(filepath.Join(base, "ghost"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := safeJoin(root, "link"); err == nil {
		t.Error("safeJoin should reject a dangling symlink")
	}
}

func TestRepoTools(t *testing.T) {
	tools, err := repoTools(t.TempDir())
	if err != nil || len(tools) != 2 {
		t.Fatalf("repoTools = %d tools, %v", len(tools), err)
	}
}
