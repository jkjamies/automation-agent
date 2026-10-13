package gate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ticketflow/internal/workspace"
)

func TestReadiness(t *testing.T) {
	good := Ticket{Type: "Story", Description: strings.Repeat("context ", 12) + "\n\nAcceptance criteria:\n- it works"}
	if p := Readiness(good, DefaultReadinessRules); len(p) != 0 {
		t.Errorf("ready ticket rejected: %v", p)
	}
	for name, tc := range map[string]struct {
		t    Ticket
		want string
	}{
		"type":        {Ticket{Type: "Epic", Description: good.Description}, "issue type"},
		"block label": {Ticket{Type: "Bug", Description: good.Description, Labels: []string{"Security"}}, "keeps this ticket"},
		"short":       {Ticket{Type: "Task", Description: "Acceptance criteria: x"}, "under 80"},
		"no AC":       {Ticket{Type: "Task", Description: strings.Repeat("words ", 30)}, "no acceptance criteria"},
	} {
		p := strings.Join(Readiness(tc.t, DefaultReadinessRules), "\n")
		if !strings.Contains(p, tc.want) {
			t.Errorf("%s: problems %q do not mention %q", name, p, tc.want)
		}
	}
	gwt := Ticket{Type: "Bug", Description: strings.Repeat("context ", 12) + "\nGiven a user\nWhen they log in\nThen it works"}
	if p := Readiness(gwt, DefaultReadinessRules); len(p) != 0 {
		t.Errorf("Given/When/Then rejected: %v", p)
	}
	if p := Readiness(Ticket{Type: "Anything", Description: "- [ ] x"}, ReadinessRules{}); len(p) != 0 {
		t.Errorf("empty rules should only require acceptance criteria: %v", p)
	}
}

func writePolicy(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if content != "" {
		if err := os.WriteFile(filepath.Join(dir, RepoPolicyFile), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadRepoPolicy(t *testing.T) {
	p, err := LoadRepoPolicy(writePolicy(t, `{"checks":[{"name":"test","run":["go","test","./..."]}],"forbidden":["db/migrations/**"],"max_files":5}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Checks) != 1 || p.Checks[0].Name != "test" || p.MaxFiles != 5 || p.Forbidden[0] != "db/migrations/**" {
		t.Errorf("policy = %+v", p)
	}
	if _, err := LoadRepoPolicy(writePolicy(t, "")); !errors.Is(err, ErrNoPolicy) {
		t.Errorf("missing file: err = %v", err)
	}
	for _, bad := range []string{`{`, `{"checks":[]}`, `{"checks":[{"name":"x"}]}`} {
		if _, err := LoadRepoPolicy(writePolicy(t, bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestDiffLimitsMerge(t *testing.T) {
	l := DiffLimits{MaxFiles: 10, MaxChangedLines: 0, Forbidden: []string{"a/**"}}.Merge(RepoPolicy{MaxFiles: 20, MaxChangedLines: 300, Forbidden: []string{"b/**"}})
	if l.MaxFiles != 10 || l.MaxChangedLines != 300 {
		t.Errorf("limits = %+v; a repo may tighten but not loosen", l)
	}
	for _, want := range []string{".github/workflows/**", "a/**", "b/**"} {
		if _, ok := matchAny(l.Forbidden, strings.ReplaceAll(want, "**", "x/y")); !ok {
			t.Errorf("merged forbidden list lacks %s", want)
		}
	}
	if got := tighter(5, 3); got != 3 {
		t.Errorf("tighter(5,3) = %d", got)
	}
}

func TestDiff(t *testing.T) {
	limits := DiffLimits{MaxFiles: 2, MaxChangedLines: 10}.Merge(RepoPolicy{})
	ok := workspace.Diff{Files: []workspace.FileChange{{Path: "main.go", Status: "M", Added: 3, Deleted: 1}}, Patch: "+x"}
	if p := Diff(ok, limits); len(p) != 0 {
		t.Errorf("clean diff rejected: %v", p)
	}
	for name, tc := range map[string]struct {
		d    workspace.Diff
		want string
	}{
		"empty":   {workspace.Diff{}, "empty"},
		"files":   {workspace.Diff{Files: []workspace.FileChange{{Path: "a"}, {Path: "b"}, {Path: "c"}}}, "3 files changed"},
		"lines":   {workspace.Diff{Files: []workspace.FileChange{{Path: "a", Added: 8, Deleted: 8}}}, "16 lines changed"},
		"ci":      {workspace.Diff{Files: []workspace.FileChange{{Path: ".github/workflows/ci.yml", Status: "M"}}}, "protected"},
		"rename":  {workspace.Diff{Files: []workspace.FileChange{{Path: "ok.yml", OldPath: ".github/workflows/ci.yml", Status: "R"}}}, "protected"},
		"policy":  {workspace.Diff{Files: []workspace.FileChange{{Path: RepoPolicyFile}}}, "protected"},
		"nested":  {workspace.Diff{Files: []workspace.FileChange{{Path: "deploy/certs/server.pem"}}}, "protected"},
		"test":    {workspace.Diff{Files: []workspace.FileChange{{Path: "pkg/x_test.go", Status: "D"}}}, "test file"},
		"secret":  {workspace.Diff{Files: []workspace.FileChange{{Path: "a"}}, Patch: "+++ b/a\n+key = \"AKIAABCDEFGHIJKLMNOP\""}, "AWS access key"},
		"privkey": {workspace.Diff{Files: []workspace.FileChange{{Path: "a"}}, Patch: "+-----BEGIN RSA PRIVATE KEY-----"}, "private key"},
	} {
		p := strings.Join(Diff(tc.d, limits), "\n")
		if !strings.Contains(p, tc.want) {
			t.Errorf("%s: problems %q do not mention %q", name, p, tc.want)
		}
	}
	removed := workspace.Diff{Files: []workspace.FileChange{{Path: "a"}}, Patch: "-token = ghp_abcdefghijklmnopqrstuvwxyz0123456789AB"}
	if p := Diff(removed, limits); len(p) != 0 {
		t.Errorf("removing a secret was blocked: %v", p)
	}
}

func TestGlobMatch(t *testing.T) {
	for _, tc := range []struct {
		pat, name string
		want      bool
	}{
		{".github/workflows/**", ".github/workflows/ci.yml", true},
		{".github/workflows/**", ".github/workflows/sub/x.yml", true},
		{".github/workflows/**", ".github/actions/x.yml", false},
		{"**/*.pem", "a.pem", true},
		{"**/*.pem", "a/b/c.pem", true},
		{"**/.env", "svc/.env", true},
		{"CODEOWNERS", "docs/CODEOWNERS", false},
		{"src/*.go", "src/a/b.go", false},
		{"[", "[", false},
	} {
		if got := globMatch(tc.pat, tc.name); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pat, tc.name, got, tc.want)
		}
	}
}

func TestIsTestFile(t *testing.T) {
	for p, want := range map[string]bool{
		"a/b_test.go": true, "web/x.spec.ts": true, "web/x.test.jsx": true, "app/FooTest.kt": true,
		"tests/test_api.py": true, "api_test.py": true, "main.go": false, "testdata/in.txt": false,
	} {
		if got := isTestFile(p); got != want {
			t.Errorf("isTestFile(%q) = %v", p, got)
		}
	}
}
