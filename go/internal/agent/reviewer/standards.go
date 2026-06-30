package reviewer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"automation-agent/internal/agent/setup"
	"automation-agent/internal/githubapi"
)

// Standards-aware review (spec Decision 14): the reviewer steers off the conventions of the repo
// *under review* — `.agents/standards`, `.cursor/rules`, `CLAUDE.md`, whatever that repo has, not
// automation-agent's own. A base-tier sub-agent distills the discovered docs (heterogeneous
// formats) into one uniform tagged rule list; the compact list is injected into every lens and a
// lazy get_rule tool serves the full text on demand. All API-only (no clone).

// Rule is one distilled, dimension-tagged convention rule extracted from the reviewed repo's own
// standards docs.
type Rule struct {
	ID        string
	Dimension Dimension
	Summary   string
	Source    string // the doc path the rule came from
}

// standards is the distilled rule set for one repo at one docs revision: the compact rule menu
// injected into every lens, plus the full source docs for lazy get_rule drill-down.
type standards struct {
	rules   []Rule
	byID    map[string]Rule
	docs    map[string]string // source path -> full doc text
	sources []string          // distinct source paths, sorted (for the summary report)
}

// empty reports whether there are no rules to inject (nil receiver included), so callers can fall
// back to a generic review.
func (s *standards) empty() bool { return s == nil || len(s.rules) == 0 }

// menu renders the compact rule list for an agent prompt: one line per rule (id, dimension,
// summary, source). Small by construction — summaries, not full text.
func (s *standards) menu() string {
	if s.empty() {
		return ""
	}
	var b strings.Builder
	for _, r := range s.rules {
		fmt.Fprintf(&b, "- %s [%s] %s (source: %s)\n", r.ID, r.Dimension, r.Summary, r.Source)
	}
	return b.String()
}

// validID reports whether id is a rule in this set (the citation gate's check).
func (s *standards) validID(id string) bool {
	if s == nil {
		return false
	}
	_, ok := s.byID[id]
	return ok
}

// ruleDoc returns the full source-doc text for a rule id, for lazy drill-down. Empty if the id is
// unknown or its source doc is absent.
func (s *standards) ruleDoc(id string) string {
	if s == nil {
		return ""
	}
	r, ok := s.byID[id]
	if !ok {
		return ""
	}
	return s.docs[r.Source]
}

// sourceList returns the applied source paths (nil when no standards), for the summary report.
func (s *standards) sourceList() []string {
	if s.empty() {
		return nil
	}
	return s.sources
}

