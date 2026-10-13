package jira

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const issueJSON = `{
  "key": "PROJ-7",
  "fields": {
    "summary": "Add export",
    "labels": ["agent-ready", "repo:acme/app"],
    "issuetype": {"name": "Story"},
    "status": {"name": "Ready"},
    "project": {"key": "PROJ"},
    "components": [{"name": "api"}],
    "description": {"type": "doc", "version": 1, "content": [
      {"type": "paragraph", "content": [{"type": "text", "text": "Export orders as CSV."}]},
      {"type": "heading", "attrs": {"level": 2}, "content": [{"type": "text", "text": "Acceptance criteria"}]},
      {"type": "taskList", "content": [
        {"type": "taskItem", "attrs": {"state": "TODO"}, "content": [{"type": "text", "text": "a button"}]},
        {"type": "taskItem", "attrs": {"state": "DONE"}, "content": [{"type": "text", "text": "an endpoint"}]}
      ]}
    ]},
    "comment": {"comments": [
      {"author": {"displayName": "Ana"}, "body": {"type": "doc", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "UTF-8 please"}]}]}}
    ]}
  }
}`

func newServer(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "bot@example.com", "tok", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestIssue(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "bot@example.com" || pass != "tok" {
			t.Errorf("basic auth = %q %q %v", user, pass, ok)
		}
		if r.URL.Path != "/rest/api/3/issue/PROJ-7" || !strings.Contains(r.URL.Query().Get("fields"), "description") {
			t.Errorf("request = %s", r.URL)
		}
		_, _ = io.WriteString(w, issueJSON)
	})
	is, err := c.Issue(context.Background(), "PROJ-7")
	if err != nil {
		t.Fatal(err)
	}
	if is.Key != "PROJ-7" || is.Project != "PROJ" || is.Type != "Story" || is.Status != "Ready" || is.Summary != "Add export" {
		t.Errorf("issue = %+v", is)
	}
	if !strings.HasSuffix(is.URL, "/browse/PROJ-7") || !is.HasLabel("agent-ready") || is.HasLabel("nope") {
		t.Errorf("url/labels = %s %v", is.URL, is.Labels)
	}
	wantDesc := "Export orders as CSV.\n\n## Acceptance criteria\n\n- [ ] a button\n- [x] an endpoint"
	if is.Description != wantDesc {
		t.Errorf("description =\n%s\nwant\n%s", is.Description, wantDesc)
	}
	if len(is.Comments) != 1 || is.Comments[0] != (Comment{Author: "Ana", Body: "UTF-8 please"}) {
		t.Errorf("comments = %+v", is.Comments)
	}
	if len(is.Components) != 1 || is.Components[0] != "api" {
		t.Errorf("components = %v", is.Components)
	}
}

func TestBearerAuthAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer pat" {
			t.Errorf("Authorization = %q", got)
		}
		http.Error(w, `{"errorMessages":["Issue does not exist"]}`, http.StatusNotFound)
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL+"/", "", "pat", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Issue(context.Background(), "X-1")
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("err = %v", err)
	}
	if _, err := NewClient("not a url", "", "", nil); err == nil {
		t.Error("NewClient accepted an invalid URL")
	}
}

func TestCommentAndLabels(t *testing.T) {
	var bodies []map[string]any
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Errorf("decode: %v", err)
		}
		b["_route"] = r.Method + " " + r.URL.Path
		bodies = append(bodies, b)
		w.WriteHeader(http.StatusNoContent)
	})
	ctx := context.Background()
	if err := c.Comment(ctx, "PROJ-7", "Opened https://gh.example/pull/1\n\nThanks"); err != nil {
		t.Fatal(err)
	}
	if err := c.EditLabels(ctx, "PROJ-7", []string{"a"}, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	if err := c.EditLabels(ctx, "PROJ-7", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("got %d requests, want 2 (empty label edit sends nothing)", len(bodies))
	}
	if bodies[0]["_route"] != "POST /rest/api/3/issue/PROJ-7/comment" {
		t.Errorf("comment route = %v", bodies[0]["_route"])
	}
	doc, _ := json.Marshal(bodies[0]["body"])
	if !strings.Contains(string(doc), `"href":"https://gh.example/pull/1"`) || !strings.Contains(string(doc), `"Thanks"`) {
		t.Errorf("comment ADF = %s", doc)
	}
	labels, _ := json.Marshal(bodies[1])
	if bodies[1]["_route"] != "PUT /rest/api/3/issue/PROJ-7" || !strings.Contains(string(labels), `[{"add":"a"},{"remove":"b"}]`) {
		t.Errorf("labels request = %s", labels)
	}
}

