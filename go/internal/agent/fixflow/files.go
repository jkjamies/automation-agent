package fixflow

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// safeJoin resolves a repo-relative path against the checkout root, REJECTING (not
// clamping) absolute paths and any path that escapes the root via "..". Both reads
// and writes route through it, so LLM-controlled paths cannot touch host files.
func safeJoin(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute path %q not allowed", rel)
	}
	full := filepath.Join(root, rel)
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the repo", rel)
	}
	// Symlink containment: a symlinked directory inside the (attacker-influenced) checkout
	// could redirect an in-bounds path outside the root, so re-check the real location.
	// EvalSymlinks fails on a not-yet-created target, so resolve the deepest existing
	// ancestor; both sides are resolved so a symlinked temp root doesn't false-reject.
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve checkout root: %w", err)
	}
	fullReal, err := resolveExisting(full)
	if err != nil {
		return "", fmt.Errorf("path %q escapes the repo via a symlink: %w", rel, err)
	}
	if fullReal != rootReal && !strings.HasPrefix(fullReal, rootReal+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the repo via a symlink", rel)
	}
	return full, nil
}

// resolveExisting returns p with its longest existing ancestor symlink-resolved and any
// not-yet-created remainder appended lexically. It errors if a path component exists but
// cannot be resolved — a dangling or looping symlink — since that could redirect a write
// outside the root rather than being a legitimate not-yet-created file.
func resolveExisting(p string) (string, error) {
	rest := ""
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			if rest == "" {
				return real, nil
			}
			return filepath.Join(real, rest), nil
		}
		// EvalSymlinks failed. If the entry itself exists (Lstat doesn't follow the final
		// link), it's a dangling/looping symlink — reject. Otherwise it's a genuinely
		// missing component, so keep walking up.
		if _, lerr := os.Lstat(p); lerr == nil {
			return "", fmt.Errorf("unresolvable symlink %q", p)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p, nil // reached the filesystem root; nothing resolved
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// ReadFile reads a repo-relative file from the checkout (path-safe).
func ReadFile(root, rel string) (string, error) {
	full, err := safeJoin(root, rel)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
