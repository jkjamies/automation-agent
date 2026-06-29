package reviewer

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Severity ranks a finding's importance. critical/major/medium are actionable (posted inline
// once publishing lands); nitpick is collapsed/low-noise.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityMajor    Severity = "major"
	SeverityMedium   Severity = "medium"
	SeverityNitpick  Severity = "nitpick"
)

// severityRank orders severities (higher = worse) so dedup can keep the worst of a pair.
func severityRank(s Severity) int {
	switch s {
	case SeverityCritical:
		return 4
	case SeverityMajor:
		return 3
	case SeverityMedium:
		return 2
	case SeverityNitpick:
		return 1
	default:
		return 0
	}
}

// normalizeSeverity maps a model-emitted severity onto a known value, defaulting an unknown
// or blank value to nitpick — the safe, low-noise bucket (a local model is biased toward
// fewer-but-real, spec Decision 13).
func normalizeSeverity(s string) Severity {
	switch Severity(strings.ToLower(strings.TrimSpace(s))) {
	case SeverityCritical:
		return SeverityCritical
	case SeverityMajor:
		return SeverityMajor
	case SeverityMedium:
		return SeverityMedium
	default:
		return SeverityNitpick
	}
}

// Dimension is one of the review lenses (spec Decision 3). A category agent tags each finding
// with the dimension it belongs to; the scorecard is a per-dimension histogram.
type Dimension string

const (
	DimRuntimeSafety    Dimension = "runtime_safety"
	DimErrorHandling    Dimension = "error_handling"
	DimSecurity         Dimension = "security"
	DimPerformance      Dimension = "performance"
	DimPatternViolation Dimension = "pattern_violation"
	DimMaintainability  Dimension = "maintainability"
	DimReadability      Dimension = "readability"
	DimDocumentation    Dimension = "documentation"
	DimAccessibility    Dimension = "accessibility"
	DimArchitecture     Dimension = "architectural_alignment"
	DimTestability      Dimension = "testability"
	DimTestCoverage     Dimension = "test_coverage"
	DimOther            Dimension = "other"
)

// knownDimensions is the lookup for normalizeDimension. A model emits the dimension as free
// text; anything unrecognized collapses to DimOther.
var knownDimensions = map[Dimension]bool{
	DimRuntimeSafety: true, DimErrorHandling: true, DimSecurity: true, DimPerformance: true,
	DimPatternViolation: true, DimMaintainability: true, DimReadability: true,
	DimDocumentation: true, DimAccessibility: true, DimArchitecture: true,
	DimTestability: true, DimTestCoverage: true, DimOther: true,
}

// normalizeDimension maps a model-emitted dimension onto a known value (lowercased, spaces and
// hyphens folded to underscores), defaulting an unrecognized one to DimOther.
func normalizeDimension(s string) Dimension {
	d := Dimension(strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), " ", "_"), "-", "_"))
	if knownDimensions[d] {
		return d
	}
	return DimOther
}

// criticalDimensions are the always-on dimensions where a critical finding caps the overall
// grade to red regardless of the other lenses (spec Decision 5).
var criticalDimensions = map[Dimension]bool{DimSecurity: true, DimRuntimeSafety: true}

// Finding is one review observation from a category agent or the glue pass.
type Finding struct {
	File       string
	Line       int
	Dimension  Dimension
	Severity   Severity
	Message    string
	Suggestion string  // optional ```suggestion body (a localized in-diff fix)
	FixPrompt  string  // optional "Prompt for AI agents" body (feeds the future fix hand-off)
	Confidence float64 // 0..1; below REVIEW_MIN_CONFIDENCE is dropped before scoring
}

// fingerprint identifies a finding across re-reviews for reconciliation (spec Decision 11) and
// for cross-lens dedup: file + line + a normalized message. Dimension is deliberately omitted so
// the same line/message surfaced by two different lenses collapses to one finding.
func (f Finding) fingerprint() string {
	return fmt.Sprintf("%s:%d:%s", f.File, f.Line, normalizeMessage(f.Message))
}

