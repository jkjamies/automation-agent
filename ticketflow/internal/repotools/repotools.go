// Package repotools gives an ADK agent hands on one checkout: read-only tools for the
// planner and reviewer, plus write and run-check tools for the coding harness.
//
// Every path is confined to the checkout root (no absolute paths, no "..", no symlink
// escapes) and the .git directory is unreachable, because a file written under .git/hooks
// would be code that git — not the agent — later runs. Tool failures the model can
// recover from (a missing file, an ambiguous edit) are returned in the result's Error
// field rather than as Go errors, so one bad call does not abort the run.
package repotools

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"ticketflow/internal/gate"
	"ticketflow/internal/workspace"
)

const (
	maxReadBytes  = 256 << 10
	maxMatches    = 200
	maxListedDirs = 1000
)

// ReadOnly returns list_dir, read_file, and search rooted at root.
func ReadOnly(root string) ([]tool.Tool, error) {
	t := &tools{root: root}
	return build(
		spec[listArgs, listResult]{"list_dir", "List a directory of the repository. Path is repo-relative; use \".\" for the root. Subdirectories end in \"/\".", t.listDir},
		spec[readArgs, readResult]{"read_file", "Read a repository file by repo-relative path. Optionally pass start_line/end_line (1-based, inclusive) to read part of a large file.", t.readFile},
		spec[searchArgs, searchResult]{"search", "Search repository text files with a Go regular expression. Returns up to 200 matches as path:line: text. Optionally restrict to a repo-relative directory.", t.search},
	)
}

// ReadWrite returns the read-only tools plus write_file, edit_file, delete_file, and
// run_check. run_check can run only the named checks given here — never an arbitrary
// command — with a scrubbed environment and the given timeout.
func ReadWrite(root string, checks []gate.Check, timeout time.Duration) ([]tool.Tool, error) {
	ro, err := ReadOnly(root)
	if err != nil {
		return nil, err
	}
	t := &tools{root: root, checks: checks, timeout: timeout}
	names := make([]string, 0, len(checks))
	for _, c := range checks {
		names = append(names, c.Name)
	}
	rw, err := build(
		spec[writeArgs, writeResult]{"write_file", "Create or overwrite a repository file with the full new content. Parent directories are created.", t.writeFile},
		spec[editArgs, writeResult]{"edit_file", "Replace exactly one occurrence of old_text with new_text in a repository file. old_text must match exactly once; include surrounding lines to make it unique.", t.editFile},
		spec[deleteArgs, writeResult]{"delete_file", "Delete a repository file.", t.deleteFile},
		spec[checkArgs, checkResult]{"run_check", "Run one of the repository's verification checks and return its exit code and output tail. Available checks: " + strings.Join(names, ", ") + ".", t.runCheck},
	)
	if err != nil {
		return nil, err
	}
	return append(ro, rw...), nil
}

type tools struct {
	root    string
	checks  []gate.Check
	timeout time.Duration
}

// spec pairs a tool's name and description with its typed handler.
type spec[A, R any] struct {
	name, desc string
	fn         func(agent.Context, A) (R, error)
}

type builder interface{ build() (tool.Tool, error) }

func (s spec[A, R]) build() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{Name: s.name, Description: s.desc}, functiontool.Func[A, R](s.fn))
}

