package reviewer

import "testing"

func f(dim Dimension, sev Severity) Finding {
	return Finding{File: "a.go", Dimension: dim, Severity: sev, Message: string(dim) + " " + string(sev), Confidence: 1}
}

func TestDimLevel(t *testing.T) {
	cases := []struct {
		name string
		s    dimScore
		want level
	}{
		{"one critical -> red", dimScore{critical: 1}, levelRed},
		{"two major -> red", dimScore{major: 2}, levelRed},
		{"one major -> yellow", dimScore{major: 1}, levelYellow},
		{"three medium -> yellow", dimScore{medium: 3}, levelYellow},
		{"two medium -> green", dimScore{medium: 2}, levelGreen},
		{"only nitpicks -> green", dimScore{nitpick: 9}, levelGreen},
	}
	for _, c := range cases {
		if got := dimLevel(c.s); got != c.want {
			t.Errorf("%s: dimLevel = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestScoreFindings(t *testing.T) {
	t.Run("critical in security caps overall red even if isolated", func(t *testing.T) {
		card := scoreFindings([]Finding{f(DimSecurity, SeverityCritical)})
		if card.overall != levelRed {
			t.Errorf("overall = %v, want red", card.overall)
		}
	})

	t.Run("critical in a non-critical dimension still reds that dim but via worst-level", func(t *testing.T) {
		card := scoreFindings([]Finding{f(DimReadability, SeverityCritical)})
		if card.overall != levelRed {
			t.Errorf("overall = %v, want red (worst dim level)", card.overall)
		}
	})

	t.Run("worst-level wins without critical-cap", func(t *testing.T) {
		// one major (yellow dim) + three medium in another dim (yellow) -> overall yellow
		card := scoreFindings([]Finding{
			f(DimPerformance, SeverityMajor),
			f(DimReadability, SeverityMedium), f(DimReadability, SeverityMedium), f(DimReadability, SeverityMedium),
		})
		if card.overall != levelYellow {
			t.Errorf("overall = %v, want yellow", card.overall)
		}
	})

	t.Run("all green", func(t *testing.T) {
		card := scoreFindings([]Finding{f(DimMaintainability, SeverityNitpick), f(DimPerformance, SeverityMedium)})
		if card.overall != levelGreen {
			t.Errorf("overall = %v, want green", card.overall)
		}
		if card.total != 2 {
			t.Errorf("total = %d, want 2", card.total)
		}
	})

	t.Run("histogram and stable dim order", func(t *testing.T) {
		card := scoreFindings([]Finding{
			f(DimSecurity, SeverityMajor), f(DimSecurity, SeverityMedium),
			f(DimPerformance, SeverityNitpick),
		})
		if len(card.dims) != 2 {
			t.Fatalf("dims = %d, want 2", len(card.dims))
		}
		// sorted alphabetically: performance < security
		if card.dims[0].dimension != DimPerformance || card.dims[1].dimension != DimSecurity {
			t.Errorf("dim order = %v,%v; want performance,security", card.dims[0].dimension, card.dims[1].dimension)
		}
		if card.dims[1].major != 1 || card.dims[1].medium != 1 {
			t.Errorf("security histogram = %+v", card.dims[1])
		}
	})

	t.Run("empty findings -> green, zero total", func(t *testing.T) {
		card := scoreFindings(nil)
		if card.overall != levelGreen || card.total != 0 || len(card.dims) != 0 {
			t.Errorf("empty score = %+v", card)
		}
	})
}

func TestLevelString(t *testing.T) {
	if levelRed.String() != "🔴" || levelYellow.String() != "🟡" || levelGreen.String() != "🟢" {
		t.Error("level glyphs wrong")
	}
}
