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
// This change is the deterministic intake pipeline: parse the event, apply the trigger and
// skip rules, fetch the changed files via the REST API, filter generated/vendored churn, and
// apply the two-dimensional size gate — producing a decision (skip / deny / review). The
// review work itself (fan out the category sub-agents, score, publish) and posting the deny
// comment land in later changes; when the ADK sub-agents arrive, their pure wiring moves into
// an agents_setup.go (the build-agent pattern). See specs/20260625-pr-code-review-agent.md.
package reviewer

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"automation-agent/internal/githubapi"
)

// ownBranchPrefix marks branches the agents create (lint/coverage fixers push to
// automation-agent/...). The reviewer skips PRs from these branches so it never reviews the
// fixers' own PRs in a loop (spec Decision 19). It mirrors the AGENT_PR_LABEL namespace.
const ownBranchPrefix = "automation-agent/"

// gitHubClient is the narrow slice of *githubapi.Client the reviewer needs — the changed
// files (with patches) for a PR. A local interface keeps the engine testable with a fake.
type gitHubClient interface {
	ListPRFiles(ctx context.Context, owner, repo string, number int) ([]githubapi.PRFile, error)
}

// Deps wires the reviewer engine.
type Deps struct {
	// Enabled is the REVIEW_ENABLED kill switch. When false the engine accepts and
	// acknowledges pull_request events but does no review work — the default and the
	// rollback posture.
	Enabled bool
	// GH fetches the PR's changed files (REST). Required when Enabled.
	GH gitHubClient
	// SkipDrafts skips draft PRs unless the triggering action is ready_for_review
	// (REVIEW_SKIP_DRAFTS, default true).
	SkipDrafts bool
	// ExcludeGlobs drops generated/vendored/lockfile/minified/binary paths before sizing and
	// review (REVIEW_EXCLUDE_GLOBS).
	ExcludeGlobs []string
	// MaxFiles / MaxDiffBytes are the two-dimensional size-gate caps (REVIEW_MAX_FILES,
	// REVIEW_MAX_DIFF_BYTES); a non-positive value disables that dimension.
	MaxFiles     int
	MaxDiffBytes int
	// Log receives the engine's diagnostics; nil falls back to slog.Default.
	Log *slog.Logger
}

// Engine runs the PR code-review workflow for one pull_request event.
type Engine struct {
	enabled      bool
	gh           gitHubClient
	skipDrafts   bool
	filter       *fileFilter
	maxFiles     int
	maxDiffBytes int
	log          *slog.Logger
}

// NewEngine builds the reviewer engine from its dependencies, compiling the exclude globs
// up front so each event reuses one matcher.
func NewEngine(d Deps) *Engine {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		enabled:      d.Enabled,
		gh:           d.GH,
		skipDrafts:   d.SkipDrafts,
		filter:       newFileFilter(d.ExcludeGlobs),
		maxFiles:     d.MaxFiles,
		maxDiffBytes: d.MaxDiffBytes,
		log:          log,
	}
}

// decisionKind is the outcome of intake for one pull_request event.
type decisionKind int

const (
	decisionSkip   decisionKind = iota // not reviewable (trigger/skip rule or empty diff)
	decisionDeny                       // reviewable but too large — deny, don't degrade
	decisionReview                     // proceed to review the kept files
)

// decision is the result of the intake pipeline. files/diffBytes are the filtered review
// surface (set for deny and review); reason explains a skip or a deny.
type decision struct {
	kind      decisionKind
	reason    string
	files     []githubapi.PRFile
	diffBytes int
}

