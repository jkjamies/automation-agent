// Package ingest defines the normalized event envelope that every ingress
// source (cron, webhooks, and future hooks like GitHub/Jira/Confluence) is
// reduced to before being handed to the root agent. See .agents/standards/architecture-design.md §2.
package ingest

import "time"

// Kind identifies what triggered an ingest, so the root agent can route it.
type Kind string

const (
	KindCronDaily  Kind = "cron.daily"  // 09:00 daily -> summary digest
	KindCronWeekly Kind = "cron.weekly" // 09:00 Monday
	KindLint       Kind = "lint"        // agnostic lint payload -> lint-fixer
	KindCoverage   Kind = "coverage"    // agnostic coverage payload -> coverage-fixer
	KindCI         Kind = "ci"          // GitHub check_run -> resume lint/coverage fixer
)

// Valid reports whether k is a recognized ingest kind.
func (k Kind) Valid() bool {
	switch k {
	case KindCronDaily, KindCronWeekly, KindLint, KindCoverage, KindCI:
		return true
	default:
		return false
	}
}

// Envelope is the normalized unit of work. Payload carries the raw source body
// (e.g. the lint JSON or check_run event) for the chosen workflow to parse.
type Envelope struct {
	Kind       Kind
	Source     string // human-readable origin, e.g. "scheduler", "webhook:/lint"
	ReceivedAt time.Time
	Payload    []byte
}

// New constructs an Envelope.
func New(kind Kind, source string, payload []byte, at time.Time) Envelope {
	return Envelope{Kind: kind, Source: source, ReceivedAt: at, Payload: payload}
}
