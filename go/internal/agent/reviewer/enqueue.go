package reviewer

import (
	"encoding/base64"
	"strconv"
	"time"

	"automation-agent/internal/githubapi"
	"automation-agent/internal/ingest"
	"automation-agent/internal/tasks"
)

// EnqueueOptions returns the transport hints for a review envelope so rapid pushes coalesce
// (transport Decision 3 / spec Decision 12). A pull_request "synchronize" (a new push to an open
// PR) is enqueued under a per-PR dedup name with a debounce delay, so a burst of pushes collapses
// to one delayed task that reviews the latest SHA; the worker's staleness check then enforces
// newest-wins (Cloud Tasks does not guarantee ordering). opened/reopened/ready_for_review enqueue
// immediately (a human is waiting on the first review). Any non-review kind, an unparseable
// payload, or a non-positive debounce yields no options (immediate enqueue). Only the Cloud Tasks
// backend honors the hints; the in-process backend ignores them.
//
// Coalescing is a workflow concern, so it lives here rather than in the transport (which stays
// dumb about PRs and SHAs).
func EnqueueOptions(e ingest.Envelope, debounce time.Duration) []tasks.Option {
	if e.Kind != ingest.KindReview || debounce <= 0 {
		return nil
	}
	ev, err := githubapi.ParsePullRequestEvent(e.Payload)
	if err != nil || ev.Action != "synchronize" {
		return nil
	}
	return []tasks.Option{
		tasks.WithName(coalesceKey(ev)),
		tasks.WithDelay(debounce),
	}
}

// coalesceKey is the per-PR Cloud Tasks dedup name. Identically-named tasks within the dedup
// window collapse to one. The repo full name is base64url-encoded so the name is both valid in the
// Cloud Tasks charset ([A-Za-z0-9_-]) and lossless: a naive replace-invalid-with-'-' would collapse
// distinct repos (e.g. "acme/web.api" and "acme/web-api") to the same name and silently drop one
// PR's review (the staleness check can't recover a cross-repo collision — it only guards stale SHAs
// within a PR).
func coalesceKey(ev githubapi.PullRequestEvent) string {
	return "review-" + base64.RawURLEncoding.EncodeToString([]byte(ev.RepoFullName)) + "-" + strconv.Itoa(ev.Number)
}
