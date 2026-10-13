package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/workflow"

	"ticketflow/internal/gate"
	"ticketflow/internal/github"
	"ticketflow/internal/harness"
	"ticketflow/internal/jira"
	"ticketflow/internal/prompts"
	"ticketflow/internal/repotools"
	"ticketflow/internal/workspace"
)

// Route names: the labels on the graph's conditional edges.
const (
	routeReady    = "ready"
	routeNotReady = "not_ready"
	routeOK       = "ok"
	routeBlocked  = "blocked"
	routeGo       = "go"
	routeInvalid  = "invalid"
	routePass     = "pass"
	routeRetry    = "retry"
	routeGiveUp   = "give_up"
	routeRevise   = "revise"
	routePublish  = "publish"
)

// run is the state of one ticket's run. The graph is built per run with its nodes closed
// over this struct, and nodes execute one at a time, so it needs no locking.
type run struct {
	p      *Pipeline
	log    *slog.Logger
	key    string
	dir    string
	dryRun bool

	issue  jira.Issue
	repo   string
	base   string
	branch string
	ws     *workspace.Workspace
	policy gate.RepoPolicy
	plan   planResult

	attempts     int
	budget       int // attempts allowed; a self-review revision adds one
	revisions    int
	feedback     string
	agentSummary string
	diff         workspace.Diff
	checks       []checkOutcome
	review       reviewResult
	openFindings []finding

	problems []string // why the ticket bounced
	outcome  *Outcome
}

type checkOutcome struct {
	Name   string
	Passed bool
	Output string
}

func (p *Pipeline) graph(r *run) (agent.Agent, error) {
	readOnly, err := repotools.ReadOnly(r.dir)
	if err != nil {
		return nil, fmt.Errorf("pipeline: tools: %w", err)
	}
	planner, err := llmAgent("planner", "Decides whether a ticket is implementable unattended and plans it.", p.d.Planner, "planner.md", readOnly)
	if err != nil {
		return nil, err
	}
	reviewer, err := llmAgent("reviewer", "Reviews the agent's change before a pull request opens.", p.d.Reviewer, "reviewer.md", readOnly)
	if err != nil {
		return nil, err
	}
	plannerNode, err := workflow.NewAgentNode(planner, workflow.NodeConfig{})
	if err != nil {
		return nil, err
	}
	reviewerNode, err := workflow.NewAgentNode(reviewer, workflow.NodeConfig{})
	if err != nil {
		return nil, err
	}

	var (
		intake       = workflow.NewEmittingFunctionNode("intake", r.intake, workflow.NodeConfig{})
		prepare      = workflow.NewEmittingFunctionNode("prepare", r.prepare, workflow.NodeConfig{})
		triagePlan   = workflow.NewEmittingFunctionNode("triage_plan", r.triagePlan, workflow.NodeConfig{})
		implement    = workflow.NewFunctionNode("implement", r.implement, workflow.NodeConfig{})
		verify       = workflow.NewEmittingFunctionNode("verify", r.verify, workflow.NodeConfig{})
		triageReview = workflow.NewEmittingFunctionNode("triage_review", r.triageReview, workflow.NodeConfig{})
		publish      = workflow.NewFunctionNode("publish", r.publish, workflow.NodeConfig{})
		bounce       = workflow.NewFunctionNode("bounce", r.bounce, workflow.NodeConfig{})
		escalate     = workflow.NewFunctionNode("escalate", r.escalate, workflow.NodeConfig{})
	)
	on := func(from, to workflow.Node, route string) workflow.Edge {
		return workflow.Edge{From: from, To: to, Route: workflow.StringRoute(route)}
	}
	edges := []workflow.Edge{
		{From: workflow.Start, To: intake},
		on(intake, prepare, routeReady),
		on(intake, bounce, routeNotReady),
		on(prepare, plannerNode, routeOK),
		on(prepare, escalate, routeBlocked),
		{From: plannerNode, To: triagePlan},
		on(triagePlan, implement, routeGo),
		on(triagePlan, bounce, routeNotReady),
		on(triagePlan, escalate, routeInvalid),
		{From: implement, To: verify},
		on(verify, reviewerNode, routePass),
		on(verify, implement, routeRetry),
		on(verify, escalate, routeGiveUp),
		{From: reviewerNode, To: triageReview},
		on(triageReview, implement, routeRevise),
		on(triageReview, publish, routePublish),
	}
	return workflowagent.New(workflowagent.Config{
		Name:        "ticketflow",
		Description: "Takes a Jira ticket to a pull request for human review.",
		Edges:       edges,
		SubAgents:   []agent.Agent{planner, reviewer},
	})
}

