package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The system's knowledge lives in the repo-root /okf bundle (Open Knowledge Format v0.2).
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

// okfFrontmatter returns the frontmatter block of an okf document and whether one is
// present. A malformed block (opened but never closed) reports absent. The closing
// delimiter is a line of exactly ---, so a body line such as ----- cannot be mistaken
// for the close and silently truncate the block.
func okfFrontmatter(doc string) (string, bool) {
	if !strings.HasPrefix(doc, "---\n") {
		return "", false
	}
	body := doc[4:]
	if end := strings.Index(body, "\n---\n"); end >= 0 {
		return body[:end], true
	}
	if strings.HasSuffix(body, "\n---") {
		return body[:len(body)-len("\n---")], true
	}
	return "", false
}

// walkOKFConcepts calls fn with the path and frontmatter of every concept document — any
// .md that is not a reserved index.md/log.md. Documents whose frontmatter is missing or
// unterminated are reported once here rather than by every caller.
func walkOKFConcepts(t *testing.T, fn func(path, frontmatter string)) {
	t.Helper()
	err := filepath.WalkDir(okfRoot(t), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return err
		}
		switch filepath.Base(p) {
		case "index.md", "log.md", "AGENTS.md":
			return nil
		}
		b, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		fm, ok := okfFrontmatter(string(b))
		if !ok {
			t.Errorf("%s: concept missing a terminated YAML frontmatter block", p)
			return nil
		}
		fn(p, fm)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// TestOKFConceptsHaveFrontmatterType asserts every concept declares a non-empty type —
// the one hard requirement of OKF conformance.
func TestOKFConceptsHaveFrontmatterType(t *testing.T) {
	typeLine := regexp.MustCompile(`(?m)^type:\s*\S`)
	walkOKFConcepts(t, func(p, fm string) {
		if !typeLine.MatchString(fm) {
			t.Errorf("%s: frontmatter missing required non-empty type field", p)
		}
	})
}

// TestOKFConceptsDeclareProvenance asserts every concept records who produced it and when
// it last materially changed, as the v0.2 generated family. The superseded v0.1 timestamp
// key must be gone: leaving both behind lets the two drift and consumers disagree about
// which one dates the concept.
func TestOKFConceptsDeclareProvenance(t *testing.T) {
	// generated may be written as a flow mapping on one line or as an indented block.
	generatedBy := regexp.MustCompile(`(?m)^generated:(?:.*\bby:\s*\S|\s*$(?:\n[ \t]+[^\s:]+:.*)*?\n[ \t]+by:\s*\S)`)
	generatedAt := regexp.MustCompile(`(?m)^generated:(?:.*\bat:\s*\S|\s*$(?:\n[ \t]+[^\s:]+:.*)*?\n[ \t]+at:\s*\S)`)
	legacy := regexp.MustCompile(`(?m)^timestamp:`)
	walkOKFConcepts(t, func(p, fm string) {
		if !generatedBy.MatchString(fm) {
			t.Errorf("%s: frontmatter missing generated.by", p)
		}
		// at carries what timestamp used to: without it the migration loses the date.
		if !generatedAt.MatchString(fm) {
			t.Errorf("%s: frontmatter missing generated.at", p)
		}
		if legacy.MatchString(fm) {
			t.Errorf("%s: frontmatter carries the superseded timestamp key; use generated.at", p)
		}
	})
}

// TestOKFConceptsDeclareLifecycleStatus asserts every concept carries a status drawn from
// the lifecycle set, so a consumer can tell reviewed knowledge from knowledge written
// ahead of the code or kept only for its links.
func TestOKFConceptsDeclareLifecycleStatus(t *testing.T) {
	status := regexp.MustCompile(`(?m)^status:\s*(\S+)`)
	valid := map[string]bool{"draft": true, "stable": true, "deprecated": true}
	walkOKFConcepts(t, func(p, fm string) {
		m := status.FindStringSubmatch(fm)
		if m == nil {
			t.Errorf("%s: frontmatter missing status", p)
			return
		}
		if !valid[m[1]] {
			t.Errorf("%s: status %q is not draft, stable, or deprecated", p, m[1])
		}
	})
}

// TestOKFConceptResourcesExist asserts every concept's optional resource — the repo-relative
// path of the unit it documents — still resolves. A concept that outlives the code it points
// at is the failure this catches: the path is the only machine-checkable link from knowledge
// back to the thing it describes.
func TestOKFConceptResourcesExist(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	resource := regexp.MustCompile(`(?m)^resource:\s*(\S+)`)
	walkOKFConcepts(t, func(p, fm string) {
		m := resource.FindStringSubmatch(fm)
		if m == nil {
			return
		}
		// The contract is a repo-relative path: an absolute one, or one climbing out of
		// the repo, would resolve against a machine rather than against this codebase.
		rel := filepath.Clean(filepath.FromSlash(m[1]))
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Errorf("%s: resource %s must be a repo-relative path", p, m[1])
			return
		}
		if _, statErr := os.Stat(filepath.Join(repo, rel)); statErr != nil {
			t.Errorf("%s: resource %s does not exist", p, m[1])
		}
	})
}

// TestOKFBundleDeclaresVersion asserts the bundle-root index.md declares the OKF version
// it targets, and that no other index.md carries frontmatter — the root is the only index
// file the format allows one on.
func TestOKFBundleDeclaresVersion(t *testing.T) {
	root := okfRoot(t)
	version := regexp.MustCompile(`(?m)^okf_version:\s*"?0\.2"?\s*$`)

	b, err := os.ReadFile(filepath.Join(root, "index.md"))
	if err != nil {
		t.Fatalf("bundle-root index.md missing: %v", err)
	}
	fm, ok := okfFrontmatter(string(b))
	if !ok {
		t.Fatal("bundle-root index.md has no frontmatter block declaring okf_version")
	}
	if !version.MatchString(fm) {
		t.Error("bundle-root index.md does not declare okf_version: \"0.2\"")
	}

	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(p) != "index.md" || filepath.Dir(p) == root {
			return err
		}
		b, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		if strings.HasPrefix(string(b), "---\n") {
			t.Errorf("%s: only the bundle-root index.md may carry frontmatter", p)
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
