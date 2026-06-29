package githubapi

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

func TestCreateReview(t *testing.T) {
	var gotPath string
	var body []byte
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repos/o/r/pulls/7/reviews", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":1}`))
	})
	c := testClient(t, mux)

	err := c.CreateReview(context.Background(), "o", "r", 7, ReviewInput{
		Body:     "summary",
		Comments: []ReviewComment{{Path: "a.go", Line: 3, Side: "RIGHT", Body: "issue"}},
	})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
	if gotPath != "/repos/o/r/pulls/7/reviews" {
		t.Errorf("path = %s", gotPath)
	}
	s := string(body)
	for _, want := range []string{`"event":"COMMENT"`, `"a.go"`, `"RIGHT"`, `"issue"`} {
		if !strings.Contains(s, want) {
			t.Errorf("review body missing %q: %s", want, s)
		}
	}
}

func TestUpsertMarkerCommentEditsExisting(t *testing.T) {
	var edited bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/issues/7/comments", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":11,"body":"other"},{"id":22,"body":"hi MARK here"}]`))
	})
	mux.HandleFunc("PATCH /repos/o/r/issues/comments/22", func(w http.ResponseWriter, _ *http.Request) {
		edited = true
		_, _ = w.Write([]byte(`{"id":22}`))
	})
	c := testClient(t, mux)

	if err := c.UpsertMarkerComment(context.Background(), "o", "r", 7, "MARK", "new MARK body"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !edited {
		t.Error("expected the existing marker comment to be edited in place")
	}
}

func TestUpsertMarkerCommentCreatesWhenAbsent(t *testing.T) {
	var created bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/issues/7/comments", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":11,"body":"unrelated"}]`))
	})
	mux.HandleFunc("POST /repos/o/r/issues/7/comments", func(w http.ResponseWriter, _ *http.Request) {
		created = true
		_, _ = w.Write([]byte(`{"id":99}`))
	})
	c := testClient(t, mux)

	if err := c.UpsertMarkerComment(context.Background(), "o", "r", 7, "MARK", "body MARK"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !created {
		t.Error("expected a new comment to be created when no marker matches")
	}
}

// Identity-unresolved fallback: with no authoredLogin (App-mode identity lookup failed), the
// upsert must not edit a marker-bearing comment authored by a non-bot (GitHub rejects editing a
// foreign comment); it creates a fresh one when the only marker match is a human echoing it.
func TestUpsertMarkerCommentAppModeSkipsForeignAuthor(t *testing.T) {
	var editedForeign, created bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/issues/7/comments", func(w http.ResponseWriter, _ *http.Request) {
		// A human quoted the hidden marker; it is not the app's own comment.
		_, _ = w.Write([]byte(`[{"id":11,"body":"look at this MARK","user":{"type":"User"}}]`))
	})
	mux.HandleFunc("PATCH /repos/o/r/issues/comments/11", func(w http.ResponseWriter, _ *http.Request) {
		editedForeign = true
		_, _ = w.Write([]byte(`{"id":11}`))
	})
	mux.HandleFunc("POST /repos/o/r/issues/7/comments", func(w http.ResponseWriter, _ *http.Request) {
		created = true
		_, _ = w.Write([]byte(`{"id":99}`))
	})
	c := testClient(t, mux)
	c.appAuthored = true

	if err := c.UpsertMarkerComment(context.Background(), "o", "r", 7, "MARK", "body MARK"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if editedForeign {
		t.Error("must not edit a foreign (non-bot) comment that merely echoes the marker")
	}
	if !created {
		t.Error("expected a new comment when the only marker match is foreign")
	}
}

// Identity-unresolved fallback: with no authoredLogin, the app's own bot comment is edited in
// place even when a human comment also echoes the marker.
func TestUpsertMarkerCommentAppModeEditsOwnBot(t *testing.T) {
	var editedBot bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/issues/7/comments", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":11,"body":"human MARK","user":{"type":"User"}},{"id":22,"body":"bot MARK","user":{"type":"Bot"}}]`))
	})
	mux.HandleFunc("PATCH /repos/o/r/issues/comments/22", func(w http.ResponseWriter, _ *http.Request) {
		editedBot = true
		_, _ = w.Write([]byte(`{"id":22}`))
	})
	c := testClient(t, mux)
	c.appAuthored = true

	if err := c.UpsertMarkerComment(context.Background(), "o", "r", 7, "MARK", "new MARK body"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !editedBot {
		t.Error("expected the app's own bot comment to be edited in place")
	}
}

