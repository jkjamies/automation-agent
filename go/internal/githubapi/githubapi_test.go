package githubapi

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"automation-agent/internal/auth"
)

// testClient points a real *github.Client at a stub server (go-github's testing
// pattern: override BaseURL).
func testClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(auth.NewStaticProvider(""))
	u, _ := url.Parse(srv.URL + "/")
	c.gh.BaseURL = u
	return c
}

func TestListCommitsSince(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/commits", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"sha":"abc","html_url":"https://gh/abc","commit":{"message":"fix bug","author":{"name":"Jane","date":"2026-06-19T10:00:00Z"}}}
		]`))
	})
	c := testClient(t, mux)

	commits, err := c.ListCommitsSince(context.Background(), "o", "r", time.Unix(0, 0))
	if err != nil {
		t.Fatalf("ListCommitsSince: %v", err)
	}
	if len(commits) != 1 {
		t.Fatalf("got %d commits, want 1", len(commits))
	}
	got := commits[0]
	if got.SHA != "abc" || got.Author != "Jane" || got.Message != "fix bug" || got.URL != "https://gh/abc" {
		t.Errorf("commit = %+v", got)
	}
	if got.When.UTC().Format(time.RFC3339) != "2026-06-19T10:00:00Z" {
		t.Errorf("when = %v", got.When)
	}
}

func TestCreatePRAndLabels(t *testing.T) {
	var labeled bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repos/o/r/pulls", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"number":5,"title":"fix lint","html_url":"https://gh/pr/5","head":{"ref":"agent/fix","sha":"deadbeef"}}`))
	})
	mux.HandleFunc("POST /repos/o/r/issues/5/labels", func(w http.ResponseWriter, _ *http.Request) {
		labeled = true
		_, _ = w.Write([]byte(`[{"name":"automation-agent"}]`))
	})
	c := testClient(t, mux)

	pr, err := c.CreatePR(context.Background(), "o", "r", PRInput{Title: "fix lint", Head: "agent/fix", Base: "main"})
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if pr.Number != 5 || pr.Branch != "agent/fix" || pr.HeadSHA != "deadbeef" || pr.URL != "https://gh/pr/5" {
		t.Errorf("pr = %+v", pr)
	}
	if err := c.AddLabels(context.Background(), "o", "r", 5, "automation-agent"); err != nil {
		t.Fatalf("AddLabels: %v", err)
	}
	if !labeled {
		t.Error("labels endpoint not called")
	}
}

func TestFindOpenPRByBranch(t *testing.T) {
	var gotHead, gotState string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		gotHead = r.URL.Query().Get("head")
		gotState = r.URL.Query().Get("state")
		_, _ = w.Write([]byte(`[
			{"number":5,"head":{"ref":"agent/fix","sha":"s5"},"labels":[{"name":"automation-agent"}]}
		]`))
	})
	c := testClient(t, mux)

	pr, found, err := c.FindOpenPRByBranch(context.Background(), "o", "r", "agent/fix")
	if err != nil {
		t.Fatalf("FindOpenPRByBranch: %v", err)
	}
	if !found || pr.Number != 5 {
		t.Fatalf("pr = %+v found=%v, want #5", pr, found)
	}
	if gotHead != "o:agent/fix" || gotState != "open" {
		t.Fatalf("query head=%q state=%q, want head=o:agent/fix state=open", gotHead, gotState)
	}
}

func TestFindOpenPRByBranchNone(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	c := testClient(t, mux)

	_, found, err := c.FindOpenPRByBranch(context.Background(), "o", "r", "nope")
	if err != nil {
		t.Fatalf("FindOpenPRByBranch: %v", err)
	}
	if found {
		t.Fatal("found = true, want false for no open PR")
	}
}

func TestGetFileContent(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("package foo\n"))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/contents/internal/foo.go", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"type":"file","encoding":"base64","path":"internal/foo.go","content":"` + encoded + `"}`))
	})
	c := testClient(t, mux)

	got, err := c.GetFileContent(context.Background(), "o", "r", "internal/foo.go", "main")
	if err != nil {
		t.Fatalf("GetFileContent: %v", err)
	}
	if got != "package foo\n" {
		t.Errorf("content = %q", got)
	}
}

