package reviewer

import (
	"strconv"
	"strings"

	"automation-agent/internal/githubapi"
)

// commentableLines returns the new-side (head) line numbers in a unified-diff patch that GitHub
// will accept a RIGHT-side inline comment on: added ('+') and context (' ') lines. Removed ('-')
// lines have no head-side line and are skipped. A malformed or empty patch yields an empty set,
// so a finding on it is treated as out-of-diff rather than posted at a wrong line.
func commentableLines(patch string) map[int]bool {
	out := map[int]bool{}
	newLine, inHunk := 0, false
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "@@") {
			newLine, inHunk = parseHunkNewStart(line)
			continue
		}
		if !inHunk {
			continue
		}
		switch {
		case strings.HasPrefix(line, "+"):
			out[newLine] = true
			newLine++
		case strings.HasPrefix(line, "-"):
			// removed line: advances the old side only, no head-side line
		case strings.HasPrefix(line, " "):
			out[newLine] = true
			newLine++
		case strings.HasPrefix(line, `\`):
			// "\ No newline at end of file": metadata, not a line
		default:
			// a blank or unexpected line ends this hunk's body (e.g. the trailing split element)
			inHunk = false
		}
	}
	return out
}

// parseHunkNewStart parses the new-file starting line from a hunk header "@@ -a,b +c,d @@",
// returning (c, true). A header it cannot parse yields (0, false) so the body until the next
// header is skipped rather than mis-numbered.
func parseHunkNewStart(header string) (int, bool) {
	plus := strings.IndexByte(header, '+')
	if plus < 0 {
		return 0, false
	}
	rest := header[plus+1:]
	if end := strings.IndexAny(rest, " ,"); end >= 0 {
		rest = rest[:end]
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// diffIndex maps each changed file to the head-side lines an inline comment can target.
type diffIndex map[string]map[int]bool

// newDiffIndex builds the in-diff line index for a set of changed files.
func newDiffIndex(files []githubapi.PRFile) diffIndex {
	idx := make(diffIndex, len(files))
	for _, f := range files {
		idx[f.Path] = commentableLines(f.Patch)
	}
	return idx
}

// inDiff reports whether file:line falls on a commentable head-side line of the diff.
func (d diffIndex) inDiff(file string, line int) bool {
	lines, ok := d[file]
	return ok && lines[line]
}
