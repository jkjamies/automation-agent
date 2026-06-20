package githubapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// testClient points a real *github.Client at a stub server (go-github's testing
// pattern: override BaseURL).
func testClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New("")
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

func TestFindAgentPRs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"number":5,"head":{"ref":"agent/fix","sha":"s5"},"labels":[{"name":"automation-agent"}]},
			{"number":6,"head":{"ref":"feature","sha":"s6"},"labels":[{"name":"enhancement"}]}
		]`))
	})
	c := testClient(t, mux)

	prs, err := c.FindAgentPRs(context.Background(), "o", "r", "automation-agent")
	if err != nil {
		t.Fatalf("FindAgentPRs: %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 5 {
		t.Fatalf("agent PRs = %+v, want only #5", prs)
	}
}

func TestAttemptCount(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls/7/commits", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"sha":"a"},{"sha":"b"}]`))
	})
	c := testClient(t, mux)

	n, err := c.AttemptCount(context.Background(), "o", "r", 7)
	if err != nil {
		t.Fatalf("AttemptCount: %v", err)
	}
	if n != 2 {
		t.Errorf("attempts = %d, want 2", n)
	}
}

func TestAgentCheck(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/commits/sha1/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"name":"agent-lint-verify","status":"completed","conclusion":"success","completed_at":"2026-06-19T11:00:00Z"}]}`))
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

	missing, err := c.AgentCheck(context.Background(), "o", "r", "sha2", "agent-lint-verify")
	if err != nil {
		t.Fatalf("AgentCheck(missing): %v", err)
	}
	if missing.Found {
		t.Error("expected Found=false for a ref with no agent check")
	}
}