func build(specs ...builder) ([]tool.Tool, error) {
	out := make([]tool.Tool, 0, len(specs))
	for _, s := range specs {
		t, err := s.build()
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

type listArgs struct {
	Path string `json:"path" jsonschema:"repo-relative directory, \".\" for the root"`
}
type listResult struct {
	Entries []string `json:"entries,omitempty"`
	Error   string   `json:"error,omitempty"`
}

func (t *tools) listDir(_ agent.Context, a listArgs) (listResult, error) {
	full, err := Resolve(t.root, a.Path)
	if err != nil {
		return listResult{Error: err.Error()}, nil
	}
	ents, err := os.ReadDir(full)
	if err != nil {
		return listResult{Error: err.Error()}, nil
	}
	var names []string
	for _, e := range ents {
		if e.Name() == ".git" {
			continue
		}
		n := e.Name()
		if e.IsDir() {
			n += "/"
		}
		names = append(names, n)
		if len(names) == maxListedDirs {
			names = append(names, "… (truncated)")
			break
		}
	}
	return listResult{Entries: names}, nil
}

type readArgs struct {
	Path      string `json:"path" jsonschema:"repo-relative file path"`
	StartLine int    `json:"start_line,omitempty" jsonschema:"first line to return (1-based); omit for the start"`
	EndLine   int    `json:"end_line,omitempty" jsonschema:"last line to return (inclusive); omit for the end"`
}
type readResult struct {
	Content    string `json:"content,omitempty"`
	TotalLines int    `json:"total_lines,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	Error      string `json:"error,omitempty"`
}

func (t *tools) readFile(_ agent.Context, a readArgs) (readResult, error) {
	full, err := Resolve(t.root, a.Path)
	if err != nil {
		return readResult{Error: err.Error()}, nil
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return readResult{Error: err.Error()}, nil
	}
	lines := strings.SplitAfter(string(b), "\n")
	res := readResult{TotalLines: len(lines)}
	start, end := 1, len(lines)
	if a.StartLine > 0 {
		start = a.StartLine
	}
	if a.EndLine > 0 && a.EndLine < end {
		end = a.EndLine
	}
	if start > end {
		return readResult{TotalLines: len(lines), Error: fmt.Sprintf("empty range %d-%d", start, end)}, nil
	}
	content := strings.Join(lines[start-1:end], "")
	if len(content) > maxReadBytes {
		content, res.Truncated = content[:maxReadBytes], true
	}
	res.Content = content
	return res, nil
}

type searchArgs struct {
	Pattern string `json:"pattern" jsonschema:"Go regular expression"`
	Path    string `json:"path,omitempty" jsonschema:"repo-relative directory to search; omit for the whole repo"`
}
type searchResult struct {
	Matches   []string `json:"matches,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
	Error     string   `json:"error,omitempty"`
}

func (t *tools) search(ctx agent.Context, a searchArgs) (searchResult, error) {
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return searchResult{Error: "invalid pattern: " + err.Error()}, nil
	}
	start, err := Resolve(t.root, orDefault(a.Path, "."))
	if err != nil {
		return searchResult{Error: err.Error()}, nil
	}
	var res searchResult
	stop := errors.New("stop")
	walkErr := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > maxReadBytes || !d.Type().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer func() { _ = f.Close() }() // read-only; a close error loses nothing
		rel, _ := filepath.Rel(t.root, p)
		sc := bufio.NewScanner(f)
		for n := 1; sc.Scan(); n++ {
			line := sc.Text()
			if strings.ContainsRune(line, 0) {
				return nil // binary
			}
			if re.MatchString(line) {
				res.Matches = append(res.Matches, fmt.Sprintf("%s:%d: %s", filepath.ToSlash(rel), n, truncate(line, 300)))
				if len(res.Matches) == maxMatches {
					res.Truncated = true
					return stop
				}
			}
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, stop) {
		return searchResult{Error: walkErr.Error()}, nil
	}
	sort.Strings(res.Matches)
	return res, nil
}

type writeArgs struct {
	Path    string `json:"path" jsonschema:"repo-relative file path"`
	Content string `json:"content" jsonschema:"the complete new file content"`
}
type editArgs struct {
	Path    string `json:"path" jsonschema:"repo-relative file path"`
	OldText string `json:"old_text" jsonschema:"exact text to replace; must occur exactly once"`
	NewText string `json:"new_text" jsonschema:"replacement text"`
}
type deleteArgs struct {
	Path string `json:"path" jsonschema:"repo-relative file path"`
}
type writeResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (t *tools) writeFile(_ agent.Context, a writeArgs) (writeResult, error) {
	full, err := Resolve(t.root, a.Path)
	if err != nil {
		return writeResult{Error: err.Error()}, nil
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return writeResult{Error: err.Error()}, nil
	}
	if err := os.WriteFile(full, []byte(a.Content), 0o644); err != nil {
		return writeResult{Error: err.Error()}, nil
	}
	return writeResult{OK: true}, nil
}

func (t *tools) editFile(_ agent.Context, a editArgs) (writeResult, error) {
	full, err := Resolve(t.root, a.Path)
	if err != nil {
		return writeResult{Error: err.Error()}, nil
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return writeResult{Error: err.Error()}, nil
	}
	if a.OldText == "" {
		return writeResult{Error: "old_text is empty; use write_file to replace a whole file"}, nil
	}
	switch n := strings.Count(string(b), a.OldText); n {
	case 0:
		return writeResult{Error: "old_text not found; read the file again and copy the text exactly"}, nil
	case 1:
	default:
		return writeResult{Error: fmt.Sprintf("old_text occurs %d times; include more surrounding lines", n)}, nil
	}
	out := strings.Replace(string(b), a.OldText, a.NewText, 1)
	if err := os.WriteFile(full, []byte(out), 0o644); err != nil {
		return writeResult{Error: err.Error()}, nil
	}
	return writeResult{OK: true}, nil
}

func (t *tools) deleteFile(_ agent.Context, a deleteArgs) (writeResult, error) {
	full, err := Resolve(t.root, a.Path)
	if err != nil {
		return writeResult{Error: err.Error()}, nil
	}
	if full == filepath.Clean(t.root) {
		return writeResult{Error: "refusing to delete the repository root"}, nil
	}
	if err := os.Remove(full); err != nil {
		return writeResult{Error: err.Error()}, nil
	}
	return writeResult{OK: true}, nil
}

type checkArgs struct {
	Name string `json:"name" jsonschema:"name of the check to run"`
}
type checkResult struct {
	Passed   bool   `json:"passed"`
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
}

func (t *tools) runCheck(ctx agent.Context, a checkArgs) (checkResult, error) {
	for _, c := range t.checks {
		if c.Name != a.Name {
			continue
		}
		r, err := workspace.Run(ctx, t.root, c.Run, t.timeout)
		if err != nil {
			return checkResult{Error: err.Error()}, nil
		}
		return checkResult{Passed: r.Passed(), ExitCode: r.ExitCode, TimedOut: r.TimedOut, Output: r.Output}, nil
	}
	return checkResult{Error: fmt.Sprintf("unknown check %q", a.Name)}, nil
}

// ErrOutsideRoot is returned for a path that would leave the checkout or enter .git.
var ErrOutsideRoot = errors.New("path is outside the repository")

// Resolve maps a repo-relative path to an absolute one inside root, refusing absolute
// paths, parent traversal, the .git directory, and symlinks that point outside root.
func Resolve(root, rel string) (string, error) {
	if rel == "" {
		rel = "."
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %q is absolute", ErrOutsideRoot, rel)
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrOutsideRoot, rel)
	}
	if first, _, _ := strings.Cut(filepath.ToSlash(clean), "/"); first == ".git" {
		return "", fmt.Errorf("%w: .git is not accessible", ErrOutsideRoot)
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	full := filepath.Join(rootReal, clean)
	// Resolve the deepest existing ancestor: the target itself may not exist yet.
	probe := full
	for {
		real, err := filepath.EvalSymlinks(probe)
		if err == nil {
			if real != rootReal && !strings.HasPrefix(real, rootReal+string(filepath.Separator)) {
				return "", fmt.Errorf("%w: %q resolves outside it", ErrOutsideRoot, rel)
			}
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	return full, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
