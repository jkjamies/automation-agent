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
// The flow per pull_request event: parse it, apply the trigger and skip rules, fetch the changed
// files via the REST API, filter generated/vendored churn, and apply the two-dimensional size gate
// to reach a decision (skip / deny / review). A review fans out the category lenses + glue pass,
// scores the findings (count-based scorecard), and publishes a CodeRabbit-style review via REST —
// inline comments for in-diff actionable findings, a marker-updated summary comment, and an
// advisory agent-review check, reconciled against the PR's existing comments (keep/add/minimize-
// outdated), and steered off the reviewed repo's own standards (.agents/standards, .cursor/rules,
// CLAUDE.md, …) when present. Deny publishes the "too large, please split" summary + a neutral
// check. Still to come: reply-to-reply threading (incremental re-review is intentionally deferred).
// See specs/20260625-pr-code-review-agent.md.
package reviewer

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"google.golang.org/adk/model"

	"automation-agent/internal/githubapi"
)

// ownBranchPrefix marks branches the agents create (lint/coverage fixers push to
// automation-agent/...). The reviewer skips PRs from these branches so it never reviews the
// fixers' own PRs in a loop (spec Decision 19). It mirrors the AGENT_PR_LABEL namespace.
const ownBranchPrefix = "automation-agent/"

// gitHubClient is the slice of *githubapi.Client the reviewer needs: read the changed files
// (with patches) and publish the review (an advisory pull-request review with inline comments,
// the marker-updated summary comment, and the advisory agent-review check). A local interface
// keeps the engine testable with a fake.
type gitHubClient interface {
	ListPRFiles(ctx context.Context, owner, repo string, number int) ([]githubapi.PRFile, error)
	CreateReview(ctx context.Context, owner, repo string, number int, in githubapi.ReviewInput) error
	UpsertMarkerComment(ctx context.Context, owner, repo string, number int, marker, body string) error
	CreateCheckRun(ctx context.Context, owner, repo string, in githubapi.CheckRunInput) error
	// ListReviewComments returns the PR's existing inline comments, and MinimizeComment collapses
	// one as outdated. Together they let a re-review reconcile against GitHub itself (no local
	// store): keep findings that still apply, add new ones, and minimize comments now fixed.
	ListReviewComments(ctx context.Context, owner, repo string, number int) ([]githubapi.ReviewCommentRef, error)
	MinimizeComment(ctx context.Context, subjectID string) error
	// AgentCheck reports whether the agent-review check already exists for the head SHA. Reconciliation
	// makes the comments idempotent, but the check run and summary are not — so a redelivered task for
	// an already-published SHA skips re-posting (a re-push has a new SHA and still reconciles).
	AgentCheck(ctx context.Context, owner, repo, ref, checkName string) (githubapi.CheckResult, error)
	// PullRequestHeadSHA returns the PR's current head SHA, so a review task superseded by a newer
	// push (Cloud Tasks gives no ordering) is skipped at execution rather than posting a stale review.
	PullRequestHeadSHA(ctx context.Context, owner, repo string, number int) (string, error)
	// Tree lists the reviewed repo's files at the head SHA so standards-aware review can discover
	// the repo's own convention docs; GetFileContent then fetches the matched ones. No clone.
	Tree(ctx context.Context, owner, repo, ref string) ([]githubapi.TreeEntry, bool, error)
	GetFileContent(ctx context.Context, owner, repo, path, ref string) (string, error)
}

// Deps wires the reviewer engine.
type Deps struct {
	// Enabled is the REVIEW_ENABLED kill switch. When false the engine accepts and
	// acknowledges pull_request events but does no review work — the default and the
	// rollback posture.
	Enabled bool
	// GH fetches the PR's changed files (REST). Required when Enabled.
	GH gitHubClient
	// BaseLLM / CodeLLM are the base- and code-reasoning tier models the category lenses run
	// on (model-size-split). Required when Enabled.
	BaseLLM model.LLM
	CodeLLM model.LLM
	// MinConfidence drops findings below this confidence before scoring (REVIEW_MIN_CONFIDENCE,
	// the phase-1 verify gate). A non-positive value keeps everything.
	MinConfidence float64
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
	// StandardsEnabled toggles standards-aware review (REVIEW_STANDARDS, default true): discover the
	// reviewed repo's own convention docs, distill them, and steer the lenses off them.
	StandardsEnabled bool
	// StandardsGlobs are the discovery globs matched against the reviewed repo's tree
	// (REVIEW_STANDARDS_GLOBS). StandardsMaxBytes caps the total doc bytes fed to the distiller
	// (REVIEW_STANDARDS_MAX_BYTES).
	StandardsGlobs    []string
	StandardsMaxBytes int
	// UncitedDrop, when true (REVIEW_UNCITED_MODE=drop), drops a conformance finding that cites no
	// real repo rule; otherwise (default) it is demoted to nitpick.
	UncitedDrop bool
	// Log receives the engine's diagnostics; nil falls back to slog.Default.
	Log *slog.Logger
}