// discoverStandards fetches and distills the reviewed repo's convention docs into a tagged rule
// list, cached per repo + docs revision. It returns nil (review generic) when standards are
// disabled, none are found, or distillation yields nothing. Best-effort: a discovery/fetch error
// logs and returns nil rather than failing the review.
func (e *Engine) discoverStandards(ctx context.Context, owner, repo, ref string, changed []githubapi.PRFile) *standards {
	if !e.standardsEnabled {
		return nil
	}
	entries, truncated, err := e.gh.Tree(ctx, owner, repo, ref)
	if err != nil {
		e.log.Warn("standards: list tree failed; reviewing generic", "repo", owner+"/"+repo, "err", err)
		return nil
	}
	if truncated {
		// A truncated tree (very large repo) may have missed deep convention files; proceed with
		// what we have rather than failing, but surface the gap (no silent caps).
		e.log.Warn("standards: repo tree truncated; discovery may be incomplete", "repo", owner+"/"+repo)
	}
	// Per-module scoping (Decision 14): a per-directory instruction file applies only when the PR
	// touches its module, so a finding in one module isn't judged against another's conventions and
	// the rule set stays relevant + compact. Repo-global conventions always apply.
	matched := scopeToTouched(matchStandards(entries, e.standardsGlobs), changed)
	if len(matched) == 0 {
		return nil
	}
	// Cache on the matched docs' blob SHAs, so distillation runs once per standards change: a
	// changed standards file changes its blob SHA → cache miss → re-distill.
	key := standardsCacheKey(owner, repo, matched)
	if cached, ok := e.standardsCache.get(key); ok {
		return cached
	}

	docs := map[string]string{}
	var sources []string
	var total int
	fetchOK := true
	for _, m := range matched {
		content, err := e.gh.GetFileContent(ctx, owner, repo, m.Path, ref)
		if err != nil {
			// A transient fetch failure must not poison the cache for this revision; use what we
			// have this round but don't memoize, so a later event retries the full set.
			e.log.Warn("standards: fetch failed; skipping doc", "path", m.Path, "err", err)
			fetchOK = false
			continue
		}
		if total+len(content) > e.standardsMaxBytes {
			e.log.Warn("standards: byte cap reached; remaining docs skipped", "cap", e.standardsMaxBytes, "applied", len(sources))
			break
		}
		total += len(content)
		docs[m.Path] = content
		sources = append(sources, m.Path)
	}
	if len(docs) == 0 {
		return nil
	}

	rules, err := e.distill(ctx, docs, sources)
	if err != nil {
		// A transient distiller failure — do not memoize; the next event re-distills.
		e.log.Warn("standards: distillation failed; reviewing generic", "repo", owner+"/"+repo, "err", err)
		return nil
	}
	std := buildStandards(rules, docs, sources)
	if fetchOK {
		// Cache only a fully-successful build (incl. a legitimate empty distill, so a rule-less repo
		// isn't re-distilled until its docs change); a partial fetch is left uncached to retry.
		e.standardsCache.put(key, std)
	}
	if std.empty() {
		e.log.Info("standards: discovered docs but distilled no rules; reviewing generic", "repo", owner+"/"+repo, "docs", len(sources))
		return nil
	}
	e.log.Info("standards: applied", "repo", owner+"/"+repo, "rules", len(std.rules), "sources", strings.Join(std.sources, ", "))
	return std
}