func TestTransition(t *testing.T) {
	var posted string
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"transitions":[{"id":"11","name":"Start","to":{"name":"In Progress"}},{"id":"21","name":"Submit","to":{"name":"In Review"}}]}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		posted = string(b)
		w.WriteHeader(http.StatusNoContent)
	})
	ctx := context.Background()
	if err := c.Transition(ctx, "PROJ-7", "in review"); err != nil {
		t.Fatal(err)
	}
	if posted != `{"transition":{"id":"21"}}` {
		t.Errorf("posted %s", posted)
	}
	if err := c.Transition(ctx, "PROJ-7", "Done"); !errors.Is(err, ErrNoTransition) {
		t.Errorf("err = %v, want ErrNoTransition", err)
	}
}

func TestADFText(t *testing.T) {
	doc := `{"type":"doc","content":[
	  {"type":"paragraph","content":[{"type":"text","text":"See "},{"type":"text","text":"docs","marks":[{"type":"link","attrs":{"href":"https://d.example"}}]},{"type":"text","text":" and "},{"type":"text","text":"x()","marks":[{"type":"code"}]},{"type":"hardBreak"},{"type":"mention","attrs":{"text":"@Ana"}},{"type":"emoji","attrs":{"shortName":":tada:"}},{"type":"inlineCard","attrs":{"url":"https://card.example"}}]},
	  {"type":"orderedList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"one"}]},{"type":"bulletList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"nested"}]}]}]}]}]},
	  {"type":"codeBlock","attrs":{"language":"go"},"content":[{"type":"text","text":"fmt.Println()"}]},
	  {"type":"blockquote","content":[{"type":"paragraph","content":[{"type":"text","text":"quoted"}]}]},
	  {"type":"rule"},
	  {"type":"panel","attrs":{"title":"Note"},"content":[{"type":"paragraph","content":[{"type":"text","text":"in panel"}]}]},
	  {"type":"table","content":[{"type":"tableRow","content":[{"type":"tableHeader","content":[{"type":"paragraph","content":[{"type":"text","text":"h"}]}]},{"type":"tableCell","content":[{"type":"paragraph","content":[{"type":"status","attrs":{"text":"DONE"}}]}]}]}]}
	]}`
	got := ADFText(json.RawMessage(doc))
	for _, want := range []string{
		"See [docs](https://d.example) and `x()`\n@Ana:tada:https://card.example",
		"1. one\n  - nested",
		"```go\nfmt.Println()\n```",
		"> quoted",
		"---",
		"in panel",
		"| h | DONE |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ADFText missing %q in:\n%s", want, got)
		}
	}
	for in, want := range map[string]string{"": "", "null": "", `"wiki *markup*"`: "wiki *markup*", `{bad`: ""} {
		if got := ADFText(json.RawMessage(in)); got != want {
			t.Errorf("ADFText(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestParseEvent(t *testing.T) {
	ev, err := ParseEvent([]byte(`{"webhookEvent":"jira:issue_updated","issue":{"key":"PROJ-7","fields":{"labels":["agent-ready"]}},"changelog":{"items":[{"field":"labels"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Name != "jira:issue_updated" || ev.IssueKey != "PROJ-7" || !ev.LabelsChanged || len(ev.Labels) != 1 {
		t.Errorf("event = %+v", ev)
	}
	if _, err := ParseEvent([]byte(`{"webhookEvent":"sprint_started"}`)); !errors.Is(err, ErrNotIssueEvent) {
		t.Errorf("err = %v, want ErrNotIssueEvent", err)
	}
	if _, err := ParseEvent([]byte(`nope`)); err == nil {
		t.Error("ParseEvent accepted invalid JSON")
	}
}

func TestAuthentic(t *testing.T) {
	body := []byte(`{"x":1}`)
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	hdr := func(k, v string) http.Header { h := http.Header{}; h.Set(k, v); return h }

	for name, tc := range map[string]struct {
		secret string
		h      http.Header
		want   bool
	}{
		"valid signature":   {"s3cret", hdr("X-Hub-Signature", sig), true},
		"wrong secret":      {"other", hdr("X-Hub-Signature", sig), false},
		"wrong algorithm":   {"s3cret", hdr("X-Hub-Signature", "sha1=abc"), false},
		"malformed":         {"s3cret", hdr("X-Hub-Signature", "sha256=zz"), false},
		"no separator":      {"s3cret", hdr("X-Hub-Signature", "deadbeef"), false},
		"valid token":       {"s3cret", hdr(TokenHeader, "s3cret"), true},
		"wrong token":       {"s3cret", hdr(TokenHeader, "nope"), false},
		"nothing presented": {"s3cret", http.Header{}, false},
		"no secret":         {"", hdr(TokenHeader, ""), false},
	} {
		if got := Authentic(tc.secret, tc.h, body); got != tc.want {
			t.Errorf("%s: Authentic = %v, want %v", name, got, tc.want)
		}
	}
}
