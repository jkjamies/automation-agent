// Package prompts holds every model prompt and every human-facing message template as
// embedded markdown, so wording changes are reviewed as text rather than hunted for in
// Go string literals.
//
// Files used as an ADK agent instruction are static (Get). Files rendered per run use
// text/template (Render); they are never passed through ADK's own instruction templating,
// which would treat a "{name}" in a ticket as a session-state lookup.
package prompts

import (
	"bytes"
	"embed"
	"fmt"
	"strings"
	"text/template"
)

//go:embed *.md
var files embed.FS

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	"indent": func(prefix, s string) string {
		return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
	},
	"join": strings.Join,
}).ParseFS(files, "*.md"))

// Get returns a prompt file verbatim. It panics on an unknown name, which is a build-time
// mistake caught by the package tests.
func Get(name string) string {
	b, err := files.ReadFile(name)
	if err != nil {
		panic(fmt.Sprintf("prompts: %s: %v", name, err))
	}
	return string(b)
}

// Render executes a template file with data.
func Render(name string, data any) (string, error) {
	var b bytes.Buffer
	if err := templates.ExecuteTemplate(&b, name, data); err != nil {
		return "", fmt.Errorf("prompts: render %s: %w", name, err)
	}
	return strings.TrimSpace(b.String()) + "\n", nil
}
