// Package ingest defines the normalized event envelope that every ingress
// source (Cloud Scheduler, webhooks, and future hooks like GitHub/Jira/Confluence) is
// reduced to before being handed to the root agent. See okf/standards/architecture-design.md §2.
package ingest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// Kind identifies what triggered an ingest, so the root agent can route it.
type Kind string

// The recognized ingest kinds. Each maps to exactly one workflow in the root dispatcher;
// a new ingress source adds a Kind here rather than a new path.
const (
	KindCronDaily Kind = "cron.daily" // daily Cloud Scheduler trigger -> summary digest
	KindLint      Kind = "lint"       // agnostic lint payload -> lint-fixer
	KindCoverage  Kind = "coverage"   // agnostic coverage payload -> coverage-fixer
	KindCI        Kind = "ci"         // GitHub check_run -> resume lint/coverage fixer
	KindReview    Kind = "review"     // GitHub pull_request -> PR code-review agent
)

// Valid reports whether k is a recognized ingest kind.
func (k Kind) Valid() bool {
	switch k {
	case KindCronDaily, KindLint, KindCoverage, KindCI, KindReview:
		return true
	default:
		return false
	}
}

// The two size limits every ingress and transport shares. They exist as a pair because the
// raw body a source POSTs and the wire envelope that carries it are different sizes, and
// capping the wrong one turns a permanent failure into an infinite retry: a body that
// passes ingress but cannot be enqueued fails the same way forever, while the source keeps
// retrying a 5xx it can never get past.
const (
	// MaxEncodedBytes is the largest wire-form envelope the execution transport will carry
	// — the Cloud Tasks HTTP-target task limit. /internal/dispatch receives a body of this
	// shape, so it reads up to this, not MaxPayloadBytes.
	MaxEncodedBytes = 1 << 20 // 1 MiB

	// MaxPayloadBytes is the largest raw source body guaranteed to still fit in
	// MaxEncodedBytes once Encode base64s it and wraps it in the envelope JSON. Ingress
	// routes cap on this and reject a larger body with 413 (permanent, the caller must
	// send less) rather than accepting it and failing at enqueue with a retryable 5xx.
	//
	// Derivation: base64 costs 4 bytes per 3, and 750 KiB divides by 3 exactly, so the
	// encoded payload is 768000/3*4 = 1,024,000 bytes. That leaves ~24 KiB for the
	// envelope's other fields — kind, source, an RFC 3339 timestamp, and the JSON
	// punctuation — which run to a few hundred bytes. TestMaxPayloadFitsInATask keeps the
	// two constants honest.
	MaxPayloadBytes = 750 << 10 // 750 KiB
)

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
