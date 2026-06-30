package reviewer

import (
	"context"
	"strings"
	"testing"

	"automation-agent/internal/githubapi"
)

func TestMatchStandards(t *testing.T) {
	entries := []githubapi.TreeEntry{
		{Path: "AGENTS.md", SHA: "a", Type: "blob"},
		{Path: "internal/AGENTS.md", SHA: "b", Type: "blob"},
		{Path: ".cursor/rules/go.mdc", SHA: "c", Type: "blob"},
		{Path: ".cursor/rules", SHA: "d", Type: "tree"}, // a directory, not a doc
		{Path: "main.go", SHA: "e", Type: "blob"},       // not a standards file
	}
	globs := []string{"AGENTS.md", "**/AGENTS.md", ".cursor/rules/**"}
	got := matchStandards(entries, globs)
	want := map[string]bool{"AGENTS.md": true, "internal/AGENTS.md": true, ".cursor/rules/go.mdc": true}
	if len(got) != len(want) {
		t.Fatalf("matched %d, want %d: %+v", len(got), len(want), got)
	}
	for _, m := range got {
		if !want[m.Path] {
			t.Errorf("unexpected match %q", m.Path)
		}
		if m.Type != "blob" {
			t.Errorf("matched a non-blob: %+v", m)
		}
	}
}

func TestStandardsCacheKeyChangesWithSHA(t *testing.T) {
	base := []githubapi.TreeEntry{{Path: "AGENTS.md", SHA: "v1"}}
	changed := []githubapi.TreeEntry{{Path: "AGENTS.md", SHA: "v2"}}
	if standardsCacheKey("o", "r", base) == standardsCacheKey("o", "r", changed) {
		t.Error("a changed blob SHA must change the cache key (else stale rules)")
	}
	if standardsCacheKey("o", "r", base) != standardsCacheKey("o", "r", base) {
		t.Error("same inputs must produce the same key")
	}
}

func TestParseRules(t *testing.T) {
	t.Run("prose-wrapped, dedup, drops blank", func(t *testing.T) {
		raw := "Here are the rules:\n```json\n[" +
			`{"id":"R1","dimension":"security","summary":"validate input","source":"SECURITY.md"},` +
			`{"id":"R1","dimension":"x","summary":"dup id dropped","source":"x"},` +
			`{"id":"R2","summary":"  ","source":"y"},` + // blank summary dropped
			`{"id":"R3","dimension":"vibes","summary":"prefer composition","source":"AGENTS.md"}` +
			"]\n```"
		got := parseRules(raw)
		if len(got) != 2 {
			t.Fatalf("got %d rules, want 2 (dup + blank dropped): %+v", len(got), got)
		}
		if got[0].ID != "R1" || got[0].Dimension != DimSecurity {
			t.Errorf("rule 0 = %+v", got[0])
		}
		if got[1].ID != "R3" || got[1].Dimension != DimOther { // "vibes" normalizes to other
			t.Errorf("rule 1 = %+v", got[1])
		}
	})
	t.Run("malformed yields nil", func(t *testing.T) {
		for _, raw := range []string{"", "no json", "[{broken", "{\"not\":\"array\"}", "[]"} {
			if got := parseRules(raw); got != nil {
				t.Errorf("parseRules(%q) = %+v, want nil", raw, got)
			}
		}
	})
}

func TestStandardsMenuAndLookup(t *testing.T) {
	std := buildStandards(
		[]Rule{{ID: "R1", Dimension: DimSecurity, Summary: "validate input", Source: "SECURITY.md"}},
		map[string]string{"SECURITY.md": "full security doc text"},
		[]string{"SECURITY.md"},
	)
	if std.empty() {
		t.Fatal("std must not be empty")
	}
	if !std.validID("R1") || std.validID("R9") {
		t.Error("validID wrong")
	}
	if std.ruleDoc("R1") != "full security doc text" {
		t.Errorf("ruleDoc(R1) = %q", std.ruleDoc("R1"))
	}
	if std.ruleDoc("R9") != "" {
		t.Error("unknown id must yield empty doc")
	}
	for _, want := range []string{"R1", "security", "validate input", "SECURITY.md"} {
		if !strings.Contains(std.menu(), want) {
			t.Errorf("menu missing %q:\n%s", want, std.menu())
		}
	}
}

