// Package fixflow is the reusable engine behind the PR-fixing agents (lint-fixer,
// coverage-fixer, …). It owns the event-driven loop — kickoff → suspend → CI
// resume → loop or finish — plus the apply mechanics and attempt counting. Each
// concrete agent supplies a Spec (a triage fn, an analyze fn, and its branch/label/
// check names). State lives on GitHub; there is no local store (see §8).
package fixflow

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Kickoff is the trusted envelope a CI job posts: repo/base identify where to work
// (reliable), and report is the arbitrary tool output (lint report, coverage report,
// …) that the agent's triage LLM reasons over.
type Kickoff struct {
	Repo   string          `json:"repo"`
	Base   string          `json:"base,omitempty"`
	Report json.RawMessage `json:"report"`
}

// ParseKickoff unmarshals and validates the envelope, applying defaults.
func ParseKickoff(b []byte) (Kickoff, error) {
	var k Kickoff
	if err := json.Unmarshal(b, &k); err != nil {
		return Kickoff{}, fmt.Errorf("parse kickoff: %w", err)
	}
	if err := k.Validate(); err != nil {
		return Kickoff{}, err
	}
	if k.Base == "" {
		k.Base = "main"
	}
	return k, nil
}

// Validate checks the trusted fields; the report is intentionally not parsed.
func (k Kickoff) Validate() error {
	if strings.TrimSpace(k.Repo) == "" {
		return fmt.Errorf("kickoff: repo is required")
	}
	if _, _, ok := splitRepo(k.Repo); !ok {
		return fmt.Errorf("kickoff: repo %q must be owner/name", k.Repo)
	}
	if len(k.Report) == 0 {
		return fmt.Errorf("kickoff: report is required")
	}
	return nil
}

// ReportText returns the report as clean text for the LLM to reason over. The report
// may be a JSON value (a linter that emits JSON) or a JSON string wrapping arbitrary
// text/XML (JaCoCo, lcov, go cover, …); in the latter case it is unquoted so the model
// sees the raw report rather than an escaped string.
func (k Kickoff) ReportText() string {
	s := strings.TrimSpace(string(k.Report))
	if strings.HasPrefix(s, `"`) {
		var unquoted string
		if err := json.Unmarshal(k.Report, &unquoted); err == nil {
			return unquoted
		}
	}
	return string(k.Report)
}

// Owner returns the owner portion of repo.
func (k Kickoff) Owner() string { o, _, _ := splitRepo(k.Repo); return o }

// Name returns the repository name portion of repo.
func (k Kickoff) Name() string { _, n, _ := splitRepo(k.Repo); return n }

func splitRepo(s string) (owner, repo string, ok bool) {
	owner, repo, ok = strings.Cut(s, "/")
	if !ok || owner == "" || repo == "" {
		return "", "", false
	}
	return owner, repo, true
}
