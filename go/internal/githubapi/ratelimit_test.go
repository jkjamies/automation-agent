package githubapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-github/v78/github"

	"automation-agent/internal/auth"
)

// fixedNow is the transport's clock in these tests, so a reset instant is an exact arithmetic
// relationship rather than a race against the wall clock.
var fixedNow = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

// recordingSleep captures the waits the transport asked for instead of taking them, so a test can
// assert the duration without spending it.
type recordingSleep struct {
	waits []time.Duration
	err   error // returned instead of sleeping, to simulate a context ending mid-wait
}

func (s *recordingSleep) record(_ context.Context, d time.Duration) error {
	s.waits = append(s.waits, d)
	return s.err
}

// rateLimitedClient points a Client at a stub server through the real transport stack (rate-limit
// retry over auth), with the clock and the sleep replaced. It returns the client and the recorder.
func rateLimitedClient(t *testing.T, h http.Handler) (*Client, *recordingSleep) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	sleeper := &recordingSleep{}
	inner := &http.Client{Timeout: httpTimeout, Transport: auth.NewRoundTripper(nil, auth.NewStaticProvider(""))}
	rt := newRateLimitTransport(&clientTransport{c: inner}, nil)
	rt.now = func() time.Time { return fixedNow }
	rt.sleep = sleeper.record

	// Rebuild the go-github client around rt rather than patching the one New made: Client()
	// hands back a clone, so mutating its Transport would be silently dropped.
	c := New(auth.NewStaticProvider(""))
	c.gh = github.NewClient(&http.Client{Transport: rt})
	u, _ := url.Parse(srv.URL + "/")
	c.gh.BaseURL = u
	return c, sleeper
}

// A secondary rate limit states its wait outright via Retry-After; the transport takes it and
// replays, and the caller never sees the rejection.
func TestRateLimitRetriesAfterRetryAfter(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit"}`))
			return
		}
		_, _ = w.Write([]byte(`{"number":7,"head":{"sha":"deadbeef"}}`))
	})
	c, sleeper := rateLimitedClient(t, mux)

	sha, err := c.PullRequestHeadSHA(context.Background(), "o", "r", 7)
	if err != nil {
		t.Fatalf("PullRequestHeadSHA: %v", err)
	}
	if sha != "deadbeef" {
		t.Errorf("sha = %q, want deadbeef", sha)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("upstream calls = %d, want 2 (one rejected, one replayed)", got)
	}
	if len(sleeper.waits) != 1 || sleeper.waits[0] != 3*time.Second {
		t.Errorf("waits = %v, want one 3s wait", sleeper.waits)
	}
}

