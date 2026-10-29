package diag

import (
	"strings"
	"testing"
)

func TestScrubberLiteralsAndOpaque(t *testing.T) {
	// Values too short to be secrets are not literals (they would mangle
	// ordinary text); duplicates are harmless.
	s := newScrubber([]string{"short", "abcdefgh12", "abcdefgh12", "  "})
	if got := s.str("short text abcdefgh12", nil); got != "short text "+Redacted {
		t.Fatalf("literals: %q", got)
	}

	// A random base62 key is redacted; long identifiers and paths without
	// the digit/case mix of a key are kept, and so are known-safe values.
	key := "aZ3kP9qL2mX7vB4nR8tY1wC6hJ5gF0dS2eU9iO3pA7"
	for in, want := range map[string]string{
		"key " + key: "key " + Redacted,
		"ThisIsAVeryLongIdentifierNameWithoutDigitsAtAllReally": "ThisIsAVeryLongIdentifierNameWithoutDigitsAtAllReally",
		"handle_review_postpones_when_rate_limited_twice_v2":    "handle_review_postpones_when_rate_limited_twice_v2",
		// A real commit SHA (lower-case hex, dense digit/letter mix).
		"commit e83c5163316f89bfbde7d9ab23ca2e25604af290": "commit e83c5163316f89bfbde7d9ab23ca2e25604af290",
	} {
		if got := s.str(in, nil); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	if got := s.str("sha "+key, []string{key}); !strings.Contains(got, key) {
		t.Errorf("safe value redacted: %q", got)
	}
}

func TestScrubFunc(t *testing.T) {
	f := ScrubFunc([]string{"super-secret-value"})
	got := f("token=abcdefgh123 at https://u:p@host/x key super-secret-value sha 0123456789abcdef0123456789abcdef01234567")
	for _, bad := range []string{"abcdefgh123", "u:p@", "super-secret-value"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q left in %q", bad, got)
		}
	}
	if !strings.Contains(got, "0123456789abcdef0123456789abcdef01234567") {
		t.Errorf("commit sha scrubbed: %q", got)
	}
}
