package fixflow

import "strings"

// Label is the PR label this engine's workflow uses.
func (e *Engine) Label() string { return e.spec.Label }

// ExtractJSONArray returns the substring from the first '[' to the last ']', so a
// JSON array can be recovered from model output that adds prose or code fences.
func ExtractJSONArray(s string) string {
	i := strings.IndexByte(s, '[')
	j := strings.LastIndexByte(s, ']')
	if i < 0 || j < 0 || j < i {
		return ""
	}
	return s[i : j+1]
}

// ExtractJSONObject returns the substring from the first '{' to the last '}'.
func ExtractJSONObject(s string) string {
	i := strings.IndexByte(s, '{')
	j := strings.LastIndexByte(s, '}')
	if i < 0 || j < 0 || j < i {
		return ""
	}
	return s[i : j+1]
}

// StripFences removes surrounding markdown code fences a model may add and
// normalizes a trailing newline.
func StripFences(out string) string {
	s := strings.TrimSpace(out)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		if j := strings.LastIndex(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	return strings.TrimRight(s, "\n") + "\n"
}
