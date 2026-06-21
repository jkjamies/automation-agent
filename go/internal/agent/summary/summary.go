package summary

import (
	"context"
	"fmt"
	"iter"
	"sort"
	"strings"
	"time"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/session"

	"github.com/jkjamies/automation-agent/internal/agent/setup"
	"github.com/jkjamies/automation-agent/internal/githubapi"
	"github.com/jkjamies/automation-agent/internal/notify"
)

// CommitLister is the slice of githubapi the fetchers need (consumer-defined for
// fakeability).
type CommitLister interface {
	ListCommitsSince(ctx context.Context, owner, repo string, since time.Time) ([]githubapi.Commit, error)
}

const (
	statePrefix = "commits:" // one key per repo: commits:<owner/repo>
	digestKey   = "digest"   // summarizer output
)

// newFetchAgent returns a code agent that fetches the last `window` of commits for
// repo and writes a formatted digest to state under commits:<repo>.
func newFetchAgent(repo string, gh CommitLister, window time.Duration, now func() time.Time) agent.Agent {
	name := "fetch_" + safeName(repo)
	a, _ := agent.New(agent.Config{
		Name:        name,
		Description: "Fetches recent commits for " + repo,
		Run: func(ctx agent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				owner, name2, ok := splitRepo(repo)
				if !ok {
					yield(nil, fmt.Errorf("invalid repo %q (want owner/repo)", repo))
					return
				}
				commits, err := gh.ListCommitsSince(ctx, owner, name2, now().Add(-window))
				if err != nil {
					yield(nil, fmt.Errorf("fetch %s: %w", repo, err))
					return
				}
				text := formatCommits(repo, commits)
				yield(setup.TextEvent(name, text, map[string]any{statePrefix + repo: text}), nil)
			}
		},
	})
	return a
}

// newNotifyAgent returns a code agent that posts the summarizer's digest to chat.
func newNotifyAgent(n notify.Notifier) agent.Agent {
	a, _ := agent.New(agent.Config{
		Name:        "notify",
		Description: "Posts the commit digest to Slack or Teams",
		Run: func(ctx agent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				digest := strings.TrimSpace(setup.StateString(ctx.Session().State(), digestKey))
				if digest == "" {
					digest = "(no digest was produced)"
				}
				if err := n.Notify(ctx, notify.Message{Title: "Daily commit digest", Text: digest}); err != nil {
					yield(nil, fmt.Errorf("notify: %w", err))
					return
				}
				yield(setup.TextEvent("notify", "Posted digest to chat.", nil), nil)
			}
		},
	})
	return a
}

// summaryInstruction is the dynamic instruction for the summarizer: it reads the
// per-repo commit data the fetchers wrote to state and appends it to the prompt.
func summaryInstruction(promptBody string) llmagent.InstructionProvider {
	return func(ctx agent.ReadonlyContext) (string, error) {
		return buildInstruction(promptBody, ctx.ReadonlyState()), nil
	}
}

// stateAll is the iteration side of session state, satisfied by ReadonlyState.
type stateAll interface {
	All() iter.Seq2[string, any]
}

func buildInstruction(promptBody string, st stateAll) string {
	type kv struct{ k, v string }
	var items []kv
	for k, v := range st.All() {
		if strings.HasPrefix(k, statePrefix) {
			if s, ok := v.(string); ok {
				items = append(items, kv{k, s})
			}
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].k < items[j].k })

	var b strings.Builder
	b.WriteString(promptBody)
	b.WriteString("\n\n## Commits\n")
	if len(items) == 0 {
		b.WriteString("(no commit data)\n")
	}
	for _, it := range items {
		b.WriteString(it.v)
		b.WriteString("\n")
	}
	return b.String()
}

func formatCommits(repo string, commits []githubapi.Commit) string {
	if len(commits) == 0 {
		return fmt.Sprintf("Repository %s: no commits in the window.", repo)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Repository %s (%d commits):\n", repo, len(commits))
	for _, c := range commits {
		fmt.Fprintf(&b, "- %s %s (%s)\n", shortSHA(c.SHA), firstLine(c.Message), c.Author)
	}
	return b.String()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func splitRepo(s string) (owner, repo string, ok bool) {
	owner, repo, ok = strings.Cut(s, "/")
	if !ok || owner == "" || repo == "" {
		return "", "", false
	}
	return owner, repo, true
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
