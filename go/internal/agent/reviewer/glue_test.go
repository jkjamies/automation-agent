package reviewer

import "testing"

func TestDropLowConfidence(t *testing.T) {
	in := []Finding{
		{Message: "a", Confidence: 0.9},
		{Message: "b", Confidence: 0.4},
		{Message: "c", Confidence: 0.6},
	}
	got := dropLowConfidence(in, 0.6)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (>=0.6)", len(got))
	}
	for _, f := range got {
		if f.Confidence < 0.6 {
			t.Errorf("kept low-confidence finding %+v", f)
		}
	}

	// non-positive minimum keeps everything
	if got := dropLowConfidence(in, 0); len(got) != 3 {
		t.Errorf("min 0 kept %d, want all 3", len(got))
	}
}

func TestDedupe(t *testing.T) {
	t.Run("same fingerprint keeps worst severity", func(t *testing.T) {
		in := []Finding{
			{File: "a.go", Line: 1, Dimension: DimSecurity, Message: "issue", Severity: SeverityMedium, Confidence: 0.5},
			{File: "a.go", Line: 1, Dimension: DimSecurity, Message: "issue", Severity: SeverityCritical, Confidence: 0.5},
		}
		got := dedupe(in)
		if len(got) != 1 {
			t.Fatalf("got %d, want 1", len(got))
		}
		if got[0].Severity != SeverityCritical {
			t.Errorf("kept severity %q, want critical", got[0].Severity)
		}
	})

	t.Run("same line across lenses collapses", func(t *testing.T) {
		in := []Finding{
			{File: "a.go", Line: 1, Dimension: DimSecurity, Message: "issue", Severity: SeverityMajor, Confidence: 0.5},
			{File: "a.go", Line: 1, Dimension: DimPerformance, Message: "issue", Severity: SeverityCritical, Confidence: 0.5},
		}
		got := dedupe(in)
		if len(got) != 1 {
			t.Fatalf("got %d, want 1 (cross-lens dedup must ignore dimension)", len(got))
		}
		if got[0].Severity != SeverityCritical {
			t.Errorf("kept severity %q, want critical", got[0].Severity)
		}
	})

	t.Run("severity tie broken by confidence", func(t *testing.T) {
		in := []Finding{
			{File: "a.go", Line: 1, Dimension: DimSecurity, Message: "issue", Severity: SeverityMajor, Confidence: 0.6},
			{File: "a.go", Line: 1, Dimension: DimSecurity, Message: "issue", Severity: SeverityMajor, Confidence: 0.95},
		}
		got := dedupe(in)
		if len(got) != 1 || got[0].Confidence != 0.95 {
			t.Fatalf("got %+v, want the higher-confidence one", got)
		}
	})

	t.Run("distinct findings preserved in order", func(t *testing.T) {
		in := []Finding{
			{File: "a.go", Line: 1, Dimension: DimSecurity, Message: "x"},
			{File: "b.go", Line: 2, Dimension: DimPerformance, Message: "y"},
		}
		if got := dedupe(in); len(got) != 2 || got[0].File != "a.go" || got[1].File != "b.go" {
			t.Errorf("got %+v, want both in order", got)
		}
	})
}

func TestDemoteToNitpick(t *testing.T) {
	in := []Finding{{Severity: SeverityCritical}, {Severity: SeverityMajor}}
	got := demoteToNitpick(in)
	for _, f := range got {
		if f.Severity != SeverityNitpick {
			t.Errorf("severity %q not demoted", f.Severity)
		}
	}
}
