package setup

import (
	"fmt"
	"io/fs"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
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

// StaticInstruction returns an InstructionProvider that yields s verbatim.
//
// Use it whenever an agent's instruction embeds text the service did not author — a diff, a
// fetched document, tool output. The ADK treats the plain Instruction string as a template:
// every `{identifier}` is a session-state lookup that fails the run when the key is absent, so
// an instruction carrying a Python f-string, a route pattern, or a templated config would error
// with "state key does not exist" before the model is ever called. An InstructionProvider is
// exempt from that templating, and this one adds nothing else — the string goes to the model
// exactly as composed.
func StaticInstruction(s string) llmagent.InstructionProvider {
	return func(agent.ReadonlyContext) (string, error) { return s, nil }
}
