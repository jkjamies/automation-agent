// Package models holds the lint-fixer's payload types.
//
// The kickoff payload is a thin, TRUSTED envelope our CI Action posts to
// /webhooks/lint: repo and base identify where to work (reliable, set by us), and
// report carries the linter's output in whatever format the stack emits. We do not
// parse the report ourselves — an LLM triage step reasons over it (see triage.go),
// since the format varies across tech stacks and the model needs it anyway.
package models

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Kickoff is the lint-fixer kickoff envelope.
type Kickoff struct {
	Repo   string          `json:"repo"`           // owner/name (trusted)
	Base   string          `json:"base,omitempty"` // base branch; defaults to "main"
	Report json.RawMessage `json:"report"`         // arbitrary linter output
}

// FileProblems is the normalized triage output: one file and its lint problems.
type FileProblems struct {
	Path     string   `json:"path"`
	Problems []string `json:"problems"`
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

// ReportText returns the report as a string for the LLM to reason over.
func (k Kickoff) ReportText() string { return string(k.Report) }

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
