package reviewer

import (
	"reflect"
	"testing"

	"automation-agent/internal/githubapi"
)

func TestParseFPMarker(t *testing.T) {
	if got := parseFPMarker("body\n" + fpMarker("a.go:3:msg") + "\n"); got != "a.go:3:msg" {
		t.Errorf("parseFPMarker = %q, want a.go:3:msg", got)
	}
	if got := parseFPMarker("no marker here"); got != "" {
		t.Errorf("parseFPMarker(no marker) = %q, want empty", got)
	}
}

func TestReconcile(t *testing.T) {
	keep := Finding{File: "a.go", Line: 1, Message: "keep me"}
	add := Finding{File: "b.go", Line: 2, Message: "brand new"}
	existing := []githubapi.ReviewCommentRef{
		{NodeID: "n-keep", Body: "x " + fpMarker(keep.fingerprint())},
		{NodeID: "n-stale", Body: "y " + fpMarker("c.go:9:gone")},
		{NodeID: "n-foreign", Body: "human note, no marker"},
	}
	res := reconcile([]Finding{keep, add}, existing)

	// keep already has a comment → not re-posted; add is new → posted.
	if len(res.toPost) != 1 || res.toPost[0].Message != "brand new" {
		t.Errorf("toPost = %+v, want only the new finding", res.toPost)
	}
	// stale's finding is gone → minimized; foreign has no marker → ignored.
	if !reflect.DeepEqual(res.toMinimize, []string{"n-stale"}) {
		t.Errorf("toMinimize = %v, want [n-stale]", res.toMinimize)
	}
}

func TestReconcileEmpty(t *testing.T) {
	if res := reconcile(nil, nil); len(res.toPost) != 0 || len(res.toMinimize) != 0 {
		t.Errorf("empty reconcile = %+v, want nothing", res)
	}
	// Every finding is new when the PR has no comments yet.
	if res := reconcile([]Finding{{File: "a.go", Line: 1, Message: "x"}}, nil); len(res.toPost) != 1 {
		t.Errorf("toPost = %d, want 1", len(res.toPost))
	}
}
