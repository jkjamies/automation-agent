Implements [{{.Key}}]({{.TicketURL}}): {{.Summary}}

> Opened by ticketflow, an unattended agent. **It has not been reviewed by a human.**
> Review it as you would any contributor's change; nothing here merges without an approval.

## Plan

{{.Plan}}
{{- if .TestPlan}}

## Tests

{{.TestPlan}}
{{- end}}

## Verification before this PR opened

{{range .Checks}}- `{{.Name}}`: {{if .Passed}}passed{{else}}failed{{end}}
{{end}}
## Self-review

{{if .ReviewSummary}}{{.ReviewSummary}}{{else}}No summary.{{end}}
{{- if .OpenFindings}}

Findings the agent did not resolve within its revision budget — please look at these first:
{{range .OpenFindings}}
- {{if .File}}`{{.File}}`: {{end}}{{.Issue}}
{{- end}}
{{- end}}

## Run

- Risk (planner's estimate): {{.Risk}}
- Attempts: {{.Attempts}} · harness: {{.Harness}}
{{- if .AgentSummary}}

<details><summary>Agent's own summary</summary>

{{.AgentSummary}}

</details>
{{- end}}