// A primary rate limit reports an exhausted quota and its reset instant instead of Retry-After.
// The wait is the distance to the reset plus a slack for clock skew between us and GitHub.
func TestRateLimitRetriesUntilResetInstant(t *testing.T) {
	var calls atomic.Int32
	reset := fixedNow.Add(10 * time.Second)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"number":7,"head":{"sha":"abc"}}`))
	})
	c, sleeper := rateLimitedClient(t, mux)

	if _, err := c.PullRequestHeadSHA(context.Background(), "o", "r", 7); err != nil {
		t.Fatalf("PullRequestHeadSHA: %v", err)
	}
	want := 10*time.Second + rateLimitSlack
	if len(sleeper.waits) != 1 || sleeper.waits[0] != want {
		t.Errorf("waits = %v, want one %v wait", sleeper.waits, want)
	}
}

// A 403 with no rate-limit headers is an ordinary permission error. Retrying it would burn the
// retry budget and delay a failure that is never going to resolve itself.
func TestRateLimitIgnoresPlainForbidden(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	})
	c, sleeper := rateLimitedClient(t, mux)

	if _, err := c.PullRequestHeadSHA(context.Background(), "o", "r", 7); err == nil {
		t.Fatal("a permission 403 must surface as an error")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("upstream calls = %d, want 1 (no retry)", got)
	}
	if len(sleeper.waits) != 0 {
		t.Errorf("waits = %v, want none", sleeper.waits)
	}
}

// A wait longer than the in-process budget is handed back rather than held: the instance would sit
// idle burning its dispatch deadline, when the task retry can pick it up later for free.
func TestRateLimitDefersWaitsBeyondBudget(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c, sleeper := rateLimitedClient(t, mux)

	if _, err := c.PullRequestHeadSHA(context.Background(), "o", "r", 7); err == nil {
		t.Fatal("an over-budget rate limit must surface as an error, not be swallowed")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("upstream calls = %d, want 1 (no retry)", got)
	}
	if len(sleeper.waits) != 0 {
		t.Errorf("waits = %v, want none", sleeper.waits)
	}
}

// Under a sustained limit the transport gives up after its budget instead of looping: each retry
// would spend the same wait again for the same answer.
func TestRateLimitStopsAfterRetryBudget(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c, sleeper := rateLimitedClient(t, mux)

	if _, err := c.PullRequestHeadSHA(context.Background(), "o", "r", 7); err == nil {
		t.Fatal("an exhausted retry budget must surface the rate-limit error")
	}
	if got, want := int(calls.Load()), maxRateLimitRetries+1; got != want {
		t.Errorf("upstream calls = %d, want %d (initial + %d retries)", got, want, maxRateLimitRetries)
	}
	if len(sleeper.waits) != maxRateLimitRetries {
		t.Errorf("waits = %v, want %d", sleeper.waits, maxRateLimitRetries)
	}
}

// A retried POST must resend its body. go-github buffers the JSON it marshals, so the request
// carries GetBody and rewinds — but nothing enforces that, and a silently empty replayed body
// would post a malformed review rather than fail.
func TestRateLimitReplaysRequestBody(t *testing.T) {
	var bodies []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repos/o/r/pulls/7/reviews", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if len(bodies) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"id":1}`))
	})
	c, _ := rateLimitedClient(t, mux)

	err := c.CreateReview(context.Background(), "o", "r", 7, ReviewInput{
		Body:     "summary",
		Comments: []ReviewComment{{Path: "a.go", Line: 3, Side: "RIGHT", Body: "issue"}},
		CommitID: "sha1",
	})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(bodies))
	}
	if bodies[1] == "" || bodies[1] != bodies[0] {
		t.Errorf("replayed body differs from the original:\nfirst:  %q\nsecond: %q", bodies[0], bodies[1])
	}
	if !strings.Contains(bodies[1], `"commit_id":"sha1"`) {
		t.Errorf("replayed body lost its pinned commit: %s", bodies[1])
	}
}

// A wait must never outlive the deadline paying for it: when the context ends mid-wait the caller
// gets the context error rather than blocking to the end of a reset window.
func TestRateLimitAbandonsWaitOnContextEnd(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c, sleeper := rateLimitedClient(t, mux)
	sleeper.err = context.DeadlineExceeded

	_, err := c.PullRequestHeadSHA(context.Background(), "o", "r", 7)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Duration
		ok    bool
	}{
		{"delta seconds", "12", 12 * time.Second, true},
		{"zero", "0", 0, true},
		{"negative clamps to now", "-5", 0, true},
		{"http date", fixedNow.Add(30 * time.Second).UTC().Format(http.TimeFormat), 30 * time.Second, true},
		{"past http date clamps to now", fixedNow.Add(-time.Hour).UTC().Format(http.TimeFormat), 0, true},
		{"garbage falls through", "soon", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseRetryAfter(tc.value, fixedNow)
			if ok != tc.ok || got != tc.want {
				t.Errorf("parseRetryAfter(%q) = %v, %v; want %v, %v", tc.value, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// A request whose body cannot be rewound is returned as-is rather than replayed empty. Nothing in
// this package builds such a request today; the guard is what keeps that true if one appears.
func TestReplayableRejectsUnrewindableBody(t *testing.T) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.test/x", io.NopCloser(strings.NewReader("body")))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.GetBody = nil // an io.Reader http.NewRequest cannot type-assert leaves GetBody unset
	if _, ok := replayable(req); ok {
		t.Error("a request with no GetBody must not be reported replayable")
	}

	bodyless, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.test/x", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if got, ok := replayable(bodyless); !ok || got != bodyless {
		t.Error("a bodyless request must replay as itself")
	}
}

// sleepCtx reports an already-ended context rather than returning success for a zero wait, so a
// cancelled caller does not get one more attempt it never asked for.
func TestSleepCtx(t *testing.T) {
	if err := sleepCtx(context.Background(), 0); err != nil {
		t.Errorf("zero wait on a live context = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("zero wait on a cancelled context = %v, want context.Canceled", err)
	}
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("long wait on a cancelled context = %v, want context.Canceled", err)
	}
	start := time.Now()
	if err := sleepCtx(context.Background(), 5*time.Millisecond); err != nil {
		t.Errorf("short wait = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed < 5*time.Millisecond {
		t.Errorf("returned after %v, want at least 5ms", elapsed)
	}
}
