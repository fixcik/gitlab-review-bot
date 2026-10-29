package limits

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func rerunReq(user int64, mr string, lim core.UserLimits) core.ReserveRequest {
	return core.ReserveRequest{Usage: core.Usage{UserID: user, Kind: core.UsageRerun, MRKey: mr}, Username: "dev", User: &lim, Global: true}
}

// Two jobs of the same user checked at the same time: only one gets the
// last rerun slot (the check and the booking are one step).
func TestReserveIsAtomicUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	l, _, st := setup(t, 0)
	lim := core.UserLimits{RerunsPerHour: 1}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var allowed []core.Reservation
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dec, res, err := l.Reserve(ctx, rerunReq(7, "1!"+string(rune('a'+i)), lim))
			if err != nil {
				t.Error(err)
				return
			}
			if dec.Allowed != (res != nil) {
				t.Errorf("allowed %v with reservation %v", dec.Allowed, res)
			}
			if dec.Allowed {
				mu.Lock()
				allowed = append(allowed, res)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if len(allowed) != 1 {
		t.Fatalf("%d jobs got the single slot", len(allowed))
	}
	// Nothing is stored until the run starts.
	if recs, _ := st.ListUsage(ctx, core.UsageFilter{}); len(recs) != 0 {
		t.Fatalf("stored before commit: %v", recs)
	}
	ok, err := allowed[0].Commit(ctx)
	if err != nil || !ok {
		t.Fatalf("commit: %v %v", ok, err)
	}
	if recs, _ := st.ListUsage(ctx, core.UsageFilter{}); len(recs) != 1 || recs[0].UserID != 7 || recs[0].Kind != core.UsageRerun {
		t.Fatalf("after commit: %+v", recs)
	}
	// Committed: the slot stays taken; a second commit records nothing.
	if dec := must(t)(l.CheckUser(ctx, 7, "dev", core.UsageRerun, "", lim)); dec.Allowed {
		t.Error("slot free after commit")
	}
	if ok, _ := allowed[0].Commit(ctx); ok {
		t.Error("second commit recorded again")
	}
	allowed[0].Release() // no-op after commit
	if recs, _ := st.ListUsage(ctx, core.UsageFilter{}); len(recs) != 1 {
		t.Fatalf("release after commit changed records: %v", recs)
	}
}

// A job that ends without a Claude run gives its slot back.
func TestReserveReleaseFreesSlot(t *testing.T) {
	ctx := context.Background()
	l, _, st := setup(t, 1)
	lim := core.UserLimits{RerunsPerHour: 5}
	dec, res, err := l.Reserve(ctx, rerunReq(1, "1!1", lim))
	if err != nil || !dec.Allowed {
		t.Fatalf("%+v %v", dec, err)
	}
	// The booking counts toward the global limit (1/h) and Remaining.
	if d := must(t)(l.CheckGlobal(ctx)); d.Allowed {
		t.Error("global slot free while booked")
	}
	if dec, res2, _ := l.Reserve(ctx, core.ReserveRequest{Usage: core.Usage{Kind: core.UsageAuto, MRKey: "2!1"}, Global: true}); dec.Allowed || res2 != nil {
		t.Error("second booking over the global limit")
	}
	if rem, _ := l.Remaining(ctx, 1, "dev", lim); rem.Global != 0 || rem.Reruns != 4 {
		t.Errorf("remaining while booked: %+v", rem)
	}
	res.Release()
	res.Release()
	if ok, _ := res.Commit(ctx); ok {
		t.Error("commit after release recorded")
	}
	if d := must(t)(l.CheckGlobal(ctx)); !d.Allowed {
		t.Error("slot not freed by release")
	}
	if recs, _ := st.ListUsage(ctx, core.UsageFilter{}); len(recs) != 0 {
		t.Fatalf("release stored a record: %v", recs)
	}
}

// The per-MR cooldown sees a booked run of the same MR.
func TestReserveCooldownCountsBooking(t *testing.T) {
	ctx := context.Background()
	l, c, _ := setup(t, 0)
	lim := core.UserLimits{MRRerunCooldown: 10 * time.Minute}
	_, res, err := l.Reserve(ctx, rerunReq(1, "5!9", lim))
	if err != nil || res == nil {
		t.Fatal(err)
	}
	if dec, _, _ := l.Reserve(ctx, rerunReq(2, "5!9", lim)); dec.Allowed {
		t.Error("cooldown ignored a booked run of the same MR")
	}
	if dec, r2, _ := l.Reserve(ctx, rerunReq(2, "5!10", lim)); !dec.Allowed || r2 == nil {
		t.Error("another MR blocked by the cooldown")
	}
	// Committed later: the cooldown starts at the commit time.
	c.Advance(3 * time.Minute)
	if ok, err := res.Commit(ctx); !ok || err != nil {
		t.Fatal(ok, err)
	}
	c.Advance(9 * time.Minute)
	if dec := must(t)(l.CheckUser(ctx, 2, "dev", core.UsageRerun, "5!9", lim)); dec.Allowed {
		t.Error("cooldown measured from the booking, not the commit")
	}
}

