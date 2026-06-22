package ingest

import (
	"testing"
	"time"
)

func TestKindValid(t *testing.T) {
	valid := []Kind{KindCronDaily, KindCronWeekly, KindLint, KindCoverage, KindCI}
	for _, k := range valid {
		if !k.Valid() {
			t.Errorf("%q should be valid", k)
		}
	}
	if Kind("jira").Valid() {
		t.Error("unknown kind should be invalid")
	}
}

func TestNew(t *testing.T) {
	at := time.Unix(1718870400, 0)
	e := New(KindLint, "webhook:/lint", []byte(`{"x":1}`), at)
	if e.Kind != KindLint || e.Source != "webhook:/lint" {
		t.Errorf("unexpected envelope: %+v", e)
	}
	if string(e.Payload) != `{"x":1}` {
		t.Errorf("payload = %s", e.Payload)
	}
	if !e.ReceivedAt.Equal(at) {
		t.Errorf("ReceivedAt = %v, want %v", e.ReceivedAt, at)
	}
}
