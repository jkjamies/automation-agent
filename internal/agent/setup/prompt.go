package setup

import (
	"fmt"
	"io/fs"
	"strings"
)

// Prompts loads markdown prompt files from an fs.FS. Each agent embeds its own
// prompts/ directory (//go:embed prompts/*.md) and passes the embed.FS here, so
// prompts stay as reviewable markdown next to the agent that uses them.
type Prompts struct {
	fsys fs.FS
}

// NewPrompts wraps a filesystem (typically an embed.FS rooted at the agent package).
func NewPrompts(fsys fs.FS) Prompts {
	return Prompts{fsys: fsys}
}

// Get returns the trimmed contents of prompts/<name>.md.
func (p Prompts) Get(name string) (string, error) {
	b, err := fs.ReadFile(p.fsys, "prompts/"+name+".md")
	if err != nil {
		return "", fmt.Errorf("read prompt %q: %w", name, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// MustGet is Get but panics on error. Use at agent construction time, where a
// missing prompt is a programming error that should fail fast at startup.
func (p Prompts) MustGet(name string) string {
	s, err := p.Get(name)
	if err != nil {
		panic(err)
	}
	return s
}