// With the client's own login known, ownership is by identity: the comment with the matching
// login is edited even when another bot's comment also echoes the marker.
func TestUpsertMarkerCommentEditsOwnByLogin(t *testing.T) {
	var editedOwn bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/issues/7/comments", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":11,"body":"foreign MARK","user":{"login":"other[bot]","type":"Bot"}},{"id":22,"body":"ours MARK","user":{"login":"agent-app[bot]","type":"Bot"}}]`))
	})
	mux.HandleFunc("PATCH /repos/o/r/issues/comments/22", func(w http.ResponseWriter, _ *http.Request) {
		editedOwn = true
		_, _ = w.Write([]byte(`{"id":22}`))
	})
	mux.HandleFunc("PATCH /repos/o/r/issues/comments/11", func(http.ResponseWriter, *http.Request) {
		t.Error("must not edit another bot's comment that merely echoes the marker")
	})
	c := testClient(t, mux)
	c.appAuthored = true
	c.authoredLogin = "agent-app[bot]"

	if err := c.UpsertMarkerComment(context.Background(), "o", "r", 7, "MARK", "new MARK body"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !editedOwn {
		t.Error("expected the comment authored by our own login to be edited in place")
	}
}

// With the login known, a foreign bot echoing the marker is not ours: it is skipped and a fresh
// comment is created (the type-only check would have wrongly edited it).
func TestUpsertMarkerCommentSkipsForeignBotByLogin(t *testing.T) {
	var created bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/issues/7/comments", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":11,"body":"foreign MARK","user":{"login":"other[bot]","type":"Bot"}}]`))
	})
	mux.HandleFunc("PATCH /repos/o/r/issues/comments/11", func(http.ResponseWriter, *http.Request) {
		t.Error("must not edit a foreign bot's comment")
	})
	mux.HandleFunc("POST /repos/o/r/issues/7/comments", func(w http.ResponseWriter, _ *http.Request) {
		created = true
		_, _ = w.Write([]byte(`{"id":99}`))
	})
	c := testClient(t, mux)
	c.appAuthored = true
	c.authoredLogin = "agent-app[bot]"

	if err := c.UpsertMarkerComment(context.Background(), "o", "r", 7, "MARK", "body MARK"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !created {
		t.Error("expected a new comment when the only marker match is a foreign bot")
	}
}

// PAT mode: ownership is by the resolved self user login — the self-authored comment is edited
// and another user's comment echoing the marker is left alone.
func TestUpsertMarkerCommentPATMatchesSelfLogin(t *testing.T) {
	var editedSelf bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/issues/7/comments", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":11,"body":"theirs MARK","user":{"login":"someone","type":"User"}},{"id":22,"body":"mine MARK","user":{"login":"me","type":"User"}}]`))
	})
	mux.HandleFunc("PATCH /repos/o/r/issues/comments/22", func(w http.ResponseWriter, _ *http.Request) {
		editedSelf = true
		_, _ = w.Write([]byte(`{"id":22}`))
	})
	mux.HandleFunc("PATCH /repos/o/r/issues/comments/11", func(http.ResponseWriter, *http.Request) {
		t.Error("must not edit another user's comment that merely echoes the marker")
	})
	c := testClient(t, mux)
	c.authoredLogin = "me" // PAT mode: appAuthored stays false

	if err := c.UpsertMarkerComment(context.Background(), "o", "r", 7, "MARK", "new MARK body"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !editedSelf {
		t.Error("expected the self-authored comment to be edited in place")
	}
}

// Weak fallback (identity unresolved): a marker-bearing bot comment that turns out not to be ours
// — the edit is rejected 403 — must not fail the publish; it falls through to create a fresh one.
func TestUpsertMarkerCommentWeakFallbackCreatesOnForbiddenEdit(t *testing.T) {
	var created bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/issues/7/comments", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":11,"body":"bot MARK","user":{"login":"other[bot]","type":"Bot"}}]`))
	})
	mux.HandleFunc("PATCH /repos/o/r/issues/comments/11", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"not authored by this app"}`, http.StatusForbidden)
	})
	mux.HandleFunc("POST /repos/o/r/issues/7/comments", func(w http.ResponseWriter, _ *http.Request) {
		created = true
		_, _ = w.Write([]byte(`{"id":99}`))
	})
	c := testClient(t, mux)
	c.appAuthored = true // identity unresolved → author-type fallback, no authoredLogin

	if err := c.UpsertMarkerComment(context.Background(), "o", "r", 7, "MARK", "body MARK"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !created {
		t.Error("a 403 on the weak-fallback edit must fall through to creating a fresh comment")
	}
}

func TestCreateCheckRun(t *testing.T) {
	var body []byte
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repos/o/r/check-runs", func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":1}`))
	})
	c := testClient(t, mux)

	err := c.CreateCheckRun(context.Background(), "o", "r", CheckRunInput{
		Name: "agent-review", HeadSHA: "deadbeef", Conclusion: "neutral", Title: "t", Summary: "s",
	})
	if err != nil {
		t.Fatalf("CreateCheckRun: %v", err)
	}
	s := string(body)
	for _, want := range []string{`"agent-review"`, `"deadbeef"`, `"completed"`, `"neutral"`} {
		if !strings.Contains(s, want) {
			t.Errorf("check body missing %q: %s", want, s)
		}
	}
}

