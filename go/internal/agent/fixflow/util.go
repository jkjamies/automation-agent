package fixflow

import (
	"encoding/json"
	"strings"
)

// Label is the PR label this engine's workflow uses.
func (e *Engine) Label() string { return e.spec.Label }

// ExtractJSONArray returns the first complete JSON array in model output (which may add
// prose or code fences), scanning from the first '[' and decoding a single value — so
// trailing prose or a stray bracket can't corrupt the span. "" if none parses.
func ExtractJSONArray(s string) string {
	return firstJSONValue(s, '[')
}

// ExtractJSONObject returns the first complete JSON object in model output. "" if none parses.
func ExtractJSONObject(s string) string {
	return firstJSONValue(s, '{')
}

// firstJSONValue returns the first complete JSON value beginning at an opener byte,
// decoding one value so trailing prose or a stray bracket can't corrupt the span.
func firstJSONValue(s string, opener byte) string {
	for start := strings.IndexByte(s, opener); start >= 0; {
		var raw json.RawMessage
		if err := json.NewDecoder(strings.NewReader(s[start:])).Decode(&raw); err == nil {
			return string(raw)
		}
		next := strings.IndexByte(s[start+1:], opener)
		if next < 0 {
			break
		}
		start += 1 + next
	}
	return ""
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
