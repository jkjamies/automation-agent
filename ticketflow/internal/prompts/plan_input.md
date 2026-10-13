Plan the implementation of this ticket in repository {{.Repo}} (base branch {{.Base}}).

<ticket key="{{.Key}}" type="{{.Type}}">
# {{.Summary}}

{{.Description}}
{{- if .Comments}}

## Comments
{{range .Comments}}
- {{.Author}}: {{.Body}}
{{- end}}
{{- end}}
</ticket>

The pull request must pass these repository checks: {{join .CheckNames ", "}}.
{{- if .RepoInstructions}}

Repository-specific instructions for agents:
{{.RepoInstructions}}
{{- end}}

