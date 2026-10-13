// Package gate holds the deterministic checks that bracket the LLM work: whether a ticket
// is fit to hand to an agent at all (Readiness), and whether the change the agent produced
// is one ticketflow is willing to propose (Diff). Both return human-readable problems; an
// empty result passes.
//
// These are the parts of the pipeline that are not up to a model. A model may judge a
// ticket ready or a diff good, but it never overrules a gate.
package gate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"ticketflow/internal/workspace"
)

// Ticket is what the readiness gate looks at.
type Ticket struct {
	Type        string
	Summary     string
	Description string
	Labels      []string
}

// ReadinessRules configure the readiness gate.
type ReadinessRules struct {
	AllowedTypes   []string // empty allows any type
	BlockLabels    []string // any of these labels blocks the run
	MinDescription int      // minimum description length in characters
}

// DefaultReadinessRules is the conservative default: work items only, a real description,
// and explicit acceptance criteria.
var DefaultReadinessRules = ReadinessRules{
	AllowedTypes:   []string{"Story", "Task", "Bug", "Sub-task", "Subtask"},
	BlockLabels:    []string{"agent-blocked", "needs-design", "security"},
	MinDescription: 80,
}

var acceptancePattern = regexp.MustCompile(`(?im)^\s*(#+\s*)?(acceptance criteria|definition of done)\b|^\s*(given|when|then)\b|^\s*-\s*\[[ x]\]`)

// Readiness reports why t is not ready for unattended implementation. The checks are
// cheap and structural; the planner model makes the judgement call on substance after.
func Readiness(t Ticket, r ReadinessRules) []string {
	var problems []string
	if len(r.AllowedTypes) > 0 && !containsFold(r.AllowedTypes, t.Type) {
		problems = append(problems, fmt.Sprintf("issue type %q is not one ticketflow implements (allowed: %s)", t.Type, strings.Join(r.AllowedTypes, ", ")))
	}
	for _, l := range t.Labels {
		if containsFold(r.BlockLabels, l) {
			problems = append(problems, fmt.Sprintf("label %q keeps this ticket with humans", l))
		}
	}
	if len(strings.TrimSpace(t.Description)) < r.MinDescription {
		problems = append(problems, fmt.Sprintf("the description is under %d characters; describe the expected behaviour", r.MinDescription))
	}
	if !acceptancePattern.MatchString(t.Description) {
		problems = append(problems, `no acceptance criteria found; add an "Acceptance criteria" section, a checklist, or Given/When/Then lines`)
	}
	return problems
}

// RepoPolicy is a target repository's own ticketflow settings, read from
// RepoPolicyFile at the base commit — before the agent touches anything, so a run can
// never loosen the rules it is judged by.
type RepoPolicy struct {
	// Checks are the commands that must pass before a PR opens (build, lint, test). They
	// run with a scrubbed environment inside the checkout.
	Checks []Check `json:"checks"`
	// Forbidden are glob patterns (with ** for any depth) the agent may not touch.
	Forbidden []string `json:"forbidden"`
	// Instructions is extra guidance appended to the agent's prompt.
	Instructions string `json:"instructions"`
	// MaxFiles and MaxChangedLines tighten (never loosen) the service-wide limits.
	MaxFiles        int `json:"max_files"`
	MaxChangedLines int `json:"max_changed_lines"`
}

// Check is one named verification command.
type Check struct {
	Name string   `json:"name"`
	Run  []string `json:"run"`
}

// RepoPolicyFile is where a repository opts in and configures ticketflow.
const RepoPolicyFile = ".ticketflow.json"

// DefaultForbidden are paths no run may change, whatever the repository says: CI
// definitions (an agent that can edit CI can make any check pass, and can reach CI
// secrets), ownership rules, the policy file itself, and obvious secret material.
var DefaultForbidden = []string{
	".github/workflows/**",
	".github/actions/**",
	".github/CODEOWNERS",
	"CODEOWNERS",
	".gitlab-ci.yml",
	RepoPolicyFile,
	"**/*.pem",
	"**/*.key",
	"**/.env",
	"**/.env.*",
}

// ErrNoPolicy means the repository has not opted in.
var ErrNoPolicy = errors.New("gate: repository has no " + RepoPolicyFile)

