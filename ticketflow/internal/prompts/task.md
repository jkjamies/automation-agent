Implement Jira ticket {{.Key}} in this repository (attempt {{.Attempt}}).

<ticket key="{{.Key}}">
# {{.Summary}}

{{.Description}}
</ticket>

## Approved plan

{{.Plan}}
{{- if .TestPlan}}

## Test plan

{{.TestPlan}}
{{- end}}

## Verification

The change is accepted only if these checks pass: {{join .CheckNames ", "}}. Run them
yourself before finishing.
{{- if .RepoInstructions}}

## Repository instructions

{{.RepoInstructions}}
{{- end}}
{{- if .Feedback}}

## What went wrong last time

Your previous attempt was rejected. Address this, keeping the parts that were right:

{{.Feedback}}
{{- end}}

