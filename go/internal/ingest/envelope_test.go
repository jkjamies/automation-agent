package ingest

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestKindValid(t *testing.T) {
	valid := []Kind{KindCronDaily, KindLint, KindCoverage, KindCI}
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

// The wire codec round-trips every field, including a payload that is not valid UTF-8
// (it travels as base64, so arbitrary bytes survive) and an empty/nil payload.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	at := time.Unix(1718870400, 0).UTC()
	cases := map[string][]byte{
		"json payload": []byte(`{"action":"completed"}`),
		"binary bytes": {0x00, 0xff, 0xfe, 0x10, 0x80},
		"empty":        {},
		"nil":          nil,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			in := New(KindCI, "webhook:/github", payload, at)
			b, err := Encode(in)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			out, err := Decode(b)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if out.Kind != in.Kind || out.Source != in.Source {
				t.Errorf("kind/source = %q/%q, want %q/%q", out.Kind, out.Source, in.Kind, in.Source)
			}
			if !out.ReceivedAt.Equal(in.ReceivedAt) {
				t.Errorf("ReceivedAt = %v, want %v", out.ReceivedAt, in.ReceivedAt)
			}
			// nil and empty both decode back to a zero-length payload.
			if len(out.Payload) != len(payload) || (len(payload) > 0 && !bytes.Equal(out.Payload, payload)) {
				t.Errorf("payload = %v, want %v", out.Payload, payload)
			}
		})
	}
}

// The wire form carries the agreed cross-port field names and base64-encodes the payload.
func TestEncodeWireShape(t *testing.T) {
	b, err := Encode(New(KindLint, "webhook:/lint", []byte("hi"), time.Unix(0, 0).UTC()))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for _, field := range []string{`"kind":"lint"`, `"source":"webhook:/lint"`, `"received_at":`, `"payload":"aGk="`} {
		if !strings.Contains(string(b), field) {
			t.Errorf("wire form %s missing %s", b, field)
		}
	}
}

// A malformed body, an unknown kind, and an undecodable payload are all permanent (poison)
// errors.
func TestDecodeRejectsBadInput(t *testing.T) {
	if _, err := Decode([]byte("not json")); err == nil {
		t.Error("malformed JSON should be rejected")
	}
	if _, err := Decode([]byte(`{"kind":"jira","source":"x"}`)); err == nil {
		t.Error("unknown kind should be rejected")
	}
	if _, err := Decode([]byte(`{"kind":"ci","source":"x","payload":"@@@not-base64"}`)); err == nil {
		t.Error("bad base64 payload should be rejected")
	}
}

// Encode rejects an unknown kind at the enqueue boundary so the cloudtasks backend fails fast
// instead of enqueuing work that Decode (and POST /internal/dispatch) would later silently drop.
func TestEncodeRejectsUnknownKind(t *testing.T) {
	if _, err := Encode(New(Kind("jira"), "x", nil, time.Unix(0, 0))); err == nil {
		t.Error("Encode should reject an unknown kind")
	}
}