// Kickoff handles one pull_request webhook delivery (ingest.KindReview). The root dispatcher
// calls it with the raw event payload; it runs in-request via the execution transport, so its
// (eventual) multi-minute LLM compute keeps CPU allocated.
//
// When disabled (REVIEW_ENABLED=false, the default) it no-ops, so the feature is dark by
// default and REVIEW_ENABLED is the kill switch. When enabled it runs intake and logs the
// decision; fanning out the category sub-agents, scoring, and publishing the review (and the
// deny comment) land in later changes.
func (e *Engine) Kickoff(ctx context.Context, raw []byte) error {
	if !e.enabled {
		e.log.Debug("reviewer disabled (REVIEW_ENABLED=false); ignoring pull_request event", "bytes", len(raw))
		return nil
	}
	// An enabled engine needs a client to fetch the diff; without one, return a controlled
	// error rather than letting decide dereference a nil client (a wiring mistake, not a
	// per-event condition — the dispatch retry surfaces it in logs).
	if e.gh == nil {
		return fmt.Errorf("reviewer: enabled but no GitHub client configured")
	}
	ev, err := githubapi.ParsePullRequestEvent(raw)
	if err != nil {
		return fmt.Errorf("reviewer: %w", err)
	}
	d, err := e.decide(ctx, ev)
	if err != nil {
		return err
	}
	pr := ev.RepoFullName + "#" + strconv.Itoa(ev.Number)
	switch d.kind {
	case decisionSkip:
		e.log.Info("review skipped", "pr", pr, "action", ev.Action, "reason", d.reason)
	case decisionDeny:
		// Posting the "too large — please split" comment + a neutral agent-review check lands
		// in the publish change; intake only decides.
		e.log.Info("review denied", "pr", pr, "files", len(d.files), "diff_bytes", d.diffBytes, "reason", d.reason)
	case decisionReview:
		// Category fan-out, scorecard, and publishing land in later changes.
		e.log.Info("review planned", "pr", pr, "files", len(d.files), "diff_bytes", d.diffBytes)
	}
	return nil
}

// decide runs the deterministic intake pipeline for one event: trigger gate → skip rules →
// fetch files → filter → size gate. It performs no model calls and posts nothing.
func (e *Engine) decide(ctx context.Context, ev githubapi.PullRequestEvent) (decision, error) {
	// Trigger gate: only these actions warrant a review (spec Decision 19).
	switch ev.Action {
	case "opened", "reopened", "synchronize", "ready_for_review":
	default:
		return skip("action %q is not a reviewed trigger", ev.Action), nil
	}
	// Draft: skip unless this is the ready_for_review transition (REVIEW_SKIP_DRAFTS).
	if e.skipDrafts && ev.Draft && ev.Action != "ready_for_review" {
		return skip("draft PR (REVIEW_SKIP_DRAFTS)"), nil
	}
	// Own PR: never review the fixers' own automation-agent/* branches (avoids a review loop).
	if strings.HasPrefix(ev.HeadRef, ownBranchPrefix) {
		return skip("agent's own PR (head %q)", ev.HeadRef), nil
	}
	// Opt-out label.
	for _, l := range ev.Labels {
		if l == "skip-review" {
			return skip("skip-review label"), nil
		}
	}
	// Dependency-bot PRs are noise for a code reviewer.
	if isDependencyBot(ev.AuthorLogin) {
		return skip("dependency-bot PR (%s)", ev.AuthorLogin), nil
	}

	owner, repo, ok := splitFullName(ev.RepoFullName)
	if !ok {
		return decision{}, fmt.Errorf("reviewer: malformed repository full name %q", ev.RepoFullName)
	}
	files, err := e.gh.ListPRFiles(ctx, owner, repo, ev.Number)
	if err != nil {
		return decision{}, fmt.Errorf("reviewer: list PR files: %w", err)
	}
	kept, diffBytes := e.filter.apply(files)
	// Empty filtered diff: nothing reviewable after dropping generated/vendored churn.
	if len(kept) == 0 {
		return skip("no reviewable files after exclude filter (%d changed)", len(files)), nil
	}
	if reason, denied := oversize(len(kept), diffBytes, e.maxFiles, e.maxDiffBytes); denied {
		return decision{kind: decisionDeny, reason: reason, files: kept, diffBytes: diffBytes}, nil
	}
	return decision{kind: decisionReview, files: kept, diffBytes: diffBytes}, nil
}

// skip builds a decisionSkip with a formatted reason.
func skip(format string, args ...any) decision {
	return decision{kind: decisionSkip, reason: fmt.Sprintf(format, args...)}
}

// isDependencyBot reports whether the author is a known dependency-update bot (spec Decision
// 19). GitHub Apps post as "<name>[bot]".
func isDependencyBot(login string) bool {
	switch login {
	case "dependabot[bot]", "renovate[bot]":
		return true
	default:
		return false
	}
}

// splitFullName splits an "owner/name" repository full name. It reports false for anything
// that is not exactly one owner and one non-empty name.
func splitFullName(full string) (owner, repo string, ok bool) {
	owner, repo, ok = strings.Cut(full, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", false
	}
	return owner, repo, true
}
