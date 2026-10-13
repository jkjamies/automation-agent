// Package workspace owns a run's git checkout: clone, branch, diff, commit, push, and
// running bounded commands inside it. It shells out to the git CLI.
//
// Two properties matter for safety, because a coding agent edits this checkout:
//   - credentials never land on disk: the token reaches git through GIT_CONFIG_* env vars
//     on the one git process that needs it, never through .git/config or argv;
//   - the agent cannot plant git hooks that ticketflow would then execute: every git
//     command runs with core.hooksPath pointed at an empty value.
package workspace

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Author is the identity commits are made as.
type Author struct {
	Name  string
	Email string
}

// Workspace is a checkout of one repository.
type Workspace struct {
	Dir    string
	Remote string // clone URL, without credentials
	Base   string // branch the work starts from
	token  string
}

// Clone clones branch base of remote into dir (which must not exist or be empty). The
// token, when set, authenticates over HTTPS without being persisted.
func Clone(ctx context.Context, remote, base, dir, token string) (*Workspace, error) {
	w := &Workspace{Dir: dir, Remote: remote, Base: base, token: token}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("workspace: create %s: %w", dir, err)
	}
	if _, err := w.git(ctx, true, "clone", "--branch", base, "--single-branch", "--no-tags", remote, "."); err != nil {
		return nil, err
	}
	return w, nil
}

// RemoteBranchExists reports whether branch already exists on the remote.
func (w *Workspace) RemoteBranchExists(ctx context.Context, branch string) (bool, error) {
	out, err := w.git(ctx, true, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// CreateBranch creates and switches to branch at the current HEAD.
func (w *Workspace) CreateBranch(ctx context.Context, branch string) error {
	_, err := w.git(ctx, false, "switch", "--create", branch)
	return err
}

// FileChange is one path in a diff.
type FileChange struct {
	Path    string
	Status  string // A (added), M (modified), D (deleted), R (renamed: Path is the new path)
	OldPath string // set for renames
	Added   int    // -1 for binary files
	Deleted int    // -1 for binary files
}

// Diff is the full working-tree change against the base commit.
type Diff struct {
	Files []FileChange
	Patch string
}

// ChangedLines is added + deleted lines across text files.
func (d Diff) ChangedLines() int {
	n := 0
	for _, f := range d.Files {
		if f.Added > 0 {
			n += f.Added
		}
		if f.Deleted > 0 {
			n += f.Deleted
		}
	}
	return n
}

// Empty reports whether nothing changed.
func (d Diff) Empty() bool { return len(d.Files) == 0 }

// Diff stages everything (including untracked files) and reports the change against
// the branch's starting point. Staging is what makes new files visible to git diff.
func (w *Workspace) Diff(ctx context.Context) (Diff, error) {
	if _, err := w.git(ctx, false, "add", "--all"); err != nil {
		return Diff{}, err
	}
	base := "origin/" + w.Base
	status, err := w.git(ctx, false, "diff", "--cached", "--name-status", "--find-renames", base)
	if err != nil {
		return Diff{}, err
	}
	numstat, err := w.git(ctx, false, "diff", "--cached", "--numstat", "--find-renames", base)
	if err != nil {
		return Diff{}, err
	}
	patch, err := w.git(ctx, false, "diff", "--cached", "--find-renames", base)
	if err != nil {
		return Diff{}, err
	}
	d := Diff{Patch: patch}
	counts := parseNumstat(numstat)
	for _, line := range strings.Split(strings.TrimSpace(status), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		fc := FileChange{Status: fields[0][:1]}
		switch {
		case fc.Status == "R" && len(fields) >= 3:
			fc.OldPath, fc.Path = fields[1], fields[2]
		case len(fields) >= 2:
			fc.Path = fields[1]
		default:
			continue
		}
		c := counts[fc.Path]
		fc.Added, fc.Deleted = c[0], c[1]
		d.Files = append(d.Files, fc)
	}
	return d, nil
}

// parseNumstat maps path -> {added, deleted}; binary files report -1. Renames appear
// as "old => new" or "dir/{old => new}/f" and are keyed by the new path.
func parseNumstat(out string) map[string][2]int {
	m := map[string][2]int{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) != 3 {
			continue
		}
		add, err1 := strconv.Atoi(f[0])
		del, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			add, del = -1, -1
		}
		m[renamedPath(f[2])] = [2]int{add, del}
	}
	return m
}

