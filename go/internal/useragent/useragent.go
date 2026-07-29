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
	version := "(devel)"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		version = info.Main.Version
	}
	return product + "/" + version
})

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
