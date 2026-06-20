package models

import "testing"

func TestParseKickoffValid(t *testing.T) {
	k, err := ParseKickoff([]byte(`{
		"repo":"acme/api",
		"report":{"issues":[{"file":"a.go","msg":"unchecked error"}]}
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if k.Owner() != "acme" || k.Name() != "api" {
		t.Errorf("owner/name = %s/%s", k.Owner(), k.Name())
	}
	if k.Base != "main" {
		t.Errorf("base default = %q, want main", k.Base)
	}
	if k.ReportText() == "" {
		t.Error("report text should be non-empty")
	}
}

func TestParseKickoffErrors(t *testing.T) {
	cases := map[string]string{
		"bad json":       `{`,
		"missing repo":   `{"report":{"x":1}}`,
		"bad repo":       `{"repo":"noslash","report":{"x":1}}`,
		"missing report": `{"repo":"a/b"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseKickoff([]byte(body)); err == nil {
				t.Errorf("%s: expected error", name)
			}
		})
	}
}

func TestBaseOverride(t *testing.T) {
	k, err := ParseKickoff([]byte(`{"repo":"a/b","base":"develop","report":"some text"}`))
	if err != nil {
		t.Fatal(err)
	}
	if k.Base != "develop" {
		t.Errorf("base = %q, want develop", k.Base)
	}
}
