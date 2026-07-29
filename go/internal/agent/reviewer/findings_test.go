package reviewer

import (
	"math"
	"testing"
)

func TestParseFindings(t *testing.T) {
	t.Run("plain array", func(t *testing.T) {
		raw := `[{"file":"a.go","line":3,"dimension":"security","severity":"critical","message":"sql injection","confidence":0.9}]`
		got := parseFindings(raw)
		if len(got) != 1 {
			t.Fatalf("got %d findings, want 1", len(got))
		}
		f := got[0]
		if f.File != "a.go" || f.Line != 3 || f.Dimension != DimSecurity || f.Severity != SeverityCritical || f.Confidence != 0.9 {
			t.Errorf("unexpected finding: %+v", f)
		}
	})

	t.Run("fenced and prose-wrapped", func(t *testing.T) {
		raw := "Here are the issues I found:\n```json\n[{\"file\":\"b.go\",\"dimension\":\"performance\",\"severity\":\"medium\",\"message\":\"n+1\"}]\n```\nDone."
		got := parseFindings(raw)
		if len(got) != 1 || got[0].File != "b.go" || got[0].Dimension != DimPerformance {
			t.Fatalf("got %+v, want one performance finding", got)
		}
	})

	t.Run("empty array", func(t *testing.T) {
		if got := parseFindings("[]"); len(got) != 0 {
			t.Errorf("got %d, want 0", len(got))
		}
	})

	t.Run("recovers a valid array past bracketed prose", func(t *testing.T) {
		// A greedy first-'['/last-']' slice would start at "[1]" and fail to decode, dropping the
		// real findings; scanning for the first array that decodes recovers them.
		raw := `I checked refs [1] and [2]. Findings: [{"file":"a.go","message":"real","severity":"major"}]`
		got := parseFindings(raw)
		if len(got) != 1 || got[0].File != "a.go" {
			t.Fatalf("got %+v, want the real finding recovered past bracketed prose", got)
		}
	})

	t.Run("skips an empty array preceding a populated one", func(t *testing.T) {
		raw := `First pass: []. After review: [{"file":"b.go","message":"x","severity":"medium"}]`
		got := parseFindings(raw)
		if len(got) != 1 || got[0].File != "b.go" {
			t.Fatalf("got %+v, want the populated array", got)
		}
	})

	t.Run("malformed yields none, never errors", func(t *testing.T) {
		for _, raw := range []string{"", "not json at all", "[{broken", "{\"not\":\"an array\"}"} {
			if got := parseFindings(raw); got != nil {
				t.Errorf("parseFindings(%q) = %+v, want nil", raw, got)
			}
		}
	})

	t.Run("blank message dropped", func(t *testing.T) {
		raw := `[{"file":"a.go","message":"   ","severity":"major"},{"file":"b.go","message":"real","severity":"major"}]`
		got := parseFindings(raw)
		if len(got) != 1 || got[0].File != "b.go" {
			t.Fatalf("got %+v, want only the real finding", got)
		}
	})

	t.Run("unknown severity and dimension normalize", func(t *testing.T) {
		raw := `[{"file":"a.go","message":"x","severity":"showstopper","dimension":"vibes"}]`
		got := parseFindings(raw)
		if got[0].Severity != SeverityNitpick || got[0].Dimension != DimOther {
			t.Errorf("got severity %q dim %q, want nitpick/other", got[0].Severity, got[0].Dimension)
		}
	})

	t.Run("dimension spacing folded", func(t *testing.T) {
		raw := `[{"file":"a.go","message":"x","dimension":"Runtime Safety"}]`
		if got := parseFindings(raw); got[0].Dimension != DimRuntimeSafety {
			t.Errorf("got %q, want runtime_safety", got[0].Dimension)
		}
	})

	t.Run("confidence clamped and defaulted", func(t *testing.T) {
		raw := `[{"file":"a.go","message":"x","confidence":0},{"file":"b.go","message":"y","confidence":5},{"file":"c.go","message":"z","confidence":-1}]`
		got := parseFindings(raw)
		if got[0].Confidence != 0.5 || got[1].Confidence != 1 || got[2].Confidence != 0.5 {
			t.Errorf("confidences = %v,%v,%v; want 0.5,1,0.5", got[0].Confidence, got[1].Confidence, got[2].Confidence)
		}
	})
}

func TestFingerprintStable(t *testing.T) {
	a := Finding{File: "a.go", Line: 3, Dimension: DimSecurity, Message: "SQL  Injection here"}
	b := Finding{File: "a.go", Line: 3, Dimension: DimSecurity, Message: "sql injection HERE"}
	if a.fingerprint() != b.fingerprint() {
		t.Errorf("messages differing only by case/space must fingerprint equal:\n%q\n%q", a.fingerprint(), b.fingerprint())
	}
	c := Finding{File: "a.go", Line: 4, Dimension: DimSecurity, Message: "sql injection here"}
	if a.fingerprint() == c.fingerprint() {
		t.Error("different lines must fingerprint differently")
	}
}

func TestClampThreshold(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{-1, 0}, {0, 0}, {0.6, 0.6}, {1, 1}, {2, 1},
		{math.Inf(1), 1}, {math.Inf(-1), 0}, {math.NaN(), 0},
	}
	for _, c := range cases {
		if got := clampThreshold(c.in); got != c.want {
			t.Errorf("clampThreshold(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestMaxBacktickRun(t *testing.T) {
	cases := map[string]int{"": 0, "no ticks": 0, "a `b` c": 1, "``x```y``": 3}
	for in, want := range cases {
		if got := maxBacktickRun(in); got != want {
			t.Errorf("maxBacktickRun(%q) = %d, want %d", in, got, want)
		}
	}
}

// A lens that prefaces its findings with some other JSON array must not silence the review.
// JSON is permissive about extra fields, so an unrelated array of objects decodes cleanly into
// findingWire records with every field zero — and committing to it would leave every element
// message-less, so the lens reports nothing and the review calls the code clean. The scan has to
// keep going until an array yields a usable finding.
func TestParseFindingsSkipsUnrelatedLeadingArray(t *testing.T) {
	raw := `Files I looked at: [{"path":"a.go"},{"path":"b.go"}]

Findings:
[{"file":"a.go","line":2,"severity":"major","dimension":"security","message":"sqli"}]`

	got := parseFindings(raw)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1 — an unrelated leading array swallowed the real one: %+v", len(got), got)
	}
	if got[0].Message != "sqli" || got[0].File != "a.go" || got[0].Line != 2 {
		t.Errorf("finding = %+v", got[0])
	}
}

// The same shape one level up: an array of well-formed-looking objects that simply have no
// message is not findings, however plausible it looks.
func TestParseFindingsSkipsMessagelessArray(t *testing.T) {
	raw := `[{"file":"a.go","line":1,"severity":"critical"},{"file":"b.go","line":2}]
[{"file":"c.go","line":3,"severity":"medium","dimension":"performance","message":"n+1"}]`

	got := parseFindings(raw)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(got), got)
	}
	if got[0].File != "c.go" || got[0].Message != "n+1" {
		t.Errorf("finding = %+v", got[0])
	}
}

// And when nothing in the text is usable, the answer stays "no findings" rather than an error or
// a pile of empty ones.
func TestParseFindingsAllUnusable(t *testing.T) {
	if got := parseFindings(`prose [{"path":"a.go"}] more prose [{"file":"b.go"}] end`); len(got) != 0 {
		t.Errorf("got %+v, want none", got)
	}
}
