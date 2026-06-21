package setup

import "strings"

// SafeName sanitizes s into an ADK agent name: every character that is not an ASCII
// letter or digit becomes an underscore. Shared by the workflow agents that derive a
// sub-agent name from a repo or file path.
func SafeName(s string) string {
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
