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
	return full, nil
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