// matchStandards returns the tree's blob entries whose path matches any standards glob, sorted by
// path for deterministic ordering and cache keys.
func matchStandards(entries []githubapi.TreeEntry, globs []string) []githubapi.TreeEntry {
	pats := compileStandardsGlobs(globs)
	var out []githubapi.TreeEntry
	for _, en := range entries {
		if en.Type != "blob" {
			continue
		}
		if matchesGlob(pats, en.Path) {
			out = append(out, en)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// compileStandardsGlobs builds path matchers from the configured globs. A glob with no '/' matches
// the basename (e.g. "AGENTS.md", "CLAUDE.md"); one with a '/' matches the full path (e.g.
// ".cursor/rules/**"). Reuses the exclude-filter glob compiler (globPattern/globToRegexp).
func compileStandardsGlobs(globs []string) []globPattern {
	var pats []globPattern
	for _, g := range globs {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		pats = append(pats, globPattern{re: globToRegexp(g), basename: !strings.Contains(g, "/")})
	}
	return pats
}

// matchesGlob reports whether p matches any compiled standards glob.
func matchesGlob(pats []globPattern, p string) bool {
	base := path.Base(p)
	for _, pat := range pats {
		target := p
		if pat.basename {
			target = base
		}
		if pat.re.MatchString(target) {
			return true
		}
	}
	return false
}

// scopeToTouched drops per-directory instruction files (AGENTS.md/CLAUDE.md/GEMINI.md nested below
// the repo root) for modules the PR does not touch — so a finding in one module isn't judged against
// another module's conventions, and a monorepo's many per-module files don't all load for a one-
// module PR. Repo-global conventions (root files, .cursor/.agents/.github rule dirs, linter configs)
// always apply.
func scopeToTouched(matched []githubapi.TreeEntry, changed []githubapi.PRFile) []githubapi.TreeEntry {
	touched := touchedDirs(changed)
	out := matched[:0:0]
	for _, m := range matched {
		if moduleScoped(m.Path) && !touched[path.Dir(m.Path)] {
			continue
		}
		out = append(out, m)
	}
	return out
}

// moduleScoped reports whether a convention file is a per-directory instruction file below the repo
// root (applies only to its own module). Root files and non-instruction conventions (dotfolder
// rules, linter configs) are repo-global.
func moduleScoped(p string) bool {
	if path.Dir(p) == "." {
		return false
	}
	switch path.Base(p) {
	case "AGENTS.md", "CLAUDE.md", "GEMINI.md":
		return true
	default:
		return false
	}
}

// touchedDirs is the set of every ancestor directory (up to the root ".") of the changed files, so
// a per-module instruction file applies when any file in its subtree changed.
func touchedDirs(changed []githubapi.PRFile) map[string]bool {
	dirs := map[string]bool{}
	for _, f := range changed {
		for d := path.Dir(f.Path); ; d = path.Dir(d) {
			dirs[d] = true
			if d == "." {
				break
			}
		}
	}
	return dirs
}

// standardsCacheKey hashes the repo and the matched docs' (path, blob SHA) pairs, so the cache
// keys on the standards revision: any change to a standards file changes its blob SHA and misses.
func standardsCacheKey(owner, repo string, matched []githubapi.TreeEntry) string {
	parts := make([]string, len(matched))
	for i, m := range matched {
		parts[i] = m.Path + ":" + m.SHA
	}
	sort.Strings(parts)
	h := sha256.Sum256([]byte(owner + "/" + repo + "\n" + strings.Join(parts, "\n")))
	return hex.EncodeToString(h[:])
}

// distill runs the base-tier distiller sub-agent over the discovered docs, returning the parsed
// rule list. Best-effort: a runner/drive error logs and returns nil (review generic).
func (e *Engine) distill(ctx context.Context, docs map[string]string, sources []string) ([]Rule, error) {
	a, err := e.buildDistillerAgent(docs, sources)
	if err != nil {
		return nil, fmt.Errorf("build distiller: %w", err)
	}
	r, err := setup.NewRunner("reviewer-distill", a)
	if err != nil {
		return nil, fmt.Errorf("distiller runner: %w", err)
	}
	text, err := setup.DriveText(ctx, r, "system", "distill", distillTrigger)
	if err != nil {
		return nil, fmt.Errorf("distillation: %w", err)
	}
	return parseRules(text), nil
}

// buildDistillerInstruction composes the distiller's instruction: the distill prompt followed by
// each discovered standards doc, fenced so the doc content (untrusted) can't break the prompt.
func buildDistillerInstruction(promptBody string, docs map[string]string, sources []string) string {
	var b strings.Builder
	b.WriteString(promptBody)
	b.WriteString("\n\n## Repository standards documents\n\n")
	for _, src := range sources {
		fmt.Fprintf(&b, "### Document: %s\n\n", src)
		fence := strings.Repeat("`", maxBacktickRun(docs[src])+1)
		if len(fence) < 3 {
			fence = "```"
		}
		b.WriteString(fence + "\n")
		b.WriteString(docs[src])
		if !strings.HasSuffix(docs[src], "\n") {
			b.WriteByte('\n')
		}
		b.WriteString(fence + "\n\n")
	}
	return b.String()
}

// buildStandards assembles the standards from distilled rules + the fetched docs. nil when there
// are no rules (so empty() and a generic fallback hold).
func buildStandards(rules []Rule, docs map[string]string, sources []string) *standards {
	if len(rules) == 0 {
		return nil
	}
	byID := make(map[string]Rule, len(rules))
	for _, r := range rules {
		byID[r.ID] = r
	}
	sorted := append([]string(nil), sources...)
	sort.Strings(sorted)
	return &standards{rules: rules, byID: byID, docs: docs, sources: sorted}
}

// ruleWire is the JSON shape the distiller is prompted to emit.
type ruleWire struct {
	ID        string `json:"id"`
	Dimension string `json:"dimension"`
	Summary   string `json:"summary"`
	Source    string `json:"source"`
}

// parseRules extracts the distilled rule list from the base model's output. Defensive by design
// (mirrors parseFindings): it scans for the first JSON array that decodes into the rule shape,
// tolerating fences/prose, and never errors — a garbled distillation degrades to "no rules" (a
// generic review) rather than failing.
func parseRules(raw string) []Rule {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '[' {
			continue
		}
		var wires []ruleWire
		if err := json.NewDecoder(strings.NewReader(raw[i:])).Decode(&wires); err != nil {
			continue
		}
		if len(wires) == 0 {
			continue
		}
		out := make([]Rule, 0, len(wires))
		seen := map[string]bool{}
		for _, w := range wires {
			id := strings.TrimSpace(w.ID)
			summary := strings.TrimSpace(w.Summary)
			if id == "" || summary == "" || seen[id] {
				continue // a rule needs a unique id and a summary to be usable
			}
			seen[id] = true
			out = append(out, Rule{
				ID:        id,
				Dimension: normalizeDimension(w.Dimension),
				Summary:   summary,
				Source:    strings.TrimSpace(w.Source),
			})
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// getRuleArgs / getRuleResult are the get_rule tool's I/O.
type getRuleArgs struct {
	ID string `json:"id"`
}
type getRuleResult struct {
	Rule string `json:"rule"`
}

// standardsTools returns the lazy get_rule drill-down tool bound to this run's rule set, or nil
// when there are no standards (the lenses then run without it). Same pattern as the fixers'
// read_file tool: the compact rule menu lives in the prompt; full text is fetched on demand.
func standardsTools(std *standards) ([]tool.Tool, error) {
	if std.empty() {
		return nil, nil
	}
	getRule, err := functiontool.New(functiontool.Config{
		Name:        "get_rule",
		Description: "Return the full source text of a repo standard rule by its id (e.g. \"R3\") so you can read the exact wording before flagging a conformance issue.",
	}, func(_ tool.Context, args getRuleArgs) (getRuleResult, error) {
		return getRuleResult{Rule: std.ruleDoc(strings.TrimSpace(args.ID))}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("reviewer: build get_rule tool: %w", err)
	}
	return []tool.Tool{getRule}, nil
}

// conformanceDimensions are the lenses whose findings assert "this violates the repo's documented
// standard" — they must cite a real injected rule_id. Other dimensions (e.g. security, runtime
// safety) stand on their own and need no repo rule.
var conformanceDimensions = map[Dimension]bool{
	DimPatternViolation: true,
	DimArchitecture:     true,
}

// uncitedMode is how an uncited conformance finding is handled (REVIEW_UNCITED_MODE).
type uncitedMode int

const (
	uncitedNitpick uncitedMode = iota // demote to nitpick (default — gentle)
	uncitedDrop                       // drop entirely
)

// gateCitations enforces that a conformance finding (pattern/architecture) is anchored to one of
// the repo's own injected rules: an empty or unknown rule_id is dropped or demoted to nitpick per
// REVIEW_UNCITED_MODE. So a conformance finding only survives at full weight if it cites a real
// repo rule. When standards-awareness is off, findings pass through untouched.
func (e *Engine) gateCitations(findings []Finding, std *standards) []Finding {
	// With no standards discovered, conformance findings have nothing to cite, so gating them would
	// suppress legitimate generic findings — pass everything through.
	if !e.standardsEnabled || std.empty() {
		return findings
	}
	out := findings[:0:0]
	for _, f := range findings {
		if conformanceDimensions[f.Dimension] && !std.validID(f.RuleID) {
			if e.uncitedMode == uncitedDrop {
				continue
			}
			f.Severity = SeverityNitpick // demote: an unanchored "violation" is at most a nitpick
		}
		out = append(out, f)
	}
	return out
}

// standardsCache memoizes distilled rule sets per repo + docs revision (in-memory; a cold start
// re-distills, which is fine). A cached nil means "discovered docs, distilled nothing" and is
// retained so a generic repo isn't re-distilled until its docs change.
type standardsCache struct {
	mu sync.Mutex
	m  map[string]*standards
}

func newStandardsCache() *standardsCache { return &standardsCache{m: map[string]*standards{}} }

func (c *standardsCache) get(key string) (*standards, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.m[key]
	return s, ok
}

func (c *standardsCache) put(key string, s *standards) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = s
}