func llmAgent(name, desc string, m model.LLM, prompt string, tools []tool.Tool) (agent.Agent, error) {
	return llmagent.New(llmagent.Config{
		Name:                name,
		Description:         desc,
		Model:               m,
		InstructionProvider: harness.Static(prompts.Get(prompt)),
		Tools:               tools,
	})
}

// emitRoute ends an emitting node by taking the edge labelled route, handing out to
// the successor on that edge.
func emitRoute(ctx agent.Context, emit func(*session.Event) error, route string, out any) (any, error) {
	ev := session.NewEvent(ctx, ctx.InvocationID())
	ev.Routes = []string{route}
	ev.Output = out
	return nil, emit(ev)
}

// intake reads the ticket, resolves its repository, and applies the readiness gate.
func (r *run) intake(ctx agent.Context, _ any, emit func(*session.Event) error) (any, error) {
	is, err := r.p.d.Tracker.Issue(ctx, r.key)
	if err != nil {
		return nil, fmt.Errorf("read ticket: %w", err)
	}
	r.issue = is
	s := r.p.d.Settings
	if repo, err := r.p.resolveRepo(is); err != nil {
		r.problems = append(r.problems, err.Error())
	} else {
		r.repo = repo
	}
	r.problems = append(r.problems, gate.Readiness(gate.Ticket{
		Type: is.Type, Summary: is.Summary, Description: is.Description, Labels: is.Labels,
	}, s.Readiness)...)
	if len(r.problems) > 0 {
		return emitRoute(ctx, emit, routeNotReady, routeNotReady)
	}
	if !r.dryRun {
		if err := r.p.d.Tracker.EditLabels(ctx, r.key, nonEmpty(s.ClaimLabel), nonEmpty(s.TriggerLabel)); err != nil {
			return nil, fmt.Errorf("claim ticket: %w", err)
		}
	}
	return emitRoute(ctx, emit, routeReady, routeReady)
}

// prepare checks out the repository on a fresh agent branch and loads its policy.
func (r *run) prepare(ctx agent.Context, _ any, emit func(*session.Event) error) (any, error) {
	s := r.p.d.Settings
	blocked := func(reason string) (any, error) {
		r.problems = []string{reason}
		return emitRoute(ctx, emit, routeBlocked, reason)
	}
	r.base = s.BaseBranch
	if r.base == "" {
		b, err := r.p.d.Host.DefaultBranch(ctx, r.repo)
		if err != nil {
			return nil, fmt.Errorf("default branch: %w", err)
		}
		r.base = b
	}
	ws, err := workspace.Clone(ctx, s.CloneURL(r.repo), r.base, r.dir, s.GitToken)
	if err != nil {
		return nil, err
	}
	r.ws = ws
	policy, err := gate.LoadRepoPolicy(r.dir)
	if err != nil {
		return blocked(fmt.Sprintf("%s is not set up for ticketflow: %v", r.repo, err))
	}
	r.policy = policy
	r.branch = s.BranchPrefix + r.key + "-" + slug(r.issue.Summary)
	exists, err := ws.RemoteBranchExists(ctx, r.branch)
	if err != nil {
		return nil, err
	}
	if exists {
		return blocked(fmt.Sprintf("branch %s already exists; an earlier run's pull request may still be open", r.branch))
	}
	if err := ws.CreateBranch(ctx, r.branch); err != nil {
		return nil, err
	}
	input, err := prompts.Render("plan_input.md", map[string]any{
		"Repo": r.repo, "Base": r.base, "Key": r.key, "Type": r.issue.Type,
		"Summary": r.issue.Summary, "Description": r.issue.Description, "Comments": r.issue.Comments,
		"CheckNames": checkNames(policy.Checks), "RepoInstructions": policy.Instructions,
	})
	if err != nil {
		return nil, err
	}
	return emitRoute(ctx, emit, routeOK, input)
}

