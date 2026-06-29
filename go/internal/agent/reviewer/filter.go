package reviewer

import (
	"path"
	"regexp"
	"strings"

	"automation-agent/internal/githubapi"
)

// fileFilter drops changed files that are not worth reviewing — generated code, vendored
// trees, lockfiles, minified bundles, snapshots, and binaries — before any size accounting
// or model call. Filtering first is the biggest cheap win: most "huge" PRs are mostly
// lockfile/vendor churn and shrink to a handful of real files (spec Design §4 / Decision 4).
type fileFilter struct {
	patterns []globPattern
}

// globPattern is one compiled exclude glob. A pattern with no '/' matches against the file's
// basename (e.g. "*.min.js", "go.sum"); a pattern with a '/' matches against the full path
// (e.g. "vendor/**"). "**" matches across path separators; "*" and "?" do not.
type globPattern struct {
	re       *regexp.Regexp
	basename bool
}

// newFileFilter compiles the exclude globs. Blank entries (e.g. a trailing comma in the env
// value) are skipped. Every glob compiles — globToRegexp escapes all regexp metacharacters —
// so this cannot fail.
func newFileFilter(globs []string) *fileFilter {
	f := &fileFilter{}
	for _, g := range globs {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		f.patterns = append(f.patterns, globPattern{re: globToRegexp(g), basename: !strings.Contains(g, "/")})
	}
	return f
}

// excluded reports whether a path matches any exclude glob.
func (f *fileFilter) excluded(p string) bool {
	base := path.Base(p)
	for _, pat := range f.patterns {
		target := p
		if pat.basename {
			target = base
		}
		if pat.re.MatchString(target) {
			return true
		}
	}
	return false
}

// avgDiffLineBytes is the per-changed-line byte estimate charged when GitHub omits a file's
// patch for an oversized text diff. A unified-diff line is its content plus a one-char +/-
// prefix and a newline; real source lines average well above this, so the estimate is
// deliberately conservative: the size gate must over-, never under-, charge an omitted diff so
// a very large PR cannot slip the byte cap by changing files too big for GitHub to diff.
const avgDiffLineBytes = 50

// patchBytes is the diff-byte cost charged for one kept file. When GitHub returns the patch it
// is the exact byte length. When GitHub omits it for an oversized text file (empty patch but
// non-zero line counts) it is estimated from the reported additions+deletions, so the file is
// not undercounted as zero diff bytes. Binary files (no patch, no line counts) cost nothing.
func patchBytes(fl githubapi.PRFile) int {
	if fl.Patch != "" {
		return len(fl.Patch)
	}
	if lines := fl.Additions + fl.Deletions; lines > 0 {
		return lines * avgDiffLineBytes
	}
	return 0
}

// apply returns the kept (non-excluded) files and the total size of their patches in bytes.
// Size is computed on the filtered set so the size gate sees real review surface, not churn;
// files whose patch GitHub omitted are charged conservatively (see patchBytes) so an oversized
// PR cannot undercount its way past the byte cap.
func (f *fileFilter) apply(files []githubapi.PRFile) (kept []githubapi.PRFile, diffBytes int) {
	for _, fl := range files {
		if f.excluded(fl.Path) {
			continue
		}
		kept = append(kept, fl)
		diffBytes += patchBytes(fl)
	}
	return kept, diffBytes
}

// globToRegexp compiles a glob into an anchored regexp. "**" becomes ".*" (crosses path
// separators), "*" becomes "[^/]*" and "?" becomes "[^/]" (within one segment); every other
// regexp metacharacter is escaped so it matches literally. Because all metacharacters are
// either escaped or rewritten, the result is always a valid pattern — MustCompile cannot
// panic on it.
func globToRegexp(glob string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				b.WriteString(".*")
				i++ // consume the second '*'
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '.', '+', '(', ')', '|', '[', ']', '{', '}', '^', '$', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}
