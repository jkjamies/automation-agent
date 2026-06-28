// Package ingest defines the normalized event envelope that every ingress
// source (Cloud Scheduler, webhooks, and future hooks like GitHub/Jira/Confluence) is
// reduced to before being handed to the root agent. See .agents/standards/architecture-design.md §2.
package ingest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// Kind identifies what triggered an ingest, so the root agent can route it.
type Kind string

const (
	KindCronDaily Kind = "cron.daily" // daily Cloud Scheduler trigger -> summary digest
	KindLint      Kind = "lint"       // agnostic lint payload -> lint-fixer
	KindCoverage  Kind = "coverage"   // agnostic coverage payload -> coverage-fixer
	KindCI        Kind = "ci"         // GitHub check_run -> resume lint/coverage fixer
)

// Valid reports whether k is a recognized ingest kind.
func (k Kind) Valid() bool {
	switch k {
	case KindCronDaily, KindLint, KindCoverage, KindCI:
		return true
	default:
		return false
	}
}

// Envelope is the normalized unit of work. Payload carries the raw source body
// (e.g. the lint JSON or check_run event) for the chosen workflow to parse.
type Envelope struct {
	Kind       Kind
	Source     string // human-readable origin, e.g. "internal:/cron/daily", "webhook:/lint"
	ReceivedAt time.Time
	Payload    []byte
}

// New constructs an Envelope.
func New(kind Kind, source string, payload []byte, at time.Time) Envelope {
	return Envelope{Kind: kind, Source: source, ReceivedAt: at, Payload: payload}
}

// wireEnvelope is the JSON wire form of an Envelope crossing the task-queue boundary
// (internal/tasks → POST /internal/dispatch). It is an external contract and must stay
// byte-identical across all four language ports (spec §7). Payload is an explicit standard
// base64 string — never a raw []byte — so an empty/absent payload is the empty string in
// every port, with no language-specific null/[]/"" divergence.
type wireEnvelope struct {
	Kind       Kind      `json:"kind"`
	Source     string    `json:"source"`
	ReceivedAt time.Time `json:"received_at"` // RFC 3339
	Payload    string    `json:"payload"`     // standard base64 of the raw bytes ("" when empty)
}

// Encode serializes an envelope to its JSON wire form for the Cloud Tasks transport (the
// in-process transport passes the struct directly and never calls this). See wireEnvelope.
func Encode(e Envelope) ([]byte, error) {
	// Reject an unknown kind at the enqueue boundary so both transports fail the same way:
	// Decode (and POST /internal/dispatch) already drop an unknown kind as a poison task, so
	// without this the cloudtasks backend would enqueue successfully and silently discard the
	// work later, while inprocess would still hand it to the dispatcher.
	if !e.Kind.Valid() {
		return nil, fmt.Errorf("ingest: unknown kind %q", e.Kind)
	}
	b, err := json.Marshal(wireEnvelope{
		Kind:       e.Kind,
		Source:     e.Source,
		ReceivedAt: e.ReceivedAt,
		Payload:    base64.StdEncoding.EncodeToString(e.Payload),
	})
	if err != nil {
		return nil, fmt.Errorf("ingest: encode envelope: %w", err)
	}
	return b, nil
}

// Decode parses an envelope from its JSON wire form and rejects an unknown Kind. A
// malformed body, bad base64, or unrecognized kind is a permanent error (the caller should
// ack the delivery rather than retry it — a redelivery cannot fix a poison payload).
func Decode(b []byte) (Envelope, error) {
	var w wireEnvelope
	if err := json.Unmarshal(b, &w); err != nil {
		return Envelope{}, fmt.Errorf("ingest: decode envelope: %w", err)
	}
	if !w.Kind.Valid() {
		return Envelope{}, fmt.Errorf("ingest: unknown kind %q", w.Kind)
	}
	payload, err := base64.StdEncoding.DecodeString(w.Payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("ingest: decode payload: %w", err)
	}
	return New(w.Kind, w.Source, payload, w.ReceivedAt), nil
}
