Review this change for Jira ticket {{.Key}}.

<ticket key="{{.Key}}">
# {{.Summary}}

{{.Description}}
</ticket>

## Plan the agent followed

{{.Plan}}

## Checks

{{range .Checks}}- {{.Name}}: {{if .Passed}}passed{{else}}FAILED{{end}}
{{end}}
## Diff ({{.FileCount}} files, {{.ChangedLines}} lines)

```diff
{{.Patch}}
```

