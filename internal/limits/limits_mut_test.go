package limits

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// A job without a username (e.g. an event whose author could not be read)
// is not exempt: limits still apply to it.
func TestMutEmptyUsernameIsNotExempt(t *testing.T) {
	ctx := context.Background()
	l, _, _ := setup(t, 0, "admin")
	lim := core.UserLimits{RerunsPerHour: 1, ChatPerHour: 1}
	for _, k := range []core.UsageKind{core.UsageRerun, core.UsageChat} {
		if err := l.Record(ctx, core.Usage{UserID: 1, Kind: k, MRKey: "1!1"}); err != nil {
			t.Fatal(err)
		}
		if d := must(t)(l.CheckUser(ctx, 1, "", k, "1!1", lim)); d.Allowed {
			t.Fatalf("%s: empty username bypassed the limit", k)
		}
	}
	r, err := l.Remaining(ctx, 1, "  ", lim)
	if err != nil || r.Reruns != 0 || r.Chat != 0 {
		t.Fatalf("remaining for blank username %+v %v", r, err)
	}
}

// When both the hourly limit and a longer MR cooldown deny, the later of the
// two times is reported.
func TestMutCooldownLaterThanHourlyWins(t *testing.T) {
	ctx := context.Background()
	l, c, _ := setup(t, 0)
	lim := core.UserLimits{RerunsPerHour: 1, MRRerunCooldown: 90 * time.Minute}
	if err := l.Record(ctx, core.Usage{UserID: 1, Kind: core.UsageRerun, MRKey: "1!1"}); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Minute)
	d := must(t)(l.CheckUser(ctx, 1, "u", core.UsageRerun, "1!1", lim))
	if d.Allowed || !d.RetryAt.Equal(time.Date(2026, 3, 10, 15, 30, 0, 0, msk)) {
		t.Fatalf("want cooldown retry 15:30, got %+v", d)
	}
}

// Remaining: a limit of 1 is a limit (not unlimited), 0 is unlimited (-1),
// and usage above the limit reports 0, never a negative number.
func TestMutRemainingEdges(t *testing.T) {
	ctx := context.Background()
	l, _, _ := setup(t, 1)
	lim := core.UserLimits{RerunsPerHour: 1, ChatPerHour: 0}
	r, err := l.Remaining(ctx, 1, "u", lim)
	if err != nil || r != (core.Remaining{Reruns: 1, Chat: -1, Global: 1}) {
		t.Fatalf("initial %+v %v", r, err)
	}
	for i := 0; i < 3; i++ {
		if err := l.Record(ctx, core.Usage{UserID: 1, Kind: core.UsageRerun, MRKey: "1!1"}); err != nil {
			t.Fatal(err)
		}
	}
	r, err = l.Remaining(ctx, 1, "u", lim)
	if err != nil || r != (core.Remaining{Reruns: 0, Chat: -1, Global: 0}) {
		t.Fatalf("over the limit %+v %v", r, err)
	}
}

// errStore fails every usage query.
type errStore struct{ core.Store }

func (errStore) ListUsage(context.Context, core.UsageFilter) ([]core.Usage, error) {
	return nil, errors.New("db down")
}

// A store error is returned, never turned into "allowed" (fail closed for
// the caller, which reports the error instead of running Claude).
func TestMutStoreErrorsPropagate(t *testing.T) {
	ctx := context.Background()
	l := New(errStore{}, 5, nil, func() time.Time { return time.Date(2026, 3, 10, 14, 0, 0, 0, msk) })
	checks := map[string]core.UserLimits{
		"rerun hourly": {RerunsPerHour: 1},
		"cooldown":     {MRRerunCooldown: time.Minute},
	}
	for name, lim := range checks {
		if _, err := l.CheckUser(ctx, 1, "u", core.UsageRerun, "1!1", lim); err == nil {
			t.Errorf("%s: error swallowed", name)
		}
	}
	if _, err := l.CheckUser(ctx, 1, "u", core.UsageChat, "1!1", core.UserLimits{ChatPerHour: 1}); err == nil {
		t.Error("chat: error swallowed")
	}
	if _, err := l.CheckGlobal(ctx); err == nil {
		t.Error("global: error swallowed")
	}
	for name, lim := range map[string]core.UserLimits{"reruns": {RerunsPerHour: 1}, "chat": {ChatPerHour: 1}, "global": {}} {
		if _, err := l.Remaining(ctx, 1, "u", lim); err == nil {
			t.Errorf("remaining %s: error swallowed", name)
		}
	}
}