type planResult struct {
	Ready     bool     `json:"ready"`
	Questions []string `json:"questions"`
	Plan      string   `json:"plan"`
	Files     []string `json:"files"`
	TestPlan  string   `json:"test_plan"`
	Risk      string   `json:"risk"`
}

// triagePlan turns the planner's answer into a route. The planner is the second half of
// the readiness gate: the structural checks passed, and this is the judgement call.
func (r *run) triagePlan(ctx agent.Context, in any, emit func(*session.Event) error) (any, error) {
	var pr planResult
	if err := decodeJSON(asText(in), &pr); err != nil {
		r.problems = []string{"the planner did not return a usable plan: " + err.Error()}
		return emitRoute(ctx, emit, routeInvalid, routeInvalid)
	}
	if !pr.Ready {
		r.problems = pr.Questions
		if len(r.problems) == 0 {
			r.problems = []string{"the planner judged the ticket not ready but gave no questions"}
		}
		return emitRoute(ctx, emit, routeNotReady, routeNotReady)
	}
	if strings.TrimSpace(pr.Plan) == "" {
		r.problems = []string{"the planner marked the ticket ready but returned an empty plan"}
		return emitRoute(ctx, emit, routeInvalid, routeInvalid)
	}
	r.plan = pr
	return emitRoute(ctx, emit, routeGo, routeGo)
}

// implement hands one attempt to the harness. A harness error does not end the run: the
// checkout is judged by verify either way, and the error becomes feedback.
func (r *run) implement(ctx agent.Context, _ any) (any, error) {
	r.attempts++
	task := harness.Task{
		Dir: r.dir, Key: r.key, Summary: r.issue.Summary, Description: r.issue.Description,
		Plan: r.plan.Plan, TestPlan: r.plan.TestPlan, RepoInstructions: r.policy.Instructions,
		Checks: r.policy.Checks, Attempt: r.attempts, Feedback: r.feedback,
	}
	res, err := r.p.d.Harness.Implement(ctx, task)
	r.agentSummary = res.Summary
	r.feedback = ""
	if err != nil {
		r.log.WarnContext(ctx, "harness reported an error", "attempt", r.attempts, "err", err)
		r.feedback = "The harness stopped with an error: " + err.Error() + "\n\n"
	}
	r.log.InfoContext(ctx, "attempt finished", "attempt", r.attempts, "harness", r.p.d.Harness.Name(), "elapsed", res.Elapsed)
	return "implemented", nil
}

// verify judges the checkout itself — diff gates first, then every repository check.
func (r *run) verify(ctx agent.Context, _ any, emit func(*session.Event) error) (any, error) {
	diff, err := r.ws.Diff(ctx)
	if err != nil {
		return nil, err
	}
	r.diff = diff
	limits := r.p.d.Settings.Limits.Merge(r.policy)
	problems := gate.Diff(diff, limits)

	r.checks = r.checks[:0]
	if len(problems) == 0 {
		for _, c := range r.policy.Checks {
			res, err := workspace.Run(ctx, r.dir, c.Run, r.p.d.Settings.CheckTimeout)
			if err != nil {
				return nil, fmt.Errorf("check %s: %w", c.Name, err)
			}
			r.checks = append(r.checks, checkOutcome{Name: c.Name, Passed: res.Passed(), Output: res.Output})
			if !res.Passed() {
				problems = append(problems, fmt.Sprintf("check %q failed (exit %d%s):\n%s", c.Name, res.ExitCode, timedOut(res), tail(res.Output, 4000)))
			}
		}
	}
	if len(problems) == 0 {
		input, err := prompts.Render("review_input.md", map[string]any{
			"Key": r.key, "Summary": r.issue.Summary, "Description": r.issue.Description,
			"Plan": r.plan.Plan, "Checks": r.checks, "Patch": tail(diff.Patch, 200_000),
			"FileCount": len(diff.Files), "ChangedLines": diff.ChangedLines(),
		})
		if err != nil {
			return nil, err
		}
		return emitRoute(ctx, emit, routePass, input)
	}
	r.feedback += "The change was rejected:\n\n- " + strings.Join(problems, "\n- ")
	if r.attempts >= r.budget {
		r.problems = problems
		return emitRoute(ctx, emit, routeGiveUp, routeGiveUp)
	}
	r.log.InfoContext(ctx, "attempt rejected; retrying", "attempt", r.attempts, "problems", len(problems))
	return emitRoute(ctx, emit, routeRetry, routeRetry)
}

