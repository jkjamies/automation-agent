package reviewer

import (
	"testing"
	"time"

	"automation-agent/internal/githubapi"
	"automation-agent/internal/ingest"
	"automation-agent/internal/tasks"
)

func reviewEnvelope(action string) ingest.Envelope {
	return reviewEnvelopeAt(action, time.Unix(1_700_000_000, 0))
}

func reviewEnvelopeAt(action string, at time.Time) ingest.Envelope {
	body := `{"action":"` + action + `","pull_request":{"number":7,"head":{"ref":"x","sha":"s"}},"repository":{"full_name":"acme/web.api"}}`
	return ingest.New(ingest.KindReview, "webhook:/github", []byte(body), at)
}

func applyOptions(opts []tasks.Option) tasks.Options {
	var o tasks.Options
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// A synchronize push coalesces: it carries a per-PR dedup name (review-<encoded repo>-<number>-
// <window>) and the debounce delay, so a burst collapses to one task on the latest SHA.
func TestEnqueueOptionsSynchronizeDebounces(t *testing.T) {
	o := applyOptions(EnqueueOptions(reviewEnvelope("synchronize"), 30*time.Second))
	bucket := time.Unix(1_700_000_000, 0).Truncate(30 * time.Second)
	want := coalesceKey(githubapi.PullRequestEvent{RepoFullName: "acme/web.api", Number: 7}, bucket)
	if o.Name != want {
		t.Errorf("dedup name = %q, want %q", o.Name, want)
	}
	if o.Delay != 30*time.Second {
		t.Errorf("delay = %v, want 30s", o.Delay)
	}
}

// Pushes within one debounce window share a name (so they coalesce to one review), while a push in
// a later window gets a distinct name. A fixed per-PR name would otherwise collide with Cloud Tasks'
// ~1h name reservation and silently drop the later review.
func TestEnqueueOptionsBucketsByWindow(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	a := applyOptions(EnqueueOptions(reviewEnvelopeAt("synchronize", base.Add(2*time.Second)), 30*time.Second))
	b := applyOptions(EnqueueOptions(reviewEnvelopeAt("synchronize", base.Add(5*time.Second)), 30*time.Second))
	c := applyOptions(EnqueueOptions(reviewEnvelopeAt("synchronize", base.Add(45*time.Second)), 30*time.Second))
	if a.Name != b.Name {
		t.Errorf("pushes in the same window must coalesce: %q != %q", a.Name, b.Name)
	}
	if a.Name == c.Name {
		t.Errorf("push in a later window must get a distinct name, both = %q", a.Name)
	}
}

// The repo component of the dedup name must be lossless: repos a naive sanitize-to-'-' would
// collapse ("acme/web.api" vs "acme/web-api") must get distinct names, or Cloud Tasks de-dup
// silently drops one PR's review.
func TestCoalesceKeyLosslessNoCollision(t *testing.T) {
	bucket := time.Unix(1_700_000_000, 0)
	a := coalesceKey(githubapi.PullRequestEvent{RepoFullName: "acme/web.api", Number: 7}, bucket)
	b := coalesceKey(githubapi.PullRequestEvent{RepoFullName: "acme/web-api", Number: 7}, bucket)
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
