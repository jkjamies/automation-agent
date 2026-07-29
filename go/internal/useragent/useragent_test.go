package useragent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStringNamesTheServiceAndAVersion(t *testing.T) {
	got := String()
	name, version, ok := strings.Cut(got, "/")
	if !ok {
		t.Fatalf("User-Agent %q must be a product/version token", got)
	}
	if name != product {
		t.Errorf("product = %q, want %q — this is the identifier a log reader greps for", name, product)
	}
	if version == "" {
		t.Errorf("version half of %q is empty", got)
	}
	// A User-Agent with whitespace or control characters in a token is malformed, and some
	// servers reject the request rather than the header.
	if strings.ContainsAny(got, " \t\r\n") {
		t.Errorf("User-Agent %q must be a single token", got)
	}
	// Memoized, so repeated calls on every request cannot disagree.
	if again := String(); again != got {
		t.Errorf("String() is not stable: %q then %q", got, again)
	}
}

// A library that set its own User-Agent is telling the server something true about how the
// request was built, so ours is added to it rather than replacing it.
func TestTransportPrependsToAnExistingAgent(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("User-Agent")
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name string
		set  string
		want string
	}{
		{"no existing agent", "", String()},
		{"library agent kept", "go-github/v78.0.0", String() + " go-github/v78.0.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			if tc.set != "" {
				req.Header.Set("User-Agent", tc.set)
			}
			resp, err := (&http.Client{Transport: Transport(nil)}).Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			resp.Body.Close()
			if seen != tc.want {
				t.Errorf("server saw %q, want %q", seen, tc.want)
			}
		})
	}
}

// RoundTrippers must not mutate the request they are given. Beyond the contract, the rate-limit
// transport replays the original request on a retry — mutating it would append our token once
// per attempt.
func TestTransportDoesNotMutateTheCallersRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("User-Agent", "caller/1.0")
	rt := Transport(nil)
	for i := 0; i < 3; i++ {
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		resp.Body.Close()
	}
	if got := req.Header.Get("User-Agent"); got != "caller/1.0" {
		t.Errorf("caller's request was mutated: User-Agent = %q after 3 attempts", got)
	}
}

// A nil base means http.DefaultTransport, matching the http.Client convention, so callers that
// have no transport of their own do not have to construct one.
func TestTransportNilBaseUsesTheDefault(t *testing.T) {
	if got := Transport(nil); got == nil {
		t.Fatal("Transport(nil) must return a usable RoundTripper")
	}
	base := http.DefaultTransport
	wrapped, ok := Transport(base).(*transport)
	if !ok {
		t.Fatalf("Transport returned %T, want *transport", Transport(base))
	}
	if wrapped.base != base {
		t.Error("an explicit base must be preserved, not replaced by the default")
	}
}

func TestApply(t *testing.T) {
	if got := apply(""); got != String() {
		t.Errorf("apply(\"\") = %q, want %q", got, String())
	}
	if got := apply("x/1"); got != String()+" x/1" {
		t.Errorf("apply(\"x/1\") = %q", got)
	}
}
