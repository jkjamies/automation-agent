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
	// path-escape attempts are clamped to the repo root, not errored
	clamped, err := listDirEntries(dir, "../..")
	if err != nil || !strings.Contains(strings.Join(clamped, ","), "f.go") {
		t.Errorf("escape should clamp to root: %v %v", clamped, err)
	}
}

func TestRepoTools(t *testing.T) {
	tools, err := repoTools(t.TempDir())
	if err != nil || len(tools) != 2 {
		t.Fatalf("repoTools = %d tools, %v", len(tools), err)
	}
}