func renamedPath(p string) string {
	if open := strings.Index(p, "{"); open >= 0 {
		if end := strings.Index(p[open:], "}"); end > 0 {
			inner := p[open+1 : open+end]
			if _, after, ok := strings.Cut(inner, " => "); ok {
				return strings.TrimPrefix(strings.ReplaceAll(p[:open]+after+p[open+end+1:], "//", "/"), "/")
			}
		}
	}
	if _, after, ok := strings.Cut(p, " => "); ok {
		return after
	}
	return p
}

// Commit commits everything staged by the last Diff as author.
func (w *Workspace) Commit(ctx context.Context, msg string, author Author) error {
	if _, err := w.git(ctx, false, "add", "--all"); err != nil {
		return err
	}
	_, err := w.gitEnv(ctx, false, []string{
		"GIT_AUTHOR_NAME=" + author.Name, "GIT_AUTHOR_EMAIL=" + author.Email,
		"GIT_COMMITTER_NAME=" + author.Name, "GIT_COMMITTER_EMAIL=" + author.Email,
	}, "commit", "--no-verify", "--no-gpg-sign", "--message", msg)
	return err
}

// Push pushes HEAD to branch on the remote. It never forces: an agent branch that already
// exists remotely may carry a human's commits.
func (w *Workspace) Push(ctx context.Context, branch string) error {
	_, err := w.git(ctx, true, "push", "--no-verify", "origin", "HEAD:refs/heads/"+branch)
	return err
}

func (w *Workspace) git(ctx context.Context, authenticated bool, args ...string) (string, error) {
	return w.gitEnv(ctx, authenticated, nil, args...)
}

// gitEnv runs git in the checkout. Hooks are disabled and prompts suppressed on every
// call; the auth header is added only when authenticated is set.
func (w *Workspace) gitEnv(ctx context.Context, authenticated bool, extra []string, args ...string) (string, error) {
	cfg := [][2]string{{"core.hooksPath", os.DevNull}}
	if authenticated && w.token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + w.token))
		cfg = append(cfg, [2]string{"http.extraHeader", "Authorization: Basic " + basic})
	}
	env := append(baseEnv(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT="+strconv.Itoa(len(cfg)))
	for i, kv := range cfg {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	env = append(env, extra...)

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = w.Dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("workspace: git %s: %w: %s", args[0], err, redact(strings.TrimSpace(stderr.String()), w.token))
	}
	return stdout.String(), nil
}

// CommandResult is the outcome of a bounded command run.
type CommandResult struct {
	Argv     []string
	ExitCode int
	Output   string // combined stdout+stderr, truncated to the last OutputLimit bytes
	TimedOut bool
	Elapsed  time.Duration
}

// Passed reports whether the command exited 0 within its timeout.
func (r CommandResult) Passed() bool { return r.ExitCode == 0 && !r.TimedOut }

// OutputLimit caps captured command output; the tail is kept because that is where
// compilers and test runners put the failure.
const OutputLimit = 16 << 10

// Run executes argv in dir with a minimal environment (PATH, HOME, and passthrough), so
// no credential held by this process leaks into a build or test the agent wrote. A
// non-zero exit is reported in the result, not as an error; err is for "could not start".
func Run(ctx context.Context, dir string, argv []string, timeout time.Duration, passthrough ...string) (CommandResult, error) {
	if len(argv) == 0 {
		return CommandResult{}, errors.New("workspace: empty command")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(baseEnv(), passthrough...)
	cmd.WaitDelay = 5 * time.Second
	var out tailBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	start := time.Now()
	err := cmd.Run()
	res := CommandResult{Argv: argv, Output: out.String(), Elapsed: time.Since(start)}
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		res.TimedOut, res.ExitCode = true, -1
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	case err != nil:
		return res, fmt.Errorf("workspace: run %s: %w", argv[0], err)
	}
	return res, nil
}

// baseEnv is the environment every child process starts from.
func baseEnv() []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "LANG=C.UTF-8"}
	if v := os.Getenv("GOCACHE"); v != "" {
		env = append(env, "GOCACHE="+v)
	}
	if v := os.Getenv("GOMODCACHE"); v != "" {
		env = append(env, "GOMODCACHE="+v)
	}
	return env
}

// tailBuffer keeps the last OutputLimit bytes written to it.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	// Trim lazily, at twice the limit, so a chatty process is not copied on every write.
	if len(t.b) > 2*OutputLimit {
		t.b = append([]byte(nil), t.b[len(t.b)-OutputLimit:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	if len(t.b) > OutputLimit {
		return string(t.b[len(t.b)-OutputLimit:])
	}
	return string(t.b)
}

func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[redacted]")
}
