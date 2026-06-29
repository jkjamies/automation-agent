package reviewer

import (
	"strings"
	"testing"
	"time"

	"automation-agent/internal/githubapi"
	"automation-agent/internal/ingest"
	"automation-agent/internal/tasks"
)

func reviewEnvelope(action string) ingest.Envelope {
	body := `{"action":"` + action + `","pull_request":{"number":7,"head":{"ref":"x","sha":"s"}},"repository":{"full_name":"acme/web.api"}}`
	return ingest.New(ingest.KindReview, "webhook:/github", []byte(body), time.Time{})
}

func applyOptions(opts []tasks.Option) tasks.Options {
	var o tasks.Options
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// A synchronize push coalesces: it carries a per-PR dedup name (review-<encoded repo>-<number>) and
// the debounce delay, so a burst collapses to one task on the latest SHA.
func TestEnqueueOptionsSynchronizeDebounces(t *testing.T) {
	o := applyOptions(EnqueueOptions(reviewEnvelope("synchronize"), 30*time.Second))
	if !strings.HasPrefix(o.Name, "review-") || !strings.HasSuffix(o.Name, "-7") {
		t.Errorf("dedup name = %q, want review-<repo>-7 shape", o.Name)
	}
	if o.Delay != 30*time.Second {
		t.Errorf("delay = %v, want 30s", o.Delay)
	}
}

// The repo component of the dedup name must be lossless: repos a naive sanitize-to-'-' would
// collapse ("acme/web.api" vs "acme/web-api") must get distinct names, or Cloud Tasks de-dup
// silently drops one PR's review.
func TestCoalesceKeyLosslessNoCollision(t *testing.T) {
	a := coalesceKey(githubapi.PullRequestEvent{RepoFullName: "acme/web.api", Number: 7})
	b := coalesceKey(githubapi.PullRequestEvent{RepoFullName: "acme/web-api", Number: 7})
	if a == b {
		t.Errorf("distinct repos collided to the same dedup name: %q", a)
	}
}

// opened/reopened/ready_for_review enqueue immediately (a human is waiting on the first review).
func TestEnqueueOptionsImmediateActions(t *testing.T) {
	for _, action := range []string{"opened", "reopened", "ready_for_review"} {
		if opts := EnqueueOptions(reviewEnvelope(action), 30*time.Second); opts != nil {
			t.Errorf("%s must enqueue immediately, got %+v", action, applyOptions(opts))
		}
	}
}

func TestEnqueueOptionsDisabledOrNonReview(t *testing.T) {
	// A non-positive debounce disables coalescing even for synchronize.
	if opts := EnqueueOptions(reviewEnvelope("synchronize"), 0); opts != nil {
		t.Errorf("zero debounce must yield no options, got %+v", applyOptions(opts))
	}
	// A non-review kind is never coalesced.
	ci := ingest.New(ingest.KindCI, "webhook:/github", []byte(`{}`), time.Time{})
	if opts := EnqueueOptions(ci, 30*time.Second); opts != nil {
		t.Errorf("non-review kind must yield no options, got %+v", applyOptions(opts))
	}
	// A malformed review payload falls back to immediate enqueue rather than erroring.
	bad := ingest.New(ingest.KindReview, "webhook:/github", []byte("{not json"), time.Time{})
	if opts := EnqueueOptions(bad, 30*time.Second); opts != nil {
		t.Errorf("malformed payload must yield no options, got %+v", applyOptions(opts))
	}
}
