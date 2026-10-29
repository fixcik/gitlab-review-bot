package limits

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/store"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

var msk = time.FixedZone("MSK", 3*3600)

func setup(t *testing.T, global int, exempt ...string) (*Limiter, *clock, core.Store) {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := &clock{t: time.Date(2026, 3, 10, 14, 0, 0, 0, msk)}
	return New(st, global, exempt, c.Now), c, st
}

func must(t *testing.T) func(core.Decision, error) core.Decision {
	return func(d core.Decision, err error) core.Decision {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
}

func TestRerunHourlyLimitAndBoundary(t *testing.T) {
	ctx := context.Background()
	l, c, _ := setup(t, 0)
	lim := core.UserLimits{RerunsPerHour: 2}

	start := c.Now()
	for i := 0; i < 2; i++ {
		d := must(t)(l.CheckUser(ctx, 7, "alice", core.UsageRerun, "1!1", lim))
		if !d.Allowed {
			t.Fatalf("run %d denied: %+v", i, d)
		}
		if err := l.Record(ctx, core.Usage{UserID: 7, Kind: core.UsageRerun, MRKey: "1!1"}); err != nil {
			t.Fatal(err)
		}
		c.Advance(10 * time.Minute)
	}
	// now = 14:20, records at 14:00 and 14:10.
	d := must(t)(l.CheckUser(ctx, 7, "alice", core.UsageRerun, "1!2", lim))
	if d.Allowed {
		t.Fatal("expected denial")
	}
	if !d.RetryAt.Equal(start.Add(time.Hour)) {
		t.Fatalf("RetryAt=%v want %v", d.RetryAt, start.Add(time.Hour))
	}
	want := "Лимит перезапусков исчерпан (2 в час). Следующий будет доступен в 15:00."
	if d.Reason != want {
		t.Fatalf("Reason=%q want %q", d.Reason, want)
	}

	// Other user is unaffected.
	if d := must(t)(l.CheckUser(ctx, 8, "bob", core.UsageRerun, "1!2", lim)); !d.Allowed {
		t.Fatalf("bob denied: %+v", d)
	}

	// One nanosecond before the boundary: still denied.
	c.Advance(40*time.Minute - time.Nanosecond)
	if d := must(t)(l.CheckUser(ctx, 7, "alice", core.UsageRerun, "1!2", lim)); d.Allowed {
		t.Fatal("expected denial just before 1h")
	}
	// Exactly 1h after the oldest record: it has left the window.
	c.Advance(time.Nanosecond)
	if d := must(t)(l.CheckUser(ctx, 7, "alice", core.UsageRerun, "1!2", lim)); !d.Allowed {
		t.Fatalf("expected allowed at exactly 1h: %+v", d)
	}
}

func TestMRCooldown(t *testing.T) {
	ctx := context.Background()
	l, c, _ := setup(t, 0)
	lim := core.UserLimits{MRRerunCooldown: 10 * time.Minute}

	if err := l.Record(ctx, core.Usage{UserID: 1, Kind: core.UsageRerun, MRKey: "5!9"}); err != nil {
		t.Fatal(err)
	}
	// Auto runs on the same MR don't count towards the cooldown.
	if err := l.Record(ctx, core.Usage{Kind: core.UsageAuto, MRKey: "5!9", At: c.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	c.Advance(3 * time.Minute)

	// Different user, same MR -> denied.
	d := must(t)(l.CheckUser(ctx, 2, "bob", core.UsageRerun, "5!9", lim))
	if d.Allowed {
		t.Fatal("expected cooldown denial")
	}
	if !d.RetryAt.Equal(time.Date(2026, 3, 10, 14, 10, 0, 0, msk)) {
		t.Fatalf("RetryAt=%v", d.RetryAt)
	}
	want := "Перезапуск этого MR возможен не чаще раза в 10 мин. Следующий — в 14:10."
	if d.Reason != want {
		t.Fatalf("Reason=%q want %q", d.Reason, want)
	}
	// Other MR -> allowed.
	if d := must(t)(l.CheckUser(ctx, 2, "bob", core.UsageRerun, "5!10", lim)); !d.Allowed {
		t.Fatalf("other MR denied: %+v", d)
	}
	// Exactly at cooldown end -> allowed.
	c.Advance(7 * time.Minute)
	if d := must(t)(l.CheckUser(ctx, 2, "bob", core.UsageRerun, "5!9", lim)); !d.Allowed {
		t.Fatalf("expected allowed at cooldown end: %+v", d)
	}
}

func TestCooldownAndHourlyPickLaterRetry(t *testing.T) {
	ctx := context.Background()
	l, c, _ := setup(t, 0)
	lim := core.UserLimits{RerunsPerHour: 1, MRRerunCooldown: 10 * time.Minute}
	if err := l.Record(ctx, core.Usage{UserID: 1, Kind: core.UsageRerun, MRKey: "1!1"}); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Minute)
	d := must(t)(l.CheckUser(ctx, 1, "u", core.UsageRerun, "1!1", lim))
	if d.Allowed || !d.RetryAt.Equal(time.Date(2026, 3, 10, 15, 0, 0, 0, msk)) {
		t.Fatalf("want hourly retry 15:00, got %+v", d)
	}
}

func TestChatLimit(t *testing.T) {
	ctx := context.Background()
	l, c, _ := setup(t, 0)
	lim := core.UserLimits{ChatPerHour: 1, RerunsPerHour: 1}
	if err := l.Record(ctx, core.Usage{UserID: 1, Kind: core.UsageRerun, MRKey: "1!1"}); err != nil {
		t.Fatal(err)
	}
	// Reruns don't count against chat.
	if d := must(t)(l.CheckUser(ctx, 1, "u", core.UsageChat, "1!1", lim)); !d.Allowed {
		t.Fatalf("chat denied: %+v", d)
	}
	if err := l.Record(ctx, core.Usage{UserID: 1, Kind: core.UsageChat, MRKey: "1!1"}); err != nil {
		t.Fatal(err)
	}
	c.Advance(5 * time.Minute)
	d := must(t)(l.CheckUser(ctx, 1, "u", core.UsageChat, "1!1", lim))
	if d.Allowed || !strings.Contains(d.Reason, "15:00") {
		t.Fatalf("expected chat denial until 15:00: %+v", d)
	}
}

func TestExemptAndUnlimited(t *testing.T) {
	ctx := context.Background()
	l, _, _ := setup(t, 0, "Admin", "@Lead")
	lim := core.UserLimits{RerunsPerHour: 1, ChatPerHour: 1, MRRerunCooldown: time.Hour}
	for i := 0; i < 3; i++ {
		for _, k := range []core.UsageKind{core.UsageRerun, core.UsageChat} {
			if err := l.Record(ctx, core.Usage{UserID: 1, Kind: k, MRKey: "1!1"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, name := range []string{"admin", "ADMIN", "lead"} {
		for _, k := range []core.UsageKind{core.UsageRerun, core.UsageChat} {
			if d := must(t)(l.CheckUser(ctx, 1, name, k, "1!1", lim)); !d.Allowed {
				t.Fatalf("exempt %s denied for %s: %+v", name, k, d)
			}
		}
	}
	if d := must(t)(l.CheckUser(ctx, 1, "someone", core.UsageRerun, "1!1", lim)); d.Allowed {
		t.Fatal("non-exempt allowed")
	}
	// Unlimited (zero values).
	for _, k := range []core.UsageKind{core.UsageRerun, core.UsageChat} {
		if d := must(t)(l.CheckUser(ctx, 1, "someone", k, "1!1", core.UserLimits{})); !d.Allowed {
			t.Fatalf("unlimited denied for %s: %+v", k, d)
		}
		if d := must(t)(l.CheckUser(ctx, 1, "someone", k, "1!1", core.UserLimits{RerunsPerHour: -1, ChatPerHour: -1, MRRerunCooldown: -1})); !d.Allowed {
			t.Fatalf("negative-unlimited denied for %s: %+v", k, d)
		}
	}
	// Global unlimited.
	if d := must(t)(l.CheckGlobal(ctx)); !d.Allowed {
		t.Fatalf("global unlimited denied: %+v", d)
	}
	r, err := l.Remaining(ctx, 1, "admin", lim)
	if err != nil || r != (core.Remaining{Reruns: -1, Chat: -1, Global: -1}) {
		t.Fatalf("exempt remaining %+v %v", r, err)
	}
}

func TestGlobalAndRemaining(t *testing.T) {
	ctx := context.Background()
	l, c, _ := setup(t, 3)
	lim := core.UserLimits{RerunsPerHour: 2, ChatPerHour: 5}

	r, err := l.Remaining(ctx, 1, "u", lim)
	if err != nil || r != (core.Remaining{Reruns: 2, Chat: 5, Global: 3}) {
		t.Fatalf("initial remaining %+v %v", r, err)
	}
	recs := []core.Usage{
		{Kind: core.UsageAuto, MRKey: "1!1"},
		{UserID: 1, Kind: core.UsageRerun, MRKey: "1!1"},
		{UserID: 2, Kind: core.UsageChat, MRKey: "1!2"},
	}
	for _, u := range recs {
		if err := l.Record(ctx, u); err != nil {
			t.Fatal(err)
		}
		c.Advance(15 * time.Minute)
	}
	// now 14:45; records at 14:00, 14:15, 14:30.
	d := must(t)(l.CheckGlobal(ctx))
	if d.Allowed || !d.RetryAt.Equal(time.Date(2026, 3, 10, 15, 0, 0, 0, msk)) {
		t.Fatalf("global: %+v", d)
	}
	if !strings.HasPrefix(d.Reason, "Глобальный лимит ревью исчерпан (3 в час).") || !strings.Contains(d.Reason, "15:00") {
		t.Fatalf("global reason %q", d.Reason)
	}
	r, _ = l.Remaining(ctx, 1, "u", lim)
	if r != (core.Remaining{Reruns: 1, Chat: 5, Global: 0}) {
		t.Fatalf("remaining %+v", r)
	}
	c.Advance(15 * time.Minute) // 15:00: first record leaves window.
	if d := must(t)(l.CheckGlobal(ctx)); !d.Allowed {
		t.Fatalf("global after window: %+v", d)
	}
}

func TestRetryAtUsesLocationOfNow(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 3, 10, 11, 0, 0, 0, time.UTC) // 14:00 MSK
	l := New(st, 1, nil, func() time.Time { return now })
	if err := l.Record(ctx, core.Usage{Kind: core.UsageAuto}); err != nil {
		t.Fatal(err)
	}
	d := must(t)(l.CheckGlobal(ctx))
	if !strings.Contains(d.Reason, "12:00") {
		t.Fatalf("reason should use UTC time: %q", d.Reason)
	}
}

func TestRecordPrunesOldRecords(t *testing.T) {
	ctx := context.Background()
	l, c, st := setup(t, 0)
	old := c.Now().Add(-25 * time.Hour)
	if err := st.RecordUsage(ctx, core.Usage{Kind: core.UsageAuto, At: old}); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(ctx, core.Usage{Kind: core.UsageAuto}); err != nil {
		t.Fatal(err)
	}
	all, _ := st.ListUsage(ctx, core.UsageFilter{})
	if len(all) != 1 {
		t.Fatalf("expected old record pruned, have %+v", all)
	}
	// Within the hour, no second prune.
	if err := st.RecordUsage(ctx, core.Usage{Kind: core.UsageAuto, At: old}); err != nil {
		t.Fatal(err)
	}
	c.Advance(30 * time.Minute)
	_ = l.Record(ctx, core.Usage{Kind: core.UsageAuto})
	if all, _ := st.ListUsage(ctx, core.UsageFilter{}); len(all) != 3 {
		t.Fatalf("unexpected prune within interval: %d", len(all))
	}
	c.Advance(31 * time.Minute)
	_ = l.Record(ctx, core.Usage{Kind: core.UsageAuto})
	if all, _ := st.ListUsage(ctx, core.UsageFilter{}); len(all) != 3 {
		t.Fatalf("expected prune after interval: %d", len(all))
	}
}

func TestFmtDuration(t *testing.T) {
	cases := map[time.Duration]string{
		10 * time.Minute: "10 мин",
		time.Hour:        "1 ч",
		90 * time.Minute: "1 ч 30 мин",
		30 * time.Second: "30 с",
	}
	for d, want := range cases {
		if got := fmtDuration(d); got != want {
			t.Errorf("fmtDuration(%v)=%q want %q", d, got, want)
		}
	}
}
