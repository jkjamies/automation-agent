package reviewer

import "testing"

func TestOversize(t *testing.T) {
	cases := []struct {
		name                             string
		files, bytes, maxFiles, maxBytes int
		wantDenied                       bool
	}{
		{"within both caps", 10, 1000, 50, 5000, false},
		{"at the file cap", 50, 1000, 50, 5000, false},
		{"over the file cap", 51, 1000, 50, 5000, true},
		{"at the byte cap", 10, 5000, 50, 5000, false},
		{"over the byte cap", 10, 5001, 50, 5000, true},
		{"file cap disabled (0)", 999, 1000, 0, 5000, false},
		{"byte cap disabled (0)", 10, 999999, 50, 0, false},
		{"both disabled", 999, 999999, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, denied := oversize(tc.files, tc.bytes, tc.maxFiles, tc.maxBytes)
			if denied != tc.wantDenied {
				t.Fatalf("oversize(%d,%d,%d,%d) denied=%v, want %v", tc.files, tc.bytes, tc.maxFiles, tc.maxBytes, denied, tc.wantDenied)
			}
			if denied && reason == "" {
				t.Error("a denial must carry a reason")
			}
			if !denied && reason != "" {
				t.Errorf("a non-denial must have an empty reason, got %q", reason)
			}
		})
	}
}
