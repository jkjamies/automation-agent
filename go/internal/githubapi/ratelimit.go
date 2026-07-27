package githubapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// Rate-limit retry budget. These are deliberately small: this transport sits inside a Cloud Tasks
// dispatch, which already retries the whole task with its own backoff. In-process waiting only
// pays off for the short secondary-limit pauses GitHub asks for (typically a few seconds); a long
// primary-limit reset is better handed back so the instance can scale to zero instead of holding a
// request open for minutes.
const (
	// maxRateLimitWait caps a single in-process wait. A longer reset returns the 403/429 to the
	// caller, whose error surfaces and lets the task retry later.
	maxRateLimitWait = 60 * time.Second
	// maxRateLimitRetries bounds how many times one request is replayed. Under a sustained limit
	// each retry would burn the same wait again, so the transport gives up quickly.
	maxRateLimitRetries = 3
	// rateLimitSlack pads a computed reset instant. X-RateLimit-Reset is GitHub's clock, ours is
	// not; waking a hair early would spend a retry on a guaranteed second rejection.
	rateLimitSlack = time.Second
	// drainLimit bounds how much of a rejected response body is read before retrying. Draining lets
	// the connection be reused; the cap keeps a pathological body from being copied wholesale.
	drainLimit = 64 << 10
)

// rateLimitTransport retries requests GitHub rejected for rate limiting, honoring the wait GitHub
// asks for. It wraps the auth transport rather than the client so it covers every call — REST and
// the GraphQL minimize mutation alike — and so each replay re-fetches a token (an App installation
// token can expire during a long wait).
//
// It retries *only* rate-limit rejections, never 5xx. That distinction is what makes retrying a
// POST safe here: a rate-limited request was refused before GitHub acted on it, so replaying it
// cannot double-post a review or a comment. A 502 carries no such guarantee — the write may well
// have landed — so those are passed straight through to the caller.
//
// This complements rather than duplicates go-github, which caches the primary-quota headers it
// sees and then refuses later requests locally until the reset instant. That short-circuit only
// works once a rejection has been observed, and it never covers the secondary limits (which are
// announced by Retry-After alone). This transport handles the rejection itself, so the short wait
// GitHub asks for is spent waiting rather than surfaced as a failed publish.
type rateLimitTransport struct {
	base       http.RoundTripper
	log        *slog.Logger
	maxWait    time.Duration
	maxRetries int
	now        func() time.Time
	sleep      func(context.Context, time.Duration) error
}

// newRateLimitTransport wraps base with the default retry budget. A nil logger discards.
func newRateLimitTransport(base http.RoundTripper, log *slog.Logger) *rateLimitTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &rateLimitTransport{
		base:       base,
		log:        log,
		maxWait:    maxRateLimitWait,
		maxRetries: maxRateLimitRetries,
		now:        time.Now,
		sleep:      sleepCtx,
	}
}

// RoundTrip performs the request, replaying it after the requested wait when GitHub rejects it for
// rate limiting. Anything else — success, a permission 403, a transport error — is returned
// untouched, as is a rate-limit rejection whose wait exceeds the budget.
func (t *rateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := t.base.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		wait, limited := t.retryDelay(resp)
		if !limited {
			return resp, nil
		}
		// Every give-up path logs: a rate limit that silently degrades into a failed publish is
		// exactly the thing this transport exists to make visible.
		if attempt >= t.maxRetries {
			t.log.Warn("github rate limited; retry budget exhausted", "url", req.URL.Path, "status", resp.StatusCode, "attempts", attempt+1)
			return resp, nil
		}
		if wait > t.maxWait {
			t.log.Warn("github rate limited; wait exceeds the in-process budget, deferring to the task retry",
				"url", req.URL.Path, "status", resp.StatusCode, "wait", wait, "budget", t.maxWait)
			return resp, nil
		}
		next, ok := replayable(req)
		if !ok {
			t.log.Warn("github rate limited; request body cannot be replayed", "url", req.URL.Path, "status", resp.StatusCode)
			return resp, nil
		}
		t.log.Info("github rate limited; waiting before retry", "url", req.URL.Path, "status", resp.StatusCode, "wait", wait, "attempt", attempt+1)
		drain(resp)
		if err := t.sleep(req.Context(), wait); err != nil {
			// The caller's deadline expired (or it was cancelled) while we waited. Surface that
			// rather than the rate-limit response — the response body is already drained.
			return nil, err
		}
		req = next
	}
}

// retryDelay reports how long to wait before replaying resp, and whether resp is a rate-limit
// rejection at all. GitHub signals two distinct limits and they are read in priority order:
// Retry-After (secondary/abuse limits — GitHub states the wait outright) then an exhausted
// X-RateLimit-Remaining with its reset instant (the primary hourly quota). A 403 carrying neither
// is an ordinary permission error and must not be retried.
func (t *rateLimitTransport) retryDelay(resp *http.Response) (time.Duration, bool) {
	if resp == nil {
		return 0, false
	}
	if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusForbidden {
		return 0, false
	}
	if v := resp.Header.Get("Retry-After"); v != "" {
		if d, ok := parseRetryAfter(v, t.now()); ok {
			return d, true
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if sec, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return max(time.Unix(sec, 0).Sub(t.now()), 0) + rateLimitSlack, true
		}
	}
	return 0, false
}

// parseRetryAfter reads a Retry-After value in either RFC 9110 form — delta-seconds or an HTTP
// date. A value in the past clamps to zero (retry now); an unparseable one reports false so the
// caller falls through to the primary-limit headers.
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	if secs, err := strconv.Atoi(v); err == nil {
		return max(time.Duration(secs)*time.Second, 0), true
	}
	if ts, err := http.ParseTime(v); err == nil {
		return max(ts.Sub(now), 0), true
	}
	return 0, false
}

// replayable returns a request equivalent to req with a fresh body, or false when the body cannot
// be rewound. A bodyless request replays as itself; a request built from an in-memory body (which
// is every request this package makes — go-github buffers its JSON, and the GraphQL helper uses a
// bytes.Reader) carries GetBody and rewinds cleanly.
func replayable(req *http.Request) (*http.Request, bool) {
	if req.Body == nil || req.Body == http.NoBody {
		return req, true
	}
	if req.GetBody == nil {
		return nil, false
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, false
	}
	next := req.Clone(req.Context())
	next.Body = body
	return next, true
}

// drain reads and closes a rejected response body so the underlying connection can be reused for
// the retry instead of being torn down.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
	_ = resp.Body.Close()
}

// sleepCtx waits for d, or returns the context's error if it ends first — a rate-limit wait must
// never outlive the dispatch deadline that is paying for it.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