func TestGateCitations(t *testing.T) {
	std := buildStandards(
		[]Rule{{ID: "R1", Dimension: DimPatternViolation, Summary: "s", Source: "AGENTS.md"}},
		map[string]string{"AGENTS.md": "doc"}, []string{"AGENTS.md"},
	)
	findings := []Finding{
		{Dimension: DimPatternViolation, Severity: SeverityMajor, Message: "cited", RuleID: "R1"}, // kept
		{Dimension: DimPatternViolation, Severity: SeverityMajor, Message: "uncited"},             // gated
		{Dimension: DimArchitecture, Severity: SeverityMajor, Message: "bad id", RuleID: "R9"},    // gated (unknown id)
		{Dimension: DimSecurity, Severity: SeverityCritical, Message: "sqli"},                     // never gated
	}

	t.Run("nitpick mode demotes uncited conformance", func(t *testing.T) {
		e := NewEngine(Deps{StandardsEnabled: true}) // UncitedDrop false -> nitpick
		got := e.gateCitations(append([]Finding(nil), findings...), std)
		if len(got) != 4 {
			t.Fatalf("nitpick mode keeps all, got %d", len(got))
		}
		if got[0].Severity != SeverityMajor { // cited conformance untouched
			t.Errorf("cited finding altered: %+v", got[0])
		}
		if got[1].Severity != SeverityNitpick || got[2].Severity != SeverityNitpick {
			t.Errorf("uncited conformance not demoted: %+v %+v", got[1], got[2])
		}
		if got[3].Severity != SeverityCritical { // security never gated
			t.Errorf("non-conformance finding altered: %+v", got[3])
		}
	})

	t.Run("drop mode removes uncited conformance", func(t *testing.T) {
		e := NewEngine(Deps{StandardsEnabled: true, UncitedDrop: true})
		got := e.gateCitations(append([]Finding(nil), findings...), std)
		if len(got) != 2 { // the cited conformance + the security finding
			t.Fatalf("drop mode = %d findings, want 2", len(got))
		}
	})

	t.Run("disabled passes everything through", func(t *testing.T) {
		e := NewEngine(Deps{StandardsEnabled: false, UncitedDrop: true})
		got := e.gateCitations(append([]Finding(nil), findings...), std)
		if len(got) != 4 {
			t.Errorf("standards off must not gate, got %d", len(got))
		}
	})
}

func TestStandardsToolsPresence(t *testing.T) {
	std := buildStandards([]Rule{{ID: "R1", Summary: "s"}}, map[string]string{}, []string{"AGENTS.md"})
	tools, err := standardsTools(std)
	if err != nil || len(tools) != 1 {
		t.Fatalf("standardsTools(non-empty) = %d tools, err %v; want 1", len(tools), err)
	}
	empty, err := standardsTools(nil)
	if err != nil || empty != nil {
		t.Errorf("standardsTools(nil) = %v, err %v; want nil/nil", empty, err)
	}
}

