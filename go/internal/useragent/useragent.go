// Package useragent gives every outbound HTTP request from this service a stable identifier.
//
// It exists because the default is anonymous: net/http sends "Go-http-client/1.1", which says
// nothing about who is calling, and each vendored SDK sends its own library name, which says
// nothing about the deployment. When a repository owner, a Slack admin, or a GCP project owner
// looks at their logs and asks "what is this traffic?", the answer should be legible without
// correlating timestamps.
package useragent

import (
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
)

// product is the identifier every request carries. Deliberately the service name and nothing
// environment-specific: a User-Agent is not a place for a project id or a hostname.
const product = "automation-agent"

// String returns the User-Agent token for this build, e.g. "automation-agent/v1.2.0". The
// version comes from the module's own build info, so it needs no ldflags and cannot drift from
// what was actually compiled. A binary built outside a module context reports "(devel)", which
// is what go itself reports and more honest than inventing a number.
var String = sync.OnceValue(func() string {
	version := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		version = tokenize(info.Main.Version)
	}
	if version == "" {
		version = "devel"
	}
	return product + "/" + version
})

// tokenize keeps only the characters RFC 9110 allows in a token, because a User-Agent is a
// sequence of product/version tokens and a delimiter in one changes how the whole header parses.
// This is not hypothetical: go reports an untagged build as "(devel)", and parentheses open a
// comment — "automation-agent/(devel)" is a product with *no* version followed by a comment,
// which is not what it looks like. Stripping rather than substituting keeps real versions,
// including pseudo-versions like v0.0.0-20260101120000-abcdef123456, exactly as go reports them.
func tokenize(s string) string {
	const punct = "!#$%&'*+-.^_`|~"
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z',
			strings.ContainsRune(punct, r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Transport wraps base so every request through it identifies this service. A nil base means
// http.DefaultTransport, matching the http.Client convention.
//
// Ours is prepended rather than substituted: a library that sets its own User-Agent is telling
// the server something true about how the request was built (go-github's REST version, go-git's
// protocol support), and a server may key behaviour off it. Space-separated product tokens are
// exactly what RFC 9110 defines a User-Agent to be, so both survive.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{base: base}
}

type transport struct{ base http.RoundTripper }

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Never mutate the caller's request: RoundTripper's contract forbids it, and a retry that
	// replays the original would otherwise accumulate our token once per attempt.
	clone := req.Clone(req.Context())
	clone.Header.Set("User-Agent", apply(req.Header.Get("User-Agent")))
	return t.base.RoundTrip(clone)
}

// apply returns the User-Agent to send given whatever a caller or library already set.
// Unexported: the egress points that cannot use Transport take a plain string or a header map,
// so they call String() and compose it themselves.
func apply(existing string) string {
	if existing == "" {
		return String()
	}
	return String() + " " + existing
}