// The advisory check must never gate a merge, so the API boundary rejects any conclusion other
// than success/neutral before it reaches GitHub (the empty mux is never called).
func TestCreateCheckRunRejectsNonAdvisory(t *testing.T) {
	c := testClient(t, http.NewServeMux())
	for _, concl := range []string{"failure", "cancelled", "timed_out", ""} {
		if err := c.CreateCheckRun(context.Background(), "o", "r", CheckRunInput{Name: "agent-review", HeadSHA: "s", Conclusion: concl}); err == nil {
			t.Errorf("conclusion %q must be rejected (advisory: success/neutral only)", concl)
		}
	}
}

// An empty marker (matches every comment) or a body missing the marker (unfindable next time)
// is a caller bug and is rejected before any API call.
func TestUpsertMarkerCommentValidates(t *testing.T) {
	c := testClient(t, http.NewServeMux())
	if err := c.UpsertMarkerComment(context.Background(), "o", "r", 1, "", "body"); err == nil {
		t.Error("an empty marker must be rejected")
	}
	if err := c.UpsertMarkerComment(context.Background(), "o", "r", 1, "MARK", "no marker here"); err == nil {
		t.Error("a body that omits the marker must be rejected")
	}
}

func TestListReviewComments(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls/7/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`[{"node_id":"N2","body":"second"}]`))
			return
		}
		w.Header().Set("Link", `<http://`+r.Host+`/repos/o/r/pulls/7/comments?page=2>; rel="next"`)
		_, _ = w.Write([]byte(`[{"node_id":"N1","body":"first <!-- ar-fp:a.go:1:x -->"}]`))
	})
	c := testClient(t, mux)

	refs, err := c.ListReviewComments(context.Background(), "o", "r", 7)
	if err != nil {
		t.Fatalf("ListReviewComments: %v", err)
	}
	if len(refs) != 2 || refs[0].NodeID != "N1" || refs[1].NodeID != "N2" {
		t.Fatalf("refs = %+v, want N1,N2 (pagination followed)", refs)
	}
	if refs[0].Body == "" {
		t.Error("comment body not captured")
	}
}

// MinimizeComment posts the OUTDATED minimize mutation to the GraphQL endpoint (derived from the
// REST BaseURL) carrying the comment's node id.
func TestMinimizeComment(t *testing.T) {
	var gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"data":{"minimizeComment":{"minimizedComment":{"isMinimized":true}}}}`))
	})
	c := testClient(t, mux)

	if err := c.MinimizeComment(context.Background(), "NODE1"); err != nil {
		t.Fatalf("MinimizeComment: %v", err)
	}
	for _, want := range []string{"minimizeComment", "OUTDATED", "NODE1"} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("graphql request missing %q: %s", want, gotBody)
		}
	}
}

func TestMinimizeCommentGraphQLError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"errors":[{"message":"Could not resolve to a node"}]}`))
	})
	c := testClient(t, mux)
	if err := c.MinimizeComment(context.Background(), "BAD"); err == nil {
		t.Fatal("a GraphQL errors[] response must become a Go error")
	}
}

func TestGraphQLURL(t *testing.T) {
	cases := map[string]string{
		"https://api.github.com/":         "https://api.github.com/graphql",
		"https://ghe.example.com/api/v3/": "https://ghe.example.com/api/graphql",
	}
	for base, want := range cases {
		c := New(auth.NewStaticProvider(""))
		u, _ := url.Parse(base)
		c.gh.BaseURL = u
		if got := c.graphqlURL(); got != want {
			t.Errorf("graphqlURL(%q) = %q, want %q", base, got, want)
		}
	}
}