func TestDiscoverStandards(t *testing.T) {
	rulesJSON := `[{"id":"R1","dimension":"pattern_violation","summary":"wrap errors","source":"AGENTS.md"}]`
	newEngine := func(gh *fakeGH) *Engine {
		llm := fakeLLM{json: rulesJSON}
		return NewEngine(Deps{
			Enabled: true, GH: gh, BaseLLM: llm, CodeLLM: llm,
			StandardsEnabled: true, StandardsGlobs: []string{"AGENTS.md"}, StandardsMaxBytes: 1 << 20,
		})
	}

	t.Run("discovers, distills, caches", func(t *testing.T) {
		gh := &fakeGH{
			tree:     []githubapi.TreeEntry{{Path: "AGENTS.md", SHA: "s1", Type: "blob"}},
			contents: map[string]string{"AGENTS.md": "wrap errors with %w"},
		}
		e := newEngine(gh)
		std := e.discoverStandards(context.Background(), "o", "r", "head", nil)
		if std.empty() || len(std.rules) != 1 || std.rules[0].ID != "R1" {
			t.Fatalf("discovered std = %+v", std)
		}
		if len(std.sourceList()) != 1 || std.sourceList()[0] != "AGENTS.md" {
			t.Errorf("sources = %v", std.sourceList())
		}
		// Second call (same tree SHAs) is a cache hit: same pointer, no re-distill.
		if again := e.discoverStandards(context.Background(), "o", "r", "head", nil); again != std {
			t.Error("second discovery must hit the cache (same *standards)")
		}
	})

	t.Run("disabled yields nil", func(t *testing.T) {
		e := NewEngine(Deps{Enabled: true, GH: &fakeGH{}, StandardsEnabled: false})
		if std := e.discoverStandards(context.Background(), "o", "r", "head", nil); std != nil {
			t.Errorf("disabled discovery = %+v, want nil", std)
		}
	})

	t.Run("no matching docs yields nil", func(t *testing.T) {
		gh := &fakeGH{tree: []githubapi.TreeEntry{{Path: "main.go", SHA: "s", Type: "blob"}}}
		if std := newEngine(gh).discoverStandards(context.Background(), "o", "r", "head", nil); std != nil {
			t.Errorf("no standards docs = %+v, want nil", std)
		}
	})

	t.Run("tree error is best-effort nil", func(t *testing.T) {
		gh := &fakeGH{treeErr: context.DeadlineExceeded}
		if std := newEngine(gh).discoverStandards(context.Background(), "o", "r", "head", nil); std != nil {
			t.Errorf("tree error must degrade to nil, got %+v", std)
		}
	})

	// A truncated tree means discovery may have missed convention files; gating against the partial
	// set is worse than a generic review, so degrade to nil (and don't cache).
	t.Run("truncated tree degrades to generic nil", func(t *testing.T) {
		gh := &fakeGH{
			tree:      []githubapi.TreeEntry{{Path: "AGENTS.md", SHA: "s1", Type: "blob"}},
			truncated: true,
			contents:  map[string]string{"AGENTS.md": "wrap errors with %w"},
		}
		if std := newEngine(gh).discoverStandards(context.Background(), "o", "r", "head", nil); std != nil {
			t.Errorf("truncated tree must degrade to nil, got %+v", std)
		}
	})

	// A fetch failure leaves the rule set incomplete; degrade to generic for this round and do not
	// cache, so a later event retries the full set rather than serving a memoized partial.
	t.Run("partial fetch failure degrades to nil, uncached", func(t *testing.T) {
		gh := &fakeGH{
			tree: []githubapi.TreeEntry{
				{Path: "AGENTS.md", SHA: "s1", Type: "blob"},
				{Path: "CONTRIBUTING.md", SHA: "s2", Type: "blob"},
			},
			contents: map[string]string{"AGENTS.md": "wrap errors"}, // CONTRIBUTING.md fetch fails
		}
		e := NewEngine(Deps{
			Enabled: true, GH: gh, BaseLLM: fakeLLM{json: rulesJSON}, CodeLLM: fakeLLM{json: rulesJSON},
			StandardsEnabled: true, StandardsGlobs: []string{"AGENTS.md", "CONTRIBUTING.md"}, StandardsMaxBytes: 1 << 20,
		})
		if std := e.discoverStandards(context.Background(), "o", "r", "head", nil); std != nil {
			t.Errorf("partial fetch must degrade to nil, got %+v", std)
		}
		// Not memoized: once the missing doc resolves, the next event must build the full set.
		gh.contents["CONTRIBUTING.md"] = "prefer composition"
		if std := e.discoverStandards(context.Background(), "o", "r", "head", nil); std.empty() {
			t.Error("after the fetch resolves, discovery must build (partial was not cached)")
		}
	})

	// A per-directory instruction file applies only to its own touched module; root files always
	// apply; an untouched module's file is excluded.
	t.Run("per-module instruction file scoped to touched dirs", func(t *testing.T) {
		gh := &fakeGH{
			tree: []githubapi.TreeEntry{
				{Path: "AGENTS.md", SHA: "s0", Type: "blob"},
				{Path: "internal/foo/AGENTS.md", SHA: "s1", Type: "blob"},
				{Path: "internal/bar/AGENTS.md", SHA: "s2", Type: "blob"},
			},
			contents: map[string]string{
				"AGENTS.md":              "root",
				"internal/foo/AGENTS.md": "foo",
				"internal/bar/AGENTS.md": "bar",
			},
		}
		e := NewEngine(Deps{
			Enabled: true, GH: gh, BaseLLM: fakeLLM{json: rulesJSON}, CodeLLM: fakeLLM{json: rulesJSON},
			StandardsEnabled: true, StandardsGlobs: []string{"AGENTS.md", "**/AGENTS.md"}, StandardsMaxBytes: 1 << 20,
		})
		std := e.discoverStandards(context.Background(), "o", "r", "head", []githubapi.PRFile{{Path: "internal/foo/x.go"}})
		if std.empty() {
			t.Fatal("expected standards")
		}
		got := std.sourceList()
		want := map[string]bool{"AGENTS.md": true, "internal/foo/AGENTS.md": true}
		if len(got) != 2 || !want[got[0]] || !want[got[1]] {
			t.Errorf("scoped sources = %v, want root + internal/foo only (internal/bar excluded)", got)
		}
	})
}