// Engine runs the PR code-review workflow for one pull_request event.
type Engine struct {
	enabled       bool
	gh            gitHubClient
	baseLLM       model.LLM
	codeLLM       model.LLM
	minConfidence float64
	skipDrafts    bool
	filter        *fileFilter
	maxFiles      int
	maxDiffBytes  int

	standardsEnabled  bool
	standardsGlobs    []string
	standardsMaxBytes int
	uncitedMode       uncitedMode
	standardsCache    *standardsCache

	log *slog.Logger
}

// NewEngine builds the reviewer engine from its dependencies, compiling the exclude globs
// up front so each event reuses one matcher.
func NewEngine(d Deps) *Engine {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	mode := uncitedNitpick
	if d.UncitedDrop {
		mode = uncitedDrop
	}
	return &Engine{
		enabled:           d.Enabled,
		gh:                d.GH,
		baseLLM:           d.BaseLLM,
		codeLLM:           d.CodeLLM,
		minConfidence:     clampThreshold(d.MinConfidence),
		skipDrafts:        d.SkipDrafts,
		filter:            newFileFilter(d.ExcludeGlobs),
		maxFiles:          d.MaxFiles,
		maxDiffBytes:      d.MaxDiffBytes,
		standardsEnabled:  d.StandardsEnabled,
		standardsGlobs:    d.StandardsGlobs,
		standardsMaxBytes: d.StandardsMaxBytes,
		uncitedMode:       mode,
		standardsCache:    newStandardsCache(),
		log:               log,
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
	// An enabled engine needs a client to fetch the diff and publish (both deny and review use
	// it); without it, return a controlled error rather than dereferencing a nil dependency (a
	// wiring mistake, not a per-event condition — the dispatch retry surfaces it in logs). The
	// tier models are validated in the review branch below, since a deny publishes no model call.
	if e.gh == nil {
		return fmt.Errorf("reviewer: enabled but GitHub client not configured")
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
	// owner/repo are only used by the publish paths; decide() already validated the full name
	// before reaching a deny/review decision, so the discarded ok is safe there.
	owner, repo, _ := splitFullName(ev.RepoFullName)
	// Coalesce-to-latest: a deny/review acts on the event's SHA, so if a newer push has superseded
	// it (debounce only narrows the window — Cloud Tasks has no ordering), skip rather than post a
	// stale review. A skip decision produced nothing, so it needs no check.
	if d.kind != decisionSkip && e.superseded(ctx, owner, repo, ev) {
		e.log.Info("stale review skipped (superseded by a newer push)", "pr", pr, "event_sha", ev.HeadSHA)
		return nil
	}
	meta := publishMeta{owner: owner, repo: repo, number: ev.Number, headSHA: ev.HeadSHA, files: d.files, tiers: "code-reasoning + base"}
	switch d.kind {
	case decisionSkip:
		e.log.Info("review skipped", "pr", pr, "action", ev.Action, "reason", d.reason)
	case decisionDeny:
		// Too large to review: post the "please split" summary + a neutral check, no model call.
		if err := e.publishDeny(ctx, meta, d.reason, len(d.files), d.diffBytes); err != nil {
			return err
		}
		e.log.Info("review denied", "pr", pr, "files", len(d.files), "diff_bytes", d.diffBytes, "reason", d.reason)
	case decisionReview:
		// Review needs both tier models; the deny branch above does not, so validate them here.
		if e.baseLLM == nil || e.codeLLM == nil {
			return fmt.Errorf("reviewer: enabled but review models not configured")
		}
		// Steer the lenses off the reviewed repo's own conventions (standards-aware review); nil
		// when disabled or none found, in which case the lenses review generically.
		std := e.discoverStandards(ctx, owner, repo, ev.HeadSHA, d.files)
		meta.standards = std.sourceList()
		// Fan out the category lenses + glue pass, score, then publish the review.
		card, findings, err := e.review(ctx, d.files, std)
		if err != nil {
			return err
		}
		if err := e.publish(ctx, card, findings, meta); err != nil {
			return err
		}
		e.log.Info("review published", "pr", pr, "files", len(d.files), "overall", card.overall.String(), "findings", card.total)
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

// superseded reports whether a newer push has replaced the SHA this task was enqueued for, so the
// review is stale and should be skipped (coalesce-to-latest). It is best-effort: a missing event
// SHA or a lookup error yields false (proceed) so a transient failure never suppresses a real
// review.
func (e *Engine) superseded(ctx context.Context, owner, repo string, ev githubapi.PullRequestEvent) bool {
	if ev.HeadSHA == "" {
		return false
	}
	current, err := e.gh.PullRequestHeadSHA(ctx, owner, repo, ev.Number)
	if err != nil {
		e.log.Warn("could not fetch current head SHA; proceeding with review", "pr", ev.RepoFullName, "err", err)
		return false
	}
	return current != "" && current != ev.HeadSHA
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
