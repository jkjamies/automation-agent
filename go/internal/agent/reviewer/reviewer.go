// Package reviewer is the in-house PR code-review workflow. It reacts to GitHub
// pull_request events (a native-event kickoff delivered to /webhooks/github, routed as
// ingest.KindReview) and is being built to post a CodeRabbit-style review: per-category
// sub-agent findings, a count-based scorecard, inline comments with suggestions, and an
// advisory agent-review check.
//
// Unlike the lint/coverage fixers, the reviewer is not a suspend/resume fix loop: it is
// mostly one-shot per pull_request event and does not park on await_ci. Its long LLM
// compute runs in-request via the execution transport (KindReview → /internal/dispatch),
// so CPU stays allocated on Cloud Run.
//
// This is the ingress slice: the engine is wired end-to-end and gated by REVIEW_ENABLED,
// while the review work itself (fetch the diff via the GitHub API, fan out the category
// sub-agents, score, publish) lands in later changes. When those ADK sub-agents arrive,
// their pure wiring moves into an agents_setup.go (the build-agent pattern); until then this
// engine mirrors the lint/coverage fixers — a NewEngine constructor plus its logic in one
// file. See specs/20260625-pr-code-review-agent.md.
package reviewer

import (
	"context"
	"log/slog"
)

// Deps wires the reviewer engine.
type Deps struct {
	// Enabled is the REVIEW_ENABLED kill switch. When false the engine accepts and
	// acknowledges pull_request events but does no review work — the default and the
	// rollback posture.
	Enabled bool
	// Log receives the engine's diagnostics; nil falls back to slog.Default.
	Log *slog.Logger
}

// Engine runs the PR code-review workflow for one pull_request event.
type Engine struct {
	enabled bool
	log     *slog.Logger
}

// NewEngine builds the reviewer engine from its dependencies.
func NewEngine(d Deps) *Engine {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	return &Engine{enabled: d.Enabled, log: log}
}

// Kickoff handles one pull_request webhook delivery (ingest.KindReview). The root
// dispatcher calls it with the raw event payload; it runs in-request via the execution
// transport, so its (eventual) multi-minute LLM compute keeps CPU allocated.
//
// Ingress slice: when disabled (REVIEW_ENABLED=false, the default) it no-ops, so the
// feature is dark by default and REVIEW_ENABLED is the kill switch. When enabled it logs
// receipt; parsing the event, fetching the diff via the GitHub API, fanning out the
// category sub-agents, scoring, and publishing the review land in later changes.
func (e *Engine) Kickoff(ctx context.Context, raw []byte) error {
	_ = ctx
	if !e.enabled {
		e.log.Debug("reviewer disabled (REVIEW_ENABLED=false); ignoring pull_request event", "bytes", len(raw))
		return nil
	}
	e.log.Info("review kickoff received", "bytes", len(raw))
	return nil
}
