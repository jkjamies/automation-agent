package reviewer

import "sort"

// level is a per-dimension and overall grade. Count-based, not a synthetic 0–100 score
// (spec Decision 5). Ordered so "worst level wins".
type level int

const (
	levelGreen level = iota
	levelYellow
	levelRed
)

// String renders a level as its scorecard glyph.
func (l level) String() string {
	switch l {
	case levelRed:
		return "🔴"
	case levelYellow:
		return "🟡"
	default:
		return "🟢"
	}
}

// dimScore is the severity histogram for one dimension plus its derived level.
type dimScore struct {
	dimension Dimension
	critical  int
	major     int
	medium    int
	nitpick   int
	level     level
}

// scorecard is the count-based review result: a per-dimension histogram and an overall grade.
type scorecard struct {
	dims    []dimScore // sorted by dimension for stable rendering
	overall level
	total   int // total findings counted (after the confidence gate)
}

// dimLevel derives a dimension's level from its severity counts (spec Decision 5 thresholds,
// pilot-tunable): red on any critical or ≥2 major; yellow on any major or ≥3 medium; else
// green.
func dimLevel(s dimScore) level {
	switch {
	case s.critical >= 1 || s.major >= 2:
		return levelRed
	case s.major >= 1 || s.medium >= 3:
		return levelYellow
	default:
		return levelGreen
	}
}

// scoreFindings builds the scorecard from already-confidence-gated findings (spec Decision 5):
// a per-dimension histogram + level, then overall = critical-cap (any critical in an always-on
// critical dimension → red) combined with the worst dimension level.
func scoreFindings(findings []Finding) scorecard {
	byDim := make(map[Dimension]*dimScore)
	criticalCap := false
	for _, f := range findings {
		d, ok := byDim[f.Dimension]
		if !ok {
			d = &dimScore{dimension: f.Dimension}
			byDim[f.Dimension] = d
		}
		switch f.Severity {
		case SeverityCritical:
			d.critical++
			if criticalDimensions[f.Dimension] {
				criticalCap = true
			}
		case SeverityMajor:
			d.major++
		case SeverityMedium:
			d.medium++
		default:
			d.nitpick++
		}
	}

	card := scorecard{total: len(findings)}
	worst := levelGreen
	for _, d := range byDim {
		d.level = dimLevel(*d)
		if d.level > worst {
			worst = d.level
		}
		card.dims = append(card.dims, *d)
	}
	sort.Slice(card.dims, func(i, j int) bool { return card.dims[i].dimension < card.dims[j].dimension })

	card.overall = worst
	if criticalCap {
		card.overall = levelRed
	}
	return card
}
