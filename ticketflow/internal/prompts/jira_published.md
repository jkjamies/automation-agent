ticketflow opened a pull request for this ticket: {{.PRURL}}

It still needs human review; nothing merges without an approval.
{{- if .OpenFindings}}

The agent's self-review left {{len .OpenFindings}} finding(s) unresolved; they are listed in the pull request.
{{- end}}
