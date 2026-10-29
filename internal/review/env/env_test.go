package env

import (
	"testing"
	"time"
)

// A postponed job runs again at the reset time Claude reported, or after
// DefaultBackoff when there is none (or it passed); a reset time further
// away than MaxRetryWait (a bogus one, e.g. a year ahead) is clamped so the
// job is not postponed indefinitely.
func TestRetryAt(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	e := &Env{Now: func() time.Time { return now }}
	for _, c := range []struct {
		name   string
		at     time.Time
		expect time.Time
	}{
		{"unknown", time.Time{}, now.Add(DefaultBackoff)},
		{"past", now.Add(-time.Minute), now.Add(DefaultBackoff)},
		{"now", now, now.Add(DefaultBackoff)},
		{"reset", now.Add(2 * time.Hour), now.Add(2 * time.Hour)},
		{"at the cap", now.Add(MaxRetryWait), now.Add(MaxRetryWait)},
		{"beyond the cap", now.Add(MaxRetryWait + time.Second), now.Add(MaxRetryWait)},
		{"year 2286", time.Unix(9999999999, 0), now.Add(MaxRetryWait)},
	} {
		if got := e.RetryAt(c.at); !got.Equal(c.expect) {
			t.Errorf("%s: RetryAt(%v) = %v, want %v", c.name, c.at, got, c.expect)
		}
	}
}