type finding struct {
	Severity string `json:"severity"`
	File     string `json:"file"`
	Issue    string `json:"issue"`
}

type reviewResult struct {
	Verdict  string    `json:"verdict"`
	Summary  string    `json:"summary"`
	Findings []finding `json:"findings"`
}

// triageReview sends blocking findings back to the agent within the revision budget. Past
// it, the PR opens anyway with the findings listed first: a human reviews every PR, and
// a visible open question is more useful to them than a silently abandoned ticket.
func (r *run) triageReview(ctx agent.Context, in any, emit func(*session.Event) error) (any, error) {
	var rv reviewResult
	if err := decodeJSON(asText(in), &rv); err != nil {
		rv = reviewResult{Verdict: "approve", Summary: "The self-review produced no usable verdict (" + err.Error() + "); review this change without it."}
	}
	r.review = rv
	var blocking []finding
	for _, f := range rv.Findings {
		if strings.EqualFold(f.Severity, "blocking") {
			blocking = append(blocking, f)
		}
	}
	if strings.EqualFold(rv.Verdict, "revise") && len(blocking) > 0 && r.revisions < r.p.d.Settings.MaxRevisions {
		r.revisions++
		r.budget++
		var b strings.Builder
		b.WriteString("Self-review found blocking problems with an otherwise passing change:\n")
		for _, f := range blocking {
			fmt.Fprintf(&b, "\n- %s: %s", orDash(f.File), f.Issue)
		}
		r.feedback = b.String()
		return emitRoute(ctx, emit, routeRevise, routeRevise)
	}
	r.openFindings = blocking
	return emitRoute(ctx, emit, routePublish, routePublish)
}

// publish commits, pushes, opens the pull request, and reports back to Jira.
func (r *run) publish(ctx agent.Context, _ any) (any, error) {
	s := r.p.d.Settings
	body, err := prompts.Render("pr_body.md", map[string]any{
		"Key": r.key, "TicketURL": r.issue.URL, "Summary": r.issue.Summary,
		"Plan": r.plan.Plan, "TestPlan": r.plan.TestPlan, "Checks": r.checks,
		"ReviewSummary": r.review.Summary, "OpenFindings": r.openFindings,
		"Risk": orDash(r.plan.Risk), "Attempts": r.attempts, "Harness": r.p.d.Harness.Name(),
		"AgentSummary": tail(r.agentSummary, 8000),
	})
	if err != nil {
		return nil, err
	}
	title := fmt.Sprintf("[%s] %s", r.key, r.issue.Summary)
	out := &Outcome{Key: r.key, Repo: r.repo, Branch: r.branch, PRBody: body}
	if r.dryRun {
		out.Status, out.Patch = StatusDryRun, r.diff.Patch
		r.outcome = out
		return "dry run", nil
	}
	msg := title + "\n\n" + "Implements " + r.issue.URL + "\n\nOpened by ticketflow; requires human review."
	if err := r.ws.Commit(ctx, msg, s.Author); err != nil {
		return nil, err
	}
	if err := r.ws.Push(ctx, r.branch); err != nil {
		return nil, err
	}
	pr, err := r.p.d.Host.OpenPR(ctx, r.repo, github.NewPR{Title: title, Head: r.branch, Base: r.base, Body: body, Draft: s.DraftPRs})
	if err != nil {
		return nil, err
	}
	out.Status, out.PRURL = StatusPublished, pr.URL
	r.outcome = out
	// The PR exists; from here on failures are reported, not fatal.
	if err := r.p.d.Host.AddLabels(ctx, r.repo, pr.Number, nonEmpty(s.PRLabel)); err != nil {
		r.log.WarnContext(ctx, "label PR", "err", err)
	}
	comment, _ := prompts.Render("jira_published.md", map[string]any{"PRURL": pr.URL, "OpenFindings": r.openFindings})
	r.report(ctx, comment, nil, nonEmpty(s.ClaimLabel))
	if s.ReviewTransition != "" {
		if err := r.p.d.Tracker.Transition(ctx, r.key, s.ReviewTransition); err != nil {
			r.log.WarnContext(ctx, "transition ticket", "err", err)
		}
	}
	return "published", nil
}

