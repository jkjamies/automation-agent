package repotools

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"

	"ticketflow/internal/gate"
)

func repo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for p, c := range map[string]string{
		"main.go":         "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n",
		"pkg/util.go":     "package pkg\n\n// Add adds.\nfunc Add(a, b int) int { return a + b }\n",
		".git/config":     "[core]\n",
		"node_modules/x":  "func Add\n",
		"bin/blob.bin":    "func Add\x00\x01",
		"docs/AGENTS.md":  "rules\n",
		"dup.txt":         "same\nsame\n",
		"pkg/sub/deep.go": "package sub\n",
	} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func ctx(t *testing.T) agent.Context {
	return &agent.StrictContextMock{Ctx: t.Context()}
}

func TestResolve(t *testing.T) {
	root := repo(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"/etc/passwd", "../x", "a/../../x", ".git/hooks/pre-commit", ".git", "escape/file", "escape"} {
		if _, err := Resolve(root, bad); !errors.Is(err, ErrOutsideRoot) {
			t.Errorf("Resolve(%q) err = %v, want ErrOutsideRoot", bad, err)
		}
	}
	for _, good := range []string{"", ".", "main.go", "pkg/../main.go", "new/dir/file.go", ".github/x"} {
		if _, err := Resolve(root, good); err != nil {
			t.Errorf("Resolve(%q) = %v", good, err)
		}
	}
}

func TestReadOnlyTools(t *testing.T) {
	root := repo(t)
	tl := &tools{root: root}
	c := ctx(t)

	ls, _ := tl.listDir(c, listArgs{Path: "."})
	if strings.Contains(strings.Join(ls.Entries, " "), ".git") || !strings.Contains(strings.Join(ls.Entries, " "), "pkg/") {
		t.Errorf("list_dir = %v", ls.Entries)
	}
	if res, _ := tl.listDir(c, listArgs{Path: "nope"}); res.Error == "" {
		t.Error("list_dir on a missing dir reported no error")
	}
	if res, _ := tl.listDir(c, listArgs{Path: "../"}); res.Error == "" {
		t.Error("list_dir outside the root reported no error")
	}

	rd, _ := tl.readFile(c, readArgs{Path: "main.go", StartLine: 3, EndLine: 4})
	if rd.Content != "func main() {\n\tprintln(\"hi\")\n" || rd.TotalLines != 6 {
		t.Errorf("read_file range = %q (%d lines)", rd.Content, rd.TotalLines)
	}
	if res, _ := tl.readFile(c, readArgs{Path: "main.go", StartLine: 5, EndLine: 2}); res.Error == "" {
		t.Error("inverted range reported no error")
	}
	if res, _ := tl.readFile(c, readArgs{Path: ".git/config"}); res.Error == "" {
		t.Error("read_file reached .git")
	}

	sr, _ := tl.search(c, searchArgs{Pattern: `func Add`})
	if len(sr.Matches) != 1 || !strings.HasPrefix(sr.Matches[0], "pkg/util.go:4:") {
		t.Errorf("search = %v (node_modules and binaries must be skipped)", sr.Matches)
	}
	if sr, _ := tl.search(c, searchArgs{Pattern: `package`, Path: "pkg/sub"}); len(sr.Matches) != 1 {
		t.Errorf("scoped search = %v", sr.Matches)
	}
	if res, _ := tl.search(c, searchArgs{Pattern: `(`}); res.Error == "" {
		t.Error("invalid regexp reported no error")
	}
}

func TestWriteTools(t *testing.T) {
	root := repo(t)
	tl := &tools{root: root}
	c := ctx(t)

	if r, _ := tl.writeFile(c, writeArgs{Path: "new/a.go", Content: "package a\n"}); !r.OK {
		t.Fatalf("write_file: %s", r.Error)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "new/a.go")); string(b) != "package a\n" {
		t.Errorf("written content = %q", b)
	}
	if r, _ := tl.writeFile(c, writeArgs{Path: ".git/hooks/pre-commit", Content: "#!/bin/sh\n"}); r.OK {
		t.Error("write_file planted a git hook")
	}

	if r, _ := tl.editFile(c, editArgs{Path: "pkg/util.go", OldText: "a + b", NewText: "b + a"}); !r.OK {
		t.Fatalf("edit_file: %s", r.Error)
	}
	for name, a := range map[string]editArgs{
		"missing":   {Path: "pkg/util.go", OldText: "nope", NewText: "x"},
		"ambiguous": {Path: "dup.txt", OldText: "same", NewText: "x"},
		"empty":     {Path: "dup.txt", OldText: "", NewText: "x"},
		"no file":   {Path: "nope.go", OldText: "x", NewText: "y"},
	} {
		if r, _ := tl.editFile(c, a); r.OK || r.Error == "" {
			t.Errorf("edit_file %s succeeded", name)
		}
	}

	if r, _ := tl.deleteFile(c, deleteArgs{Path: "dup.txt"}); !r.OK {
		t.Errorf("delete_file: %s", r.Error)
	}
	if r, _ := tl.deleteFile(c, deleteArgs{Path: "."}); r.OK {
		t.Error("delete_file removed the root")
	}
}

func TestRunCheck(t *testing.T) {
	root := repo(t)
	tl := &tools{root: root, timeout: 10 * time.Second, checks: []gate.Check{
		{Name: "pass", Run: []string{"sh", "-c", "echo fine"}},
		{Name: "fail", Run: []string{"sh", "-c", "echo broken >&2; exit 3"}},
		{Name: "secret", Run: []string{"sh", "-c", "echo ${GITHUB_TOKEN:-unset}"}},
	}}
	c := ctx(t)
	if r, _ := tl.runCheck(c, checkArgs{Name: "pass"}); !r.Passed || !strings.Contains(r.Output, "fine") {
		t.Errorf("pass = %+v", r)
	}
	if r, _ := tl.runCheck(c, checkArgs{Name: "fail"}); r.Passed || r.ExitCode != 3 || !strings.Contains(r.Output, "broken") {
		t.Errorf("fail = %+v", r)
	}
	t.Setenv("GITHUB_TOKEN", "leak")
	if r, _ := tl.runCheck(c, checkArgs{Name: "secret"}); strings.Contains(r.Output, "leak") {
		t.Errorf("a check saw the orchestrator's environment: %q", r.Output)
	}
	if r, _ := tl.runCheck(c, checkArgs{Name: "rm -rf /"}); r.Error == "" {
		t.Error("run_check ran an undeclared command")
	}
}

func TestToolsets(t *testing.T) {
	ro, err := ReadOnly(t.TempDir())
	if err != nil || len(ro) != 3 {
		t.Fatalf("ReadOnly = %d tools, %v", len(ro), err)
	}
	rw, err := ReadWrite(t.TempDir(), []gate.Check{{Name: "test", Run: []string{"true"}}}, time.Second)
	if err != nil || len(rw) != 7 {
		t.Fatalf("ReadWrite = %d tools, %v", len(rw), err)
	}
	names := map[string]bool{}
	for _, tl := range rw {
		names[tl.Name()] = true
	}
	for _, n := range []string{"list_dir", "read_file", "search", "write_file", "edit_file", "delete_file", "run_check"} {
		if !names[n] {
			t.Errorf("missing tool %s", n)
		}
	}
}
