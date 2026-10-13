package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"ticketflow/internal/fakellm"
	"ticketflow/internal/gate"
	"ticketflow/internal/github"
	"ticketflow/internal/harness"
	"ticketflow/internal/jira"
)

const readyDescription = `Users need a feature file so the release script can find it.

## Acceptance criteria
- [ ] feature.txt exists at the repository root
- [ ] it contains the word ok`

func readyIssue() jira.Issue {
	return jira.Issue{
		Key: "PROJ-7", Project: "PROJ", URL: "https://jira.example/browse/PROJ-7", Type: "Story",
		Summary: "Add the feature file", Description: readyDescription, Labels: []string{"agent-ready"},
	}
}

func planJSON(ready bool) string {
	b, _ := json.Marshal(map[string]any{
		"ready": ready, "questions": []string{"Which directory?"}, "plan": "Create feature.txt.",
		"files": []string{"feature.txt"}, "test_plan": "The check greps it.", "risk": "low",
	})
	return string(b)
}

const (
	approve = `{"verdict":"approve","summary":"Looks right.","findings":[]}`
	revise  = "```json\n{\"verdict\":\"revise\",\"summary\":\"Missing newline.\",\"findings\":[{\"severity\":\"blocking\",\"file\":\"feature.txt\",\"issue\":\"add a trailing newline\"}]}\n```"
)

// fakeTracker records what the pipeline told Jira.
type fakeTracker struct {
	mu          sync.Mutex
	issue       jira.Issue
	comments    []string
	added       []string
	removed     []string
	transitions []string
}

func (f *fakeTracker) Issue(_ context.Context, key string) (jira.Issue, error) {
	if key != f.issue.Key {
		return jira.Issue{}, errors.New("no such issue")
	}
	return f.issue, nil
}

func (f *fakeTracker) Comment(_ context.Context, _, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, text)
	return nil
}

func (f *fakeTracker) EditLabels(_ context.Context, _ string, add, remove []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, add...)
	f.removed = append(f.removed, remove...)
	return nil
}

func (f *fakeTracker) Transition(_ context.Context, _, name string) error {
	f.transitions = append(f.transitions, name)
	return nil
}

type fakeHost struct {
	prs    []github.NewPR
	labels []string
}

func (f *fakeHost) DefaultBranch(context.Context, string) (string, error) { return "main", nil }

func (f *fakeHost) OpenPR(_ context.Context, repo string, pr github.NewPR) (github.PR, error) {
	f.prs = append(f.prs, pr)
	return github.PR{Number: 42, URL: "https://github.example/" + repo + "/pull/42"}, nil
}

func (f *fakeHost) AddLabels(_ context.Context, _ string, _ int, labels []string) error {
	f.labels = append(f.labels, labels...)
	return nil
}

// scriptHarness writes files[attempt-1] (the last entry repeating) into the checkout.
type scriptHarness struct {
	files     []map[string]string
	tasks     []harness.Task
	failFirst bool
}

func (h *scriptHarness) Name() string { return "script" }

func (h *scriptHarness) Implement(_ context.Context, t harness.Task) (harness.Result, error) {
	h.tasks = append(h.tasks, t)
	i := min(t.Attempt-1, len(h.files)-1)
	for p, content := range h.files[i] {
		full := filepath.Join(t.Dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return harness.Result{}, err
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return harness.Result{}, err
		}
	}
	if h.failFirst && t.Attempt == 1 {
		return harness.Result{Summary: "partial"}, errors.New("model-call budget exhausted")
	}
	return harness.Result{Summary: "wrote files"}, nil
}

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