// LoadRepoPolicy reads RepoPolicyFile from a checkout.
func LoadRepoPolicy(dir string) (RepoPolicy, error) {
	b, err := os.ReadFile(filepath.Join(dir, RepoPolicyFile))
	if errors.Is(err, os.ErrNotExist) {
		return RepoPolicy{}, ErrNoPolicy
	}
	if err != nil {
		return RepoPolicy{}, err
	}
	var p RepoPolicy
	if err := json.Unmarshal(b, &p); err != nil {
		return RepoPolicy{}, fmt.Errorf("gate: parse %s: %w", RepoPolicyFile, err)
	}
	for i, c := range p.Checks {
		if c.Name == "" || len(c.Run) == 0 {
			return RepoPolicy{}, fmt.Errorf("gate: %s: check %d needs a name and a run command", RepoPolicyFile, i)
		}
	}
	if len(p.Checks) == 0 {
		return RepoPolicy{}, fmt.Errorf("gate: %s declares no checks; unverified changes are never proposed", RepoPolicyFile)
	}
	return p, nil
}

// DiffLimits configure the diff gate.
type DiffLimits struct {
	MaxFiles        int
	MaxChangedLines int
	Forbidden       []string
}

// Merge tightens l with a repository policy: the smaller non-zero limit wins and
// forbidden patterns accumulate.
func (l DiffLimits) Merge(p RepoPolicy) DiffLimits {
	out := DiffLimits{
		MaxFiles:        tighter(l.MaxFiles, p.MaxFiles),
		MaxChangedLines: tighter(l.MaxChangedLines, p.MaxChangedLines),
	}
	out.Forbidden = append(append(append(out.Forbidden, DefaultForbidden...), l.Forbidden...), p.Forbidden...)
	return out
}

func tighter(a, b int) int {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case b < a:
		return b
	default:
		return a
	}
}

// Diff reports why a change should not be proposed.
func Diff(d workspace.Diff, l DiffLimits) []string {
	if d.Empty() {
		return []string{"the change is empty; nothing was implemented"}
	}
	var problems []string
	if l.MaxFiles > 0 && len(d.Files) > l.MaxFiles {
		problems = append(problems, fmt.Sprintf("%d files changed (limit %d); the ticket may need splitting", len(d.Files), l.MaxFiles))
	}
	if n := d.ChangedLines(); l.MaxChangedLines > 0 && n > l.MaxChangedLines {
		problems = append(problems, fmt.Sprintf("%d lines changed (limit %d); the ticket may need splitting", n, l.MaxChangedLines))
	}
	for _, f := range d.Files {
		for _, p := range []string{f.Path, f.OldPath} {
			if p == "" {
				continue
			}
			if pat, ok := matchAny(l.Forbidden, p); ok {
				problems = append(problems, fmt.Sprintf("%s is protected (%s) and may not be changed by an agent", p, pat))
			}
		}
		if f.Status == "D" && isTestFile(f.Path) {
			problems = append(problems, fmt.Sprintf("test file %s was deleted; agents may add or fix tests, not remove them", f.Path))
		}
	}
	problems = append(problems, scanSecrets(d.Patch)...)
	return problems
}

var testFile = regexp.MustCompile(`(_test\.go|\.test\.[jt]sx?|\.spec\.[jt]sx?|Test\.(java|kt|swift)|Tests?\.(cs|swift)|_test\.py|(^|/)test_[^/]+\.py)$`)

func isTestFile(p string) bool { return testFile.MatchString(p) }

var secretPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"AWS access key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"GitHub token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`)},
	{"Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`)},
	{"Slack token", regexp.MustCompile(`\bxox[abpr]-[A-Za-z0-9-]{10,}\b`)},
	{"Atlassian API token", regexp.MustCompile(`\bATATT[A-Za-z0-9_\-=]{20,}\b`)},
}

// scanSecrets flags credential-shaped strings on added lines only, so a change that
// removes a leaked secret is not itself blocked.
func scanSecrets(patch string) []string {
	var problems []string
	seen := map[string]bool{}
	for _, line := range strings.Split(patch, "\n") {
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		for _, sp := range secretPatterns {
			if !seen[sp.name] && sp.re.MatchString(line) {
				seen[sp.name] = true
				problems = append(problems, "the change adds what looks like a "+sp.name)
			}
		}
	}
	return problems
}

// matchAny reports the first pattern matching p. Patterns are path.Match globs where a
// "**" segment matches any number of directories.
func matchAny(patterns []string, p string) (string, bool) {
	for _, pat := range patterns {
		if globMatch(pat, p) {
			return pat, true
		}
	}
	return "", false
}

func globMatch(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if matchSegments(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], name[0]); err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}