// bounce hands a ticket that is not ready back to its humans, with the questions.
func (r *run) bounce(ctx agent.Context, _ any) (any, error) {
	s := r.p.d.Settings
	comment, err := prompts.Render("jira_bounce.md", map[string]any{"Problems": r.problems, "TriggerLabel": s.TriggerLabel})
	if err != nil {
		return nil, err
	}
	r.outcome = &Outcome{Status: StatusBounced, Key: r.key, Repo: r.repo, Reason: "ticket not ready", Details: r.problems}
	r.report(ctx, comment, nil, nonEmpty(s.TriggerLabel, s.ClaimLabel))
	return "bounced", nil
}

// escalate gives a claimed ticket back to humans with the reason the agent stopped.
func (r *run) escalate(ctx agent.Context, _ any) (any, error) {
	reason := "the agent could not produce a change that passes the repository's gates"
	if r.attempts == 0 && len(r.problems) > 0 {
		reason = r.problems[0]
	}
	r.escalateNow(ctx, reason, r.problems)
	return "escalated", nil
}

func (r *run) escalateNow(ctx context.Context, reason string, details []string) {
	s := r.p.d.Settings
	r.outcome = &Outcome{Status: StatusEscalated, Key: r.key, Repo: r.repo, Branch: r.branch, Reason: reason, Details: details}
	comment, err := prompts.Render("jira_escalate.md", map[string]any{
		"Reason": reason, "Details": truncateAll(details, 1500),
		"NeedsHumanLabel": s.NeedsHumanLabel, "TriggerLabel": s.TriggerLabel,
	})
	if err != nil {
		comment = "ticketflow stopped and needs a human: " + reason
	}
	r.report(ctx, comment, nonEmpty(s.NeedsHumanLabel), nonEmpty(s.TriggerLabel, s.ClaimLabel))
}

// report posts to Jira unless this is a dry run. Reporting is best-effort: the outcome is
// already decided, and a Jira outage must not turn an opened PR into a failed run.
func (r *run) report(ctx context.Context, comment string, add, remove []string) {
	if r.dryRun {
		return
	}
	if err := r.p.d.Tracker.Comment(ctx, r.key, comment); err != nil {
		r.log.WarnContext(ctx, "comment on ticket", "err", err)
	}
	if err := r.p.d.Tracker.EditLabels(ctx, r.key, add, remove); err != nil {
		r.log.WarnContext(ctx, "update ticket labels", "err", err)
	}
}

func (p *Pipeline) resolveRepo(is jira.Issue) (string, error) {
	s := p.d.Settings
	for _, l := range is.Labels {
		if repo, ok := strings.CutPrefix(l, "repo:"); ok {
			if !s.RepoAllowed(repo) {
				return "", fmt.Errorf("label %q names a repository ticketflow is not allowed to change", l)
			}
			return repo, nil
		}
	}
	if repo, ok := s.ProjectRepos[strings.ToUpper(is.Project)]; ok {
		return repo, nil
	}
	return "", fmt.Errorf("no repository is mapped for project %s; add a repo:<owner/name> label", is.Project)
}

var fence = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)```")

// decodeJSON reads the JSON object a model was asked to reply with, tolerating a
// markdown fence or prose around it.
func decodeJSON(text string, v any) error {
	if m := fence.FindStringSubmatch(text); m != nil {
		text = m[1]
	}
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return errors.New("no JSON object in the reply")
	}
	return json.Unmarshal([]byte(text[start:end+1]), v)
}

func asText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	out := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(out) > 40 {
		out = strings.TrimRight(out[:40], "-")
	}
	if out == "" {
		return "change"
	}
	return out
}

func checkNames(cs []gate.Check) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func nonEmpty(vals ...string) []string {
	var out []string
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func truncateAll(items []string, n int) []string {
	out := make([]string, len(items))
	for i, s := range items {
		if len(s) > n {
			s = s[:n] + "…"
		}
		out[i] = s
	}
	return out
}

func timedOut(r workspace.CommandResult) string {
	if r.TimedOut {
		return ", timed out"
	}
	return ""
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