// newRemote creates a bare repository seeded with files on main and returns its path.
func newRemote(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	git(t, root, "init", "--bare", remote)
	git(t, root, "init", seed)
	for p, c := range files {
		full := filepath.Join(seed, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-m", "seed")
	git(t, seed, "push", remote, "HEAD:refs/heads/main")
	return remote
}

var quiet = slog.New(slog.DiscardHandler)

const policy = `{"checks":[{"name":"has-ok","run":["sh","-c","grep -q ok feature.txt"]}],"instructions":"Keep it small."}`

type fixture struct {
	remote   string
	tracker  *fakeTracker
	host     *fakeHost
	harness  *scriptHarness
	planner  *fakellm.Model
	reviewer *fakellm.Model
	settings Settings
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	remote := newRemote(t, map[string]string{"README.md": "# demo\n", gate.RepoPolicyFile: policy})
	return &fixture{
		remote:   remote,
		tracker:  &fakeTracker{issue: readyIssue()},
		host:     &fakeHost{},
		harness:  &scriptHarness{files: []map[string]string{{"feature.txt": "ok\n"}}},
		planner:  &fakellm.Model{Replies: []string{planJSON(true)}, ToolCall: &fakellm.Call{Name: "list_dir", Args: map[string]any{"path": "."}}},
		reviewer: &fakellm.Model{Replies: []string{approve}},
		settings: Settings{
			TriggerLabel: "agent-ready", ClaimLabel: "agent-in-progress", NeedsHumanLabel: "agent-needs-human",
			ReviewTransition: "In Review", ProjectRepos: map[string]string{"PROJ": "acme/app"},
			PRLabel: "agent-authored", DraftPRs: true, MaxAttempts: 2, MaxRevisions: 1,
			Limits: gate.DiffLimits{MaxFiles: 10, MaxChangedLines: 100}, CheckTimeout: time.Minute,
			WorkRoot: t.TempDir(), CloneURL: func(string) string { return remote },
		},
	}
}

func (f *fixture) run(t *testing.T, opts RunOptions) Outcome {
	t.Helper()
	p, err := New(Deps{Tracker: f.tracker, Host: f.host, Harness: f.harness, Planner: f.planner, Reviewer: f.reviewer, Settings: f.settings, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := p.Run(ctx, f.tracker.issue.Key, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return out
}

func (f *fixture) remoteHas(t *testing.T, branch, file string) bool {
	t.Helper()
	cmd := exec.Command("git", "--git-dir", f.remote, "cat-file", "-e", branch+":"+file)
	return cmd.Run() == nil
}

func TestRunPublishesAPullRequest(t *testing.T) {
	f := newFixture(t)
	out := f.run(t, RunOptions{})

	if out.Status != StatusPublished {
		t.Fatalf("status = %s (%s %v), want published", out.Status, out.Reason, out.Details)
	}
	wantBranch := "ticketflow/PROJ-7-add-the-feature-file"
	if out.Branch != wantBranch || !f.remoteHas(t, wantBranch, "feature.txt") {
		t.Errorf("branch %q with feature.txt not pushed (outcome branch %q)", wantBranch, out.Branch)
	}
	if len(f.host.prs) != 1 {
		t.Fatalf("opened %d PRs, want 1", len(f.host.prs))
	}
	pr := f.host.prs[0]
	if pr.Head != wantBranch || pr.Base != "main" || !pr.Draft || !strings.HasPrefix(pr.Title, "[PROJ-7] ") {
		t.Errorf("PR = %+v", pr)
	}
	if !slices.Equal(f.host.labels, []string{"agent-authored"}) {
		t.Errorf("PR labels = %v", f.host.labels)
	}
	if !slices.Contains(f.tracker.added, "agent-in-progress") || !slices.Contains(f.tracker.removed, "agent-ready") {
		t.Errorf("ticket not claimed: added %v removed %v", f.tracker.added, f.tracker.removed)
	}
	if len(f.tracker.comments) != 1 || !strings.Contains(f.tracker.comments[0], out.PRURL) {
		t.Errorf("Jira comments = %q, want one linking %s", f.tracker.comments, out.PRURL)
	}
	if !slices.Equal(f.tracker.transitions, []string{"In Review"}) {
		t.Errorf("transitions = %v", f.tracker.transitions)
	}
	if got := len(f.planner.ToolResponses()); got != 1 {
		t.Errorf("planner saw %d tool responses, want 1 (its list_dir call)", got)
	}
}

func TestRunRetriesWithFeedbackThenPublishes(t *testing.T) {
	f := newFixture(t)
	f.harness = &scriptHarness{files: []map[string]string{{"feature.txt": "nope\n"}, {"feature.txt": "ok\n"}}, failFirst: true}
	out := f.run(t, RunOptions{})

	if out.Status != StatusPublished {
		t.Fatalf("status = %s (%s), want published", out.Status, out.Reason)
	}
	if len(f.harness.tasks) != 2 {
		t.Fatalf("harness ran %d times, want 2", len(f.harness.tasks))
	}
	fb := f.harness.tasks[1].Feedback
	if !strings.Contains(fb, "has-ok") || !strings.Contains(fb, "harness stopped with an error") {
		t.Errorf("second attempt feedback does not explain the failure: %q", fb)
	}
}

func TestRunEscalatesWhenChecksNeverPass(t *testing.T) {
	f := newFixture(t)
	f.harness = &scriptHarness{files: []map[string]string{{"feature.txt": "nope\n"}}}
	out := f.run(t, RunOptions{})

	if out.Status != StatusEscalated {
		t.Fatalf("status = %s, want escalated", out.Status)
	}
	if len(f.harness.tasks) != f.settings.MaxAttempts {
		t.Errorf("harness ran %d times, want MaxAttempts=%d", len(f.harness.tasks), f.settings.MaxAttempts)
	}
	if len(f.host.prs) != 0 {
		t.Errorf("opened a PR for a change that never passed")
	}
	if !slices.Contains(f.tracker.added, "agent-needs-human") {
		t.Errorf("needs-human label not added: %v", f.tracker.added)
	}
}

func TestRunRejectsProtectedPaths(t *testing.T) {
	f := newFixture(t)
	f.harness = &scriptHarness{files: []map[string]string{{"feature.txt": "ok\n", ".github/workflows/ci.yml": "on: push\n"}}}
	out := f.run(t, RunOptions{})

	if out.Status != StatusEscalated {
		t.Fatalf("status = %s, want escalated", out.Status)
	}
	if !strings.Contains(strings.Join(out.Details, "\n"), ".github/workflows/ci.yml") {
		t.Errorf("details do not name the protected path: %v", out.Details)
	}
}

func TestRunSendsBlockingReviewFindingsBack(t *testing.T) {
	f := newFixture(t)
	f.reviewer.Replies = []string{revise, approve}
	out := f.run(t, RunOptions{})

	if out.Status != StatusPublished {
		t.Fatalf("status = %s (%s), want published", out.Status, out.Reason)
	}
	if len(f.harness.tasks) != 2 || !strings.Contains(f.harness.tasks[1].Feedback, "trailing newline") {
		t.Errorf("revision did not reach the harness: %d tasks", len(f.harness.tasks))
	}
	if f.reviewer.Turns() != 2 {
		t.Errorf("reviewer ran %d times, want 2", f.reviewer.Turns())
	}
}

func TestRunPublishesWithOpenFindingsPastRevisionBudget(t *testing.T) {
	f := newFixture(t)
	f.settings.MaxRevisions = 0
	f.reviewer.Replies = []string{revise}
	out := f.run(t, RunOptions{})

	if out.Status != StatusPublished {
		t.Fatalf("status = %s, want published", out.Status)
	}
	if !strings.Contains(f.host.prs[0].Body, "trailing newline") {
		t.Errorf("open finding missing from the PR body")
	}
}

func TestRunBouncesUnreadyTicketsWithoutCloning(t *testing.T) {
	f := newFixture(t)
	f.tracker.issue.Description = "make it better"
	f.settings.CloneURL = func(string) string { t.Error("cloned an unready ticket's repo"); return f.remote }
	out := f.run(t, RunOptions{})

	if out.Status != StatusBounced {
		t.Fatalf("status = %s, want bounced", out.Status)
	}
	if len(f.tracker.comments) != 1 || !slices.Contains(f.tracker.removed, "agent-ready") {
		t.Errorf("bounce not reported: comments %d, removed %v", len(f.tracker.comments), f.tracker.removed)
	}
	if f.planner.Turns() != 0 {
		t.Errorf("planner ran for a ticket the gate rejected")
	}
}

func TestRunBouncesWhenPlannerHasQuestions(t *testing.T) {
	f := newFixture(t)
	f.planner.Replies = []string{planJSON(false)}
	out := f.run(t, RunOptions{})

	if out.Status != StatusBounced || !slices.Equal(out.Details, []string{"Which directory?"}) {
		t.Fatalf("outcome = %+v, want bounced with the planner's question", out)
	}
	if len(f.harness.tasks) != 0 {
		t.Errorf("harness ran for an unready ticket")
	}
}

func TestRunEscalatesOnUnusablePlan(t *testing.T) {
	f := newFixture(t)
	f.planner.Replies = []string{"I think we should just do it."}
	if out := f.run(t, RunOptions{}); out.Status != StatusEscalated {
		t.Fatalf("status = %s, want escalated", out.Status)
	}
}

func TestRunEscalatesWhenRepoHasNoPolicy(t *testing.T) {
	f := newFixture(t)
	bare := newRemote(t, map[string]string{"README.md": "# no policy\n"})
	f.settings.CloneURL = func(string) string { return bare }
	out := f.run(t, RunOptions{})
	if out.Status != StatusEscalated || !strings.Contains(out.Reason, "not set up") {
		t.Fatalf("outcome = %+v, want escalated as not set up", out)
	}
}

func TestRunEscalatesWhenBranchExists(t *testing.T) {
	f := newFixture(t)
	git(t, filepath.Dir(f.remote), "--git-dir", f.remote, "branch", "ticketflow/PROJ-7-add-the-feature-file", "main")
	out := f.run(t, RunOptions{})
	if out.Status != StatusEscalated || !strings.Contains(out.Reason, "already exists") {
		t.Fatalf("outcome = %+v, want escalated on existing branch", out)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	f := newFixture(t)
	out := f.run(t, RunOptions{DryRun: true})

	if out.Status != StatusDryRun {
		t.Fatalf("status = %s, want dry_run", out.Status)
	}
	if !strings.Contains(out.Patch, "+ok") || !strings.Contains(out.PRBody, "PROJ-7") {
		t.Errorf("dry run did not report the change and PR body")
	}
	if len(f.host.prs) != 0 || len(f.tracker.comments) != 0 || len(f.tracker.added)+len(f.tracker.removed) != 0 {
		t.Errorf("dry run wrote to Jira or GitHub")
	}
	if f.remoteHas(t, "ticketflow/PROJ-7-add-the-feature-file", "feature.txt") {
		t.Errorf("dry run pushed a branch")
	}
}

func TestRunWithUnknownTicketFailsWithoutReporting(t *testing.T) {
	f := newFixture(t)
	p, err := New(Deps{Tracker: f.tracker, Host: f.host, Harness: f.harness, Planner: f.planner, Reviewer: f.reviewer, Settings: f.settings, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Run(context.Background(), "NOPE-1", RunOptions{}); err == nil {
		t.Fatal("Run succeeded for an unknown ticket")
	}
	if len(f.tracker.comments) != 0 {
		t.Errorf("commented on a ticket that could not be read")
	}
}

func TestResolveRepo(t *testing.T) {
	p, err := New(Deps{Tracker: &fakeTracker{}, Host: &fakeHost{}, Harness: &scriptHarness{}, Planner: &fakellm.Model{}, Reviewer: &fakellm.Model{},
		Settings: Settings{
			ProjectRepos: map[string]string{"PROJ": "acme/app"},
			RepoAllowed:  func(r string) bool { return r == "acme/lib" },
			CloneURL:     func(string) string { return "" },
		}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		project string
		labels  []string
		want    string
		wantErr bool
	}{
		{project: "PROJ", want: "acme/app"},
		{project: "proj", want: "acme/app"},
		{project: "PROJ", labels: []string{"repo:acme/lib"}, want: "acme/lib"},
		{project: "PROJ", labels: []string{"repo:evil/repo"}, wantErr: true},
		{project: "OTHER", wantErr: true},
	} {
		got, err := p.resolveRepo(jira.Issue{Project: tc.project, Labels: tc.labels})
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("resolveRepo(%s, %v) = %q, %v; want %q, err=%v", tc.project, tc.labels, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestNewRequiresDeps(t *testing.T) {
	if _, err := New(Deps{}); err == nil {
		t.Fatal("New accepted empty deps")
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Add the feature file":            "add-the-feature-file",
		"  ??? ":                          "change",
		"Fix: crash on 💥 login":           "fix-crash-on-login",
		strings.Repeat("long words ", 10): "long-words-long-words-long-words-long-wo",
	} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodeJSON(t *testing.T) {
	var v struct{ A int }
	for _, in := range []string{`{"A":1}`, "Sure:\n```json\n{\"A\":1}\n```", `prefix {"A":1} suffix`} {
		v.A = 0
		if err := decodeJSON(in, &v); err != nil || v.A != 1 {
			t.Errorf("decodeJSON(%q) = %v, A=%d", in, err, v.A)
		}
	}
	if err := decodeJSON("no json here", &v); err == nil {
		t.Error("decodeJSON accepted text without JSON")
	}
}

func TestWorkflowBuilds(t *testing.T) {
	f := newFixture(t)
	p, err := New(Deps{Tracker: f.tracker, Host: f.host, Harness: f.harness, Planner: f.planner, Reviewer: f.reviewer, Settings: f.settings, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Workflow("PROJ-1", t.TempDir()); err != nil {
		t.Fatalf("Workflow: %v", err)
	}
}