// Exempt users skip the per-user limits but still book a run for the
// global limit; a denied request books nothing.
func TestReserveExemptAndDenied(t *testing.T) {
	ctx := context.Background()
	l, _, _ := setup(t, 2, "lead")
	lim := core.UserLimits{RerunsPerHour: 1}
	for i := 0; i < 2; i++ {
		req := rerunReq(3, "", lim)
		req.Username = "lead"
		if dec, res, err := l.Reserve(ctx, req); err != nil || !dec.Allowed || res == nil {
			t.Fatalf("exempt booking %d: %+v %v", i, dec, err)
		}
	}
	req := rerunReq(3, "", lim)
	req.Username = "lead"
	dec, res, _ := l.Reserve(ctx, req)
	if dec.Allowed || res != nil || dec.Reason == "" || dec.RetryAt.IsZero() {
		t.Errorf("exempt user over the global limit: %+v", dec)
	}
	l.pendMu.Lock()
	n := len(l.pending)
	l.pendMu.Unlock()
	if n != 2 {
		t.Errorf("denied request booked: %d pending", n)
	}
}

func learnReq(user int64, perHour int) core.ReserveRequest {
	return core.ReserveRequest{Usage: core.Usage{UserID: user, Kind: core.UsageLearn, MRKey: "1!1"}, Global: true, KindPerHour: perHour}
}

// The per-kind cap (review.learn.max_per_hour) is checked and booked in one
// step: learning jobs started at the same time cannot all pass a cap that
// has room for one of them, and a booked run counts until it is committed
// or released.
func TestReserveKindCapUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	l, _, st := setup(t, 0)
	// One learn run and runs of other kinds already in the window.
	for _, u := range []core.Usage{{Kind: core.UsageLearn}, {Kind: core.UsageRerun, UserID: 1}, {Kind: core.UsageAuto}} {
		if err := l.Record(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var allowed []core.Reservation
	var reasons []string
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dec, res, err := l.Reserve(ctx, learnReq(int64(i+1), 2))
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if dec.Allowed {
				allowed = append(allowed, res)
			} else {
				reasons = append(reasons, dec.Reason)
			}
		}(i)
	}
	wg.Wait()
	if len(allowed) != 1 {
		t.Fatalf("%d learning jobs passed a cap with room for one", len(allowed))
	}
	for _, r := range reasons {
		if r != "Лимит обучения исчерпан (2 в час)." {
			t.Fatalf("reason %q", r)
		}
	}
	if ok, err := allowed[0].Commit(ctx); !ok || err != nil {
		t.Fatalf("commit %v %v", ok, err)
	}
	k := core.UsageLearn
	if recs, _ := st.ListUsage(ctx, core.UsageFilter{Kind: &k}); len(recs) != 2 {
		t.Fatalf("learn records %+v", recs)
	}
	if dec, _, _ := l.Reserve(ctx, learnReq(9, 2)); dec.Allowed {
		t.Fatal("cap reached after the commit")
	}
	// A higher cap, or none, lets it through; other kinds ignore the
	// learn records.
	if dec, res, _ := l.Reserve(ctx, learnReq(9, 3)); !dec.Allowed {
		t.Fatal("below the cap denied")
	} else {
		res.Release()
	}
	if dec, res, _ := l.Reserve(ctx, learnReq(9, 0)); !dec.Allowed {
		t.Fatal("no cap denied")
	} else {
		res.Release()
	}
	rr := core.ReserveRequest{Usage: core.Usage{Kind: core.UsageAuto}, KindPerHour: 2}
	if dec, res, _ := l.Reserve(ctx, rr); !dec.Allowed {
		t.Fatal("auto runs counted the learn runs")
	} else {
		res.Release()
	}
}

// The cap is a sliding hour like the other limits; the global limit is
// checked first and its reason wins.
func TestReserveKindCapWindowAndGlobal(t *testing.T) {
	ctx := context.Background()
	l, c, _ := setup(t, 2)
	if err := l.Record(ctx, core.Usage{Kind: core.UsageLearn}); err != nil {
		t.Fatal(err)
	}
	dec, _, err := l.Reserve(ctx, learnReq(0, 1))
	if err != nil || dec.Allowed || dec.RetryAt.IsZero() {
		t.Fatalf("cap: %+v %v", dec, err)
	}
	c.Advance(time.Hour) // the record leaves the window
	dec, res, err := l.Reserve(ctx, learnReq(0, 1))
	if err != nil || !dec.Allowed {
		t.Fatalf("after the hour: %+v %v", dec, err)
	}
	if _, err := res.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(ctx, core.Usage{Kind: core.UsageAuto}); err != nil {
		t.Fatal(err)
	}
	dec, _, _ = l.Reserve(ctx, learnReq(0, 1))
	if dec.Allowed || dec.Reason == "Лимит обучения исчерпан (1 в час)." {
		t.Fatalf("global limit must deny first: %+v", dec)
	}
}
