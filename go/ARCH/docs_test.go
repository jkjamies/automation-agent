package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The system's knowledge lives in the repo-root /okf bundle (Open Knowledge Format).
// These tests are the bundle's conformance gate: structural rules only, never content.

// okfRoot resolves the repo-root okf/ bundle directory.
func okfRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "okf"))
	if err != nil {
		t.Fatalf("okfRoot: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("okf bundle missing at %s: %v", root, err)
	}
	return root
}

// TestOKFConceptsHaveFrontmatterType asserts every concept document (any .md that is not
// a reserved index.md/log.md) opens with a YAML frontmatter block declaring a non-empty
// type — the one hard requirement of OKF conformance.
func TestOKFConceptsHaveFrontmatterType(t *testing.T) {
	typeLine := regexp.MustCompile(`(?m)^type:\s*\S`)
	err := filepath.WalkDir(okfRoot(t), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return err
		}
		base := filepath.Base(p)
		if base == "index.md" || base == "log.md" || base == "AGENTS.md" {
			return nil
		}
		b, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		s := string(b)
		if !strings.HasPrefix(s, "---\n") {
			t.Errorf("%s: concept missing YAML frontmatter block", p)
			return nil
		}
		end := strings.Index(s[4:], "\n---")
		if end < 0 {
			t.Errorf("%s: frontmatter block not closed", p)
			return nil
		}
		if !typeLine.MatchString(s[4 : 4+end]) {
			t.Errorf("%s: frontmatter missing required non-empty type field", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// TestOKFEveryDirHasIndex asserts every bundle directory carries an index.md for
// progressive disclosure.
func TestOKFEveryDirHasIndex(t *testing.T) {
	err := filepath.WalkDir(okfRoot(t), func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		if _, statErr := os.Stat(filepath.Join(p, "index.md")); statErr != nil {
			t.Errorf("missing index.md in %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// TestOKFBundleLinksResolve asserts every bundle-absolute markdown link (/path/file.md,
// with or without a #fragment) resolves to a file within the bundle. Anchor existence
// inside the target is content, not structure, and is deliberately not validated.
// Relative and external links are out of scope (OKF consumers tolerate them; the
// absolute form is the house convention).
func TestOKFBundleLinksResolve(t *testing.T) {
	root := okfRoot(t)
	link := regexp.MustCompile(`\]\((/[^)#]+\.md)(?:#[^)]*)?\)`)
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return err
		}
		b, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		for _, m := range link.FindAllStringSubmatch(string(b), -1) {
			target := filepath.Join(root, filepath.FromSlash(m[1]))
			if _, statErr := os.Stat(target); statErr != nil {
				t.Errorf("%s: bundle-absolute link %s does not resolve", p, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// TestOKFSkillCitationsResolve asserts every knowledge citation in a skill file
// (.agents/skills/**/SKILL.md) points at a concept that exists — the skills→knowledge
// edge is one-way and machine-checked, never hand-maintained.
func TestOKFSkillCitationsResolve(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	skills := filepath.Join(root, ".agents", "skills")
	if _, statErr := os.Stat(skills); statErr != nil {
		t.Skipf("no skills directory: %v", statErr)
	}
	cite := regexp.MustCompile(`okf/[A-Za-z0-9._/-]+\.md`)
	err = filepath.WalkDir(skills, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(p) != "SKILL.md" {
			return err
		}
		b, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		for _, m := range cite.FindAllString(string(b), -1) {
			target := filepath.Clean(filepath.Join(root, filepath.FromSlash(m)))
			// A citation must stay inside the bundle — okf/../elsewhere.md is not a concept.
			if !strings.HasPrefix(target, filepath.Join(root, "okf")+string(filepath.Separator)) {
				t.Errorf("%s: knowledge citation %s escapes the bundle", p, m)
				continue
			}
			if _, statErr := os.Stat(target); statErr != nil {
				t.Errorf("%s: knowledge citation %s does not resolve", p, m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// TestOKFRootAgentsDocPointsAtBundle asserts the repo-root AGENTS.md (the one auto-loaded
// discovery surface) still points agents at the bundle's index.
func TestOKFRootAgentsDocPointsAtBundle(t *testing.T) {
	p, err := filepath.Abs(filepath.Join("..", "..", "AGENTS.md"))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("repo-root AGENTS.md missing: %v", err)
	}
	if !strings.Contains(string(b), "okf/index.md") {
		t.Error("repo-root AGENTS.md no longer points at okf/index.md")
	}
}
