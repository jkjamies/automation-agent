package scheduler

import (
	"testing"
	"time"

	"github.com/jkjamies/automation-agent/internal/ingest"
)

func TestAddValidAndInvalid(t *testing.T) {
	s := New(func(ingest.Envelope) {})
	if err := s.Add("0 9 * * *", ingest.KindCronDaily); err != nil {
		t.Errorf("valid daily spec: %v", err)
	}
	if err := s.Add("0 9 * * 1", ingest.KindCronWeekly); err != nil {
		t.Errorf("valid weekly spec: %v", err)
	}
	if s.Entries() != 2 {
		t.Errorf("entries = %d, want 2", s.Entries())
	}
	if err := s.Add("not a cron spec", ingest.KindCronDaily); err == nil {
		t.Error("expected error for invalid spec")
	}
}

func TestStartFiresAndStop(t *testing.T) {
	ch := make(chan ingest.Envelope, 1)
	s := New(func(e ingest.Envelope) {
		select {
		case ch <- e:
		default:
		}
	})
	if err := s.Add("@every 1s", ingest.KindCronDaily); err != nil {
		t.Fatalf("Add: %v", err)
	}
	s.Start()
	defer s.Stop()

	select {
	case e := <-ch:
		if e.Kind != ingest.KindCronDaily {
			t.Errorf("fired kind = %q", e.Kind)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("schedule did not fire within 3s")
	}
}

func TestTriggerEmitsEnvelope(t *testing.T) {
	var got ingest.Envelope
	s := New(func(e ingest.Envelope) { got = e })
	fixed := time.Unix(1718870400, 0)
	s.now = func() time.Time { return fixed }

	s.trigger(ingest.KindCronWeekly)

	if got.Kind != ingest.KindCronWeekly {
		t.Errorf("kind = %q, want %q", got.Kind, ingest.KindCronWeekly)
	}
	if got.Source != "scheduler" {
		t.Errorf("source = %q", got.Source)
	}
	if !got.ReceivedAt.Equal(fixed) {
		t.Errorf("ReceivedAt = %v, want %v", got.ReceivedAt, fixed)
	}
}