func TestListPRFiles(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls/7/files", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`[
				{"filename":"b.go","previous_filename":"old.go","status":"renamed","additions":1,"deletions":1,"patch":"@@ -1 +1 @@"}
			]`))
			return
		}
		// First page advertises a next page so ListPRFiles must follow pagination.
		w.Header().Set("Link", `<http://`+r.Host+`/repos/o/r/pulls/7/files?page=2>; rel="next"`)
		_, _ = w.Write([]byte(`[
			{"filename":"a.go","status":"added","additions":10,"deletions":0,"patch":"@@ -0,0 +1,10 @@"},
			{"filename":"img.png","status":"added","additions":0,"deletions":0}
		]`))
	})
	c := testClient(t, mux)

	files, err := c.ListPRFiles(context.Background(), "o", "r", 7)
	if err != nil {
		t.Fatalf("ListPRFiles: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("got %d files, want 3 (pagination followed)", len(files))
	}
	if files[0].Path != "a.go" || files[0].Patch == "" {
		t.Errorf("file[0] = %+v, want a.go with a patch", files[0])
	}
	// A binary file carries no patch — kept (counted), not dropped or an error.
	if files[1].Path != "img.png" || files[1].Patch != "" {
		t.Errorf("file[1] = %+v, want img.png with empty patch", files[1])
	}
	if files[2].Path != "b.go" || files[2].PreviousPath != "old.go" || files[2].Status != "renamed" {
		t.Errorf("file[2] = %+v, want b.go renamed from old.go", files[2])
	}
}

func TestParsePullRequestEvent(t *testing.T) {
	body := `{
		"action":"opened",
		"pull_request":{
			"number":7,
			"draft":true,
			"head":{"ref":"feature/x","sha":"headsha"},
			"base":{"ref":"main"},
			"user":{"login":"octocat"},
			"labels":[{"name":"enhancement"},{"name":"skip-review"}]
		},
		"repository":{"full_name":"acme/web"}
	}`
	ev, err := ParsePullRequestEvent([]byte(body))
	if err != nil {
		t.Fatalf("ParsePullRequestEvent: %v", err)
	}
	if ev.Action != "opened" || ev.Number != 7 || ev.RepoFullName != "acme/web" {
		t.Errorf("event = %+v", ev)
	}
	if ev.HeadRef != "feature/x" || ev.HeadSHA != "headsha" || ev.BaseRef != "main" {
		t.Errorf("refs = %+v", ev)
	}
	if !ev.Draft || ev.AuthorLogin != "octocat" {
		t.Errorf("draft/author = %v / %q", ev.Draft, ev.AuthorLogin)
	}
	if len(ev.Labels) != 2 || ev.Labels[0] != "enhancement" || ev.Labels[1] != "skip-review" {
		t.Errorf("labels = %v", ev.Labels)
	}
}

func TestParsePullRequestEventMalformed(t *testing.T) {
	if _, err := ParsePullRequestEvent([]byte("{not json")); err == nil {
		t.Fatal("expected an error for a malformed pull_request body")
	}
}

func TestParseCheckRunEvent(t *testing.T) {
	body := `{
		"action":"completed",
		"check_run":{
			"name":"agent-lint-verify",
			"status":"completed",
			"conclusion":"failure",
			"head_sha":"sha123",
			"output":{"text":"errcheck: unchecked error"},
			"pull_requests":[{"number":12,"head":{"ref":"agent/fix"}}]
		},
		"repository":{"full_name":"acme/api"}
	}`
	ev, err := ParseCheckRunEvent([]byte(body))
	if err != nil {
		t.Fatalf("ParseCheckRunEvent: %v", err)
	}
	if ev.Action != "completed" || ev.CheckName != "agent-lint-verify" || ev.Conclusion != "failure" {
		t.Errorf("event = %+v", ev)
	}
	if ev.HeadSHA != "sha123" || ev.PRNumber != 12 || ev.PRBranch != "agent/fix" {
		t.Errorf("correlation = %+v", ev)
	}
	if ev.RepoFullName != "acme/api" || ev.OutputText != "errcheck: unchecked error" {
		t.Errorf("repo/output = %q / %q", ev.RepoFullName, ev.OutputText)
	}
}

func TestAgentCheck(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/commits/sha1/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"name":"agent-lint-verify","status":"completed","conclusion":"success","completed_at":"2026-06-19T11:00:00Z","output":{"summary":"all checks passed"}}]}`))
	})
	mux.HandleFunc("GET /repos/o/r/commits/sha2/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
	})
	c := testClient(t, mux)

	res, err := c.AgentCheck(context.Background(), "o", "r", "sha1", "agent-lint-verify")
	if err != nil {
		t.Fatalf("AgentCheck: %v", err)
	}
	if !res.Found || res.Status != "completed" || res.Conclusion != "success" {
		t.Errorf("check = %+v", res)
	}
	if res.OutputText != "all checks passed" {
		t.Errorf("output text = %q", res.OutputText)
	}

	missing, err := c.AgentCheck(context.Background(), "o", "r", "sha2", "agent-lint-verify")
	if err != nil {
		t.Fatalf("AgentCheck(missing): %v", err)
	}
	if missing.Found {
		t.Error("expected Found=false for a ref with no agent check")
	}
}