// normalizeMessage lowercases and collapses internal whitespace so trivially different
// renderings of the same message fingerprint identically.
func normalizeMessage(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// findingWire is the JSON shape a category agent is prompted to emit. Severity and dimension
// are free strings normalized on ingest; confidence is clamped.
type findingWire struct {
	File       string  `json:"file"`
	Line       int     `json:"line"`
	Dimension  string  `json:"dimension"`
	Severity   string  `json:"severity"`
	Message    string  `json:"message"`
	Suggestion string  `json:"suggestion"`
	FixPrompt  string  `json:"fix_prompt"`
	Confidence float64 `json:"confidence"`
}

// parseFindings extracts findings from a category agent's raw output. Local models wrap JSON
// in prose or ``` fences and occasionally emit nothing, and adk-go's OutputSchema does not
// enforce a shape (v1.4.0 leaves validation a TODO) — so this is best-effort by design: it
// pulls the first JSON array out of the text and tolerates a malformed body by returning no
// findings (empty = success, spec Decisions 2/13). It never errors, so a garbled response
// degrades to "no findings for this lens" rather than failing the whole review.
func parseFindings(raw string) []Finding {
	wires := decodeFirstFindingArray(raw)
	if len(wires) == 0 {
		return nil
	}
	out := make([]Finding, 0, len(wires))
	for _, w := range wires {
		msg := strings.TrimSpace(w.Message)
		if msg == "" {
			continue // a finding with no message is unusable
		}
		out = append(out, Finding{
			File:       strings.TrimSpace(w.File),
			Line:       w.Line,
			Dimension:  normalizeDimension(w.Dimension),
			Severity:   normalizeSeverity(w.Severity),
			Message:    msg,
			Suggestion: strings.TrimSpace(w.Suggestion),
			FixPrompt:  strings.TrimSpace(w.FixPrompt),
			Confidence: clampConfidence(w.Confidence),
		})
	}
	return out
}

// decodeFirstFindingArray scans raw for the first '[' that begins a JSON array decoding cleanly
// into the findings shape, returning its elements. Scanning for a *decodable* array (rather than
// slicing the first '[' to the last ']') tolerates ``` fences, prose, and stray brackets without
// over-grabbing: a bracketed phrase like "[see below]" fails to decode and the scan moves on. A
// valid but empty array is skipped in case a populated one follows; if none decodes, it returns
// nil (best-effort: empty = success). json.Decoder reads just the first value, so trailing prose
// after the array is ignored.
func decodeFirstFindingArray(raw string) []findingWire {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '[' {
			continue
		}
		var wires []findingWire
		if err := json.NewDecoder(strings.NewReader(raw[i:])).Decode(&wires); err != nil {
			continue
		}
		if len(wires) > 0 {
			return wires
		}
	}
	return nil
}

// clampThreshold normalizes a confidence *threshold* into [0,1]. Unlike clampConfidence (which
// treats 0 as "unspecified"), a 0 threshold is meaningful — it disables the gate (keep all) — so
// NaN and negatives fold to 0 (keep all, the safe default) and values above 1 fold to 1.
func clampThreshold(f float64) float64 {
	switch {
	case !(f >= 0): // also catches NaN
		return 0
	case f > 1:
		return 1
	default:
		return f
	}
}

// findingsJSON renders findings as a compact JSON array for embedding in the glue prompt. On the
// (practically impossible) marshal error it falls back to an empty array.
func findingsJSON(findings []Finding) string {
	b, err := json.Marshal(findings)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// clampConfidence keeps confidence in [0,1]. A zero/absent value is treated as 0.5
// (unspecified) so a model that omits the field is not silently dropped by the gate.
func clampConfidence(c float64) float64 {
	switch {
	case c <= 0:
		return 0.5
	case c > 1:
		return 1
	default:
		return c
	}
}
