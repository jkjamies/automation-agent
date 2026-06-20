package fixflow

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// safeJoin resolves a repo-relative path against the checkout root, refusing paths
// that escape it.
func safeJoin(root, rel string) (string, error) {
	full := filepath.Join(root, filepath.Clean("/"+rel))
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
