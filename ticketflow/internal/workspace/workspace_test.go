package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(t, os.WriteFile(path, []byte(content), 0o644))
}

func remote(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	r, seed := filepath.Join(root, "r.git"), filepath.Join(root, "seed")
	git(t, root, "init", "--bare", r)
	git(t, root, "init", seed)
	for p, c := range map[string]string{"a.txt": "one\ntwo\n", "old/name.txt": "rename me\n", "gone_test.go": "package x\n", "bin.dat": "\x00\x01"} {
		write(t, filepath.Join(seed, p), c)
	}
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-m", "seed")
	git(t, seed, "push", r, "HEAD:refs/heads/main")
	return r
}

func TestCloneDiffCommitPush(t *testing.T) {
	ctx := context.Background()
	r := remote(t)
	dir := filepath.Join(t.TempDir(), "ws")
	w, err := Clone(ctx, r, "main", dir, "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	if cfg, _ := os.ReadFile(filepath.Join(dir, ".git", "config")); strings.Contains(string(cfg), "secret-token") {
		t.Error("token persisted in .git/config")
	}
	if exists, err := w.RemoteBranchExists(ctx, "feature"); err != nil || exists {
		t.Fatalf("RemoteBranchExists = %v, %v", exists, err)
	}
	if err := w.CreateBranch(ctx, "feature"); err != nil {
		t.Fatal(err)
	}

	write(t, filepath.Join(dir, "a.txt"), "one\n2\nthree\n")
	write(t, filepath.Join(dir, "new.txt"), "fresh\n")
	write(t, filepath.Join(dir, "bin.dat"), "\x00\x02")
	must(t, os.Remove(filepath.Join(dir, "gone_test.go")))
	must(t, os.MkdirAll(filepath.Join(dir, "new"), 0o755))
	must(t, os.Rename(filepath.Join(dir, "old/name.txt"), filepath.Join(dir, "new/name.txt")))
	// A hook planted by an agent must never run.
	write(t, filepath.Join(dir, ".git", "hooks", "pre-commit"), "#!/bin/sh\ntouch HOOK_RAN\nexit 1\n")
	must(t, os.Chmod(filepath.Join(dir, ".git", "hooks", "pre-commit"), 0o755))

	d, err := w.Diff(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]FileChange{}
	for _, f := range d.Files {
		got[f.Path] = f
	}
	if f := got["a.txt"]; f.Status != "M" || f.Added != 2 || f.Deleted != 1 {
		t.Errorf("a.txt = %+v", f)
	}
	if f := got["new.txt"]; f.Status != "A" || f.Added != 1 {
		t.Errorf("new.txt = %+v", f)
	}
	if f := got["gone_test.go"]; f.Status != "D" {
		t.Errorf("gone_test.go = %+v", f)
	}
	if f := got["new/name.txt"]; f.Status != "R" || f.OldPath != "old/name.txt" {
		t.Errorf("rename = %+v", f)
	}
	if f := got["bin.dat"]; f.Added != -1 {
		t.Errorf("binary = %+v", f)
	}
	if d.ChangedLines() != 5 || d.Empty() || !strings.Contains(d.Patch, "+fresh") {
		t.Errorf("changed lines = %d, patch has new file: %v", d.ChangedLines(), strings.Contains(d.Patch, "+fresh"))
	}

	if err := w.Commit(ctx, "msg", Author{Name: "bot", Email: "bot@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "HOOK_RAN")); err == nil {
		t.Error("a planted git hook ran")
	}
	if err := w.Push(ctx, "feature"); err != nil {
		t.Fatal(err)
	}
	if exists, _ := w.RemoteBranchExists(ctx, "feature"); !exists {
		t.Error("pushed branch not on the remote")
	}
	if author := git(t, dir, "log", "-1", "--format=%an <%ae>"); strings.TrimSpace(author) != "bot <bot@example.com>" {
		t.Errorf("author = %q", author)
	}
}

func TestCloneFailureRedactsToken(t *testing.T) {
	_, err := Clone(context.Background(), "/nonexistent/secret-token/repo", "main", t.TempDir(), "secret-token")
	if err == nil {
		t.Fatal("clone of a missing repo succeeded")
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Errorf("error leaks the token: %v", err)
	}
}

func TestRenamedPath(t *testing.T) {
	for in, want := range map[string]string{
		"a.txt":                 "a.txt",
		"old.txt => new.txt":    "new.txt",
		"dir/{old => new}/f.go": "dir/new/f.go",
		"{a => }/f.go":          "f.go",
		"src/{ => sub}/f.go":    "src/sub/f.go",
	} {
		if got := renamedPath(in); got != want {
			t.Errorf("renamedPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRun(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	r, err := Run(ctx, dir, []string{"sh", "-c", "echo out; echo err >&2; echo $EXTRA"}, time.Second, "EXTRA=yes")
	if err != nil || !r.Passed() || !strings.Contains(r.Output, "out") || !strings.Contains(r.Output, "err") || !strings.Contains(r.Output, "yes") {
		t.Errorf("Run = %+v, %v", r, err)
	}
	r, _ = Run(ctx, dir, []string{"sh", "-c", "exit 7"}, time.Second)
	if r.Passed() || r.ExitCode != 7 {
		t.Errorf("exit = %+v", r)
	}
	r, _ = Run(ctx, dir, []string{"sleep", "5"}, 50*time.Millisecond)
	if !r.TimedOut || r.Passed() {
		t.Errorf("timeout = %+v", r)
	}
	if _, err := Run(ctx, dir, nil, time.Second); err == nil {
		t.Error("empty argv ran")
	}
	if _, err := Run(ctx, dir, []string{"/does/not/exist"}, time.Second); err == nil {
		t.Error("missing binary ran")
	}
}

func TestTailBuffer(t *testing.T) {
	var b tailBuffer
	chunk := strings.Repeat("x", 1000)
	for range 50 {
		_, _ = b.Write([]byte(chunk))
	}
	_, _ = b.Write([]byte("END"))
	if s := b.String(); len(s) != OutputLimit || !strings.HasSuffix(s, "END") {
		t.Errorf("tail len = %d, suffix ok = %v", len(s), strings.HasSuffix(s, "END"))
	}
}
