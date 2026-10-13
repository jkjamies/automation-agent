package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDefaultBranchAndLabels(t *testing.T) {
	var labelBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/acme/app":
			_, _ = io.WriteString(w, `{"default_branch":"trunk"}`)
		case "POST /repos/acme/app/issues/5/labels":
			b, _ := io.ReadAll(r.Body)
			labelBody = string(b)
			_, _ = io.WriteString(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL+"/", "tok", srv.Client())
	ctx := context.Background()

	if b, err := c.DefaultBranch(ctx, "acme/app"); err != nil || b != "trunk" {
		t.Errorf("DefaultBranch = %q, %v", b, err)
	}
	if err := c.AddLabels(ctx, "acme/app", 5, []string{"agent-authored"}); err != nil {
		t.Fatal(err)
	}
	if labelBody != `{"labels":["agent-authored"]}` {
		t.Errorf("label body = %s", labelBody)
	}
	if err := c.AddLabels(ctx, "acme/app", 5, nil); err != nil {
		t.Errorf("AddLabels(nil) = %v", err)
	}
	if _, err := c.DefaultBranch(ctx, "acme/missing"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want a 404", err)
	}
}

func TestOpenPR(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"number":9,"html_url":"https://gh.example/acme/app/pull/9"}`)
	}))
	defer srv.Close()
	pr, err := NewClient(srv.URL, "", nil).OpenPR(context.Background(), "acme/app", NewPR{Title: "t", Head: "b", Base: "main", Body: "x", Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 9 || pr.URL != "https://gh.example/acme/app/pull/9" {
		t.Errorf("pr = %+v", pr)
	}
	if got["draft"] != true || got["head"] != "b" || got["base"] != "main" {
		t.Errorf("request = %v", got)
	}
}

func TestOpenPRReturnsExistingOnConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			http.Error(w, `{"message":"A pull request already exists"}`, http.StatusUnprocessableEntity)
			return
		}
		if r.URL.Query().Get("head") != "acme:feature" {
			t.Errorf("head query = %q", r.URL.Query().Get("head"))
		}
		_, _ = io.WriteString(w, `[{"number":3,"html_url":"https://gh.example/pull/3"}]`)
	}))
	defer srv.Close()
	pr, err := NewClient(srv.URL, "", nil).OpenPR(context.Background(), "acme/app", NewPR{Head: "feature", Base: "main"})
	if err != nil || pr.Number != 3 {
		t.Fatalf("OpenPR = %+v, %v; want the existing PR #3", pr, err)
	}
}

func TestOpenPRSurfacesOtherErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			http.Error(w, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity)
			return
		}
		_, _ = io.WriteString(w, `[]`)
	}))
	defer srv.Close()
	if _, err := NewClient(srv.URL, "", nil).OpenPR(context.Background(), "acme/app", NewPR{Head: "x"}); err == nil || !strings.Contains(err.Error(), "Validation Failed") {
		t.Errorf("err = %v, want the original 422", err)
	}
}
