package limits

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// usageStore is an in-memory usage store with injectable failures; it
// counts the calls so a test can tell whether the limiter queried it.
type usageStore struct {
	core.Store
	mu                     sync.Mutex
	recs                   []core.Usage
	recordErr, pruneErr    error
	listErr                error
	lists, records, prunes int
	prunedBefore           time.Time
}

func (s *usageStore) RecordUsage(_ context.Context, u core.Usage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records++
	if s.recordErr != nil {
		return s.recordErr
	}
	s.recs = append(s.recs, u)
	return nil
}

func (s *usageStore) ListUsage(_ context.Context, f core.UsageFilter) ([]core.Usage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists++
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []core.Usage
	for _, u := range s.recs {
		if (f.UserID == nil || u.UserID == *f.UserID) && (f.Kind == nil || u.Kind == *f.Kind) &&
			(f.MRKey == nil || u.MRKey == *f.MRKey) && !u.At.Before(f.Since) {
			out = append(out, u)
		}
	}
	return out, nil
}

func (s *usageStore) PruneUsage(_ context.Context, before time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prunes++
	s.prunedBefore = before
	return s.pruneErr
}

// Without a clock the limiter stamps records with the wall clock.
func TestCovNewDefaultsToWallClock(t *testing.T) {
	st := &usageStore{}
	l := New(st, 0, nil, nil)
	before := time.Now()
	if err := l.Record(context.Background(), core.Usage{UserID: 1, Kind: core.UsageRerun}); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	if len(st.recs) != 1 || st.recs[0].At.Before(before) || st.recs[0].At.After(after) {
		t.Fatalf("record not stamped with the wall clock: %+v (between %v and %v)", st.recs, before, after)
	}
	if st.prunes != 1 || !st.prunedBefore.Equal(st.recs[0].At.Add(-retention)) {
		t.Fatalf("first record must prune at now-%v: prunes %d before %v", retention, st.prunes, st.prunedBefore)
	}
}

// A store failure while reading the per-kind cap's window fails the
// booking (no reservation, no "allowed"); a cap of 0 does not query the
// store at all.
func TestCovKindCapStoreError(t *testing.T) {
	ctx := context.Background()
	st := &usageStore{listErr: errors.New("db down")}
	l := New(st, 0, nil, func() time.Time { return time.Date(2026, 3, 10, 14, 0, 0, 0, msk) })
	dec, res, err := l.Reserve(ctx, core.ReserveRequest{Usage: core.Usage{UserID: 1, Kind: core.UsageLearn}, KindPerHour: 1})
	if err == nil || !strings.Contains(err.Error(), "db down") || dec.Allowed || res != nil {
		t.Fatalf("kind cap with a failing store: %+v %v %v", dec, res, err)
	}
	dec, res, err = l.Reserve(ctx, core.ReserveRequest{Usage: core.Usage{UserID: 1, Kind: core.UsageLearn}, KindPerHour: 0})
	if err != nil || !dec.Allowed || res == nil {
		t.Fatalf("no cap: %+v %v %v", dec, res, err)
	}
	if st.lists != 1 {
		t.Fatalf("store queried %d times, want 1 (a cap of 0 is not checked)", st.lists)
	}
	res.Release()
}

// Kinds without per-user limits (automatic reviews, learning) pass
// CheckUser whatever the limits and the user's history, without reading the
// store; rerun and chat with the same history are denied.
func TestCovCheckUserOtherKindsUnlimited(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 3, 10, 14, 0, 0, 0, msk)
	st := &usageStore{}
	for _, k := range []core.UsageKind{core.UsageRerun, core.UsageChat, core.UsageAuto, core.UsageLearn} {
		st.recs = append(st.recs, core.Usage{UserID: 1, Kind: k, MRKey: "1!1", At: now.Add(-time.Minute)})
	}
	l := New(st, 0, nil, func() time.Time { return now })
	lim := core.UserLimits{RerunsPerHour: 1, ChatPerHour: 1, MRRerunCooldown: time.Hour}
	for _, k := range []core.UsageKind{core.UsageAuto, core.UsageLearn} {
		d, err := l.CheckUser(ctx, 1, "dev", k, "1!1", lim)
		if err != nil || !d.Allowed {
			t.Errorf("%s: %+v %v", k, d, err)
		}
	}
	if st.lists != 0 {
		t.Fatalf("store queried %d times for kinds without per-user limits", st.lists)
	}
	for _, k := range []core.UsageKind{core.UsageRerun, core.UsageChat} {
		if d, err := l.CheckUser(ctx, 1, "dev", k, "1!1", lim); err != nil || d.Allowed {
			t.Errorf("%s with the limit used up: %+v %v", k, d, err)
		}
	}
}

// Record: a failed write is returned and does not count as the hourly
// prune (the next successful Record prunes); a failed prune is returned
// after the record was stored.
func TestCovRecordErrors(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: time.Date(2026, 3, 10, 14, 0, 0, 0, msk)}
	st := &usageStore{recordErr: errors.New("disk full")}
	l := New(st, 0, nil, c.Now)
	err := l.Record(ctx, core.Usage{UserID: 1, Kind: core.UsageRerun})
	if err == nil || !strings.Contains(err.Error(), "limits: disk full") {
		t.Fatalf("record error: %v", err)
	}
	if st.prunes != 0 {
		t.Fatal("pruned after a failed record")
	}

	st.recordErr, st.pruneErr = nil, errors.New("locked")
	err = l.Record(ctx, core.Usage{UserID: 1, Kind: core.UsageRerun})
	if err == nil || !strings.Contains(err.Error(), "limits: prune: locked") {
		t.Fatalf("prune error: %v", err)
	}
	if len(st.recs) != 1 || st.prunes != 1 {
		t.Fatalf("record must be stored before the prune fails: recs %d prunes %d", len(st.recs), st.prunes)
	}
	// The failed prune still counts as this hour's prune.
	st.pruneErr = nil
	c.Advance(pruneInterval - time.Second)
	if err := l.Record(ctx, core.Usage{UserID: 1, Kind: core.UsageRerun}); err != nil || st.prunes != 1 {
		t.Fatalf("pruned again within the interval: %v prunes %d", err, st.prunes)
	}
	c.Advance(time.Second)
	if err := l.Record(ctx, core.Usage{UserID: 1, Kind: core.UsageRerun}); err != nil || st.prunes != 2 {
		t.Fatalf("no prune after the interval: %v prunes %d", err, st.prunes)
	}
}

// Commit is done once: a second Commit records nothing. A Commit whose
// record fails reports the error, and the booking is gone (a retry does
// not record it twice, and the slot is free again).
func TestCovCommitOnce(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: time.Date(2026, 3, 10, 14, 0, 0, 0, msk)}
	st := &usageStore{}
	l := New(st, 1, nil, c.Now)
	req := core.ReserveRequest{Usage: core.Usage{UserID: 1, Kind: core.UsageRerun, MRKey: "1!1"}, Global: true}

	_, res, err := l.Reserve(ctx, req)
	if err != nil || res == nil {
		t.Fatal(err)
	}
	c.Advance(time.Minute)
	if ok, err := res.Commit(ctx); !ok || err != nil {
		t.Fatalf("commit: %v %v", ok, err)
	}
	if ok, err := res.Commit(ctx); ok || err != nil {
		t.Fatalf("second commit: %v %v", ok, err)
	}
	res.Release() // no-op after commit
	if len(st.recs) != 1 || !st.recs[0].At.Equal(c.Now()) {
		t.Fatalf("want one record stamped at commit time: %+v", st.recs)
	}
	if d, _ := l.CheckGlobal(ctx); d.Allowed {
		t.Fatal("committed run not counted after a Release on the committed reservation")
	}

	c.Advance(time.Hour) // window free again
	st.recordErr = errors.New("disk full")
	_, res, err = l.Reserve(ctx, req)
	if err != nil || res == nil {
		t.Fatal(err)
	}
	if ok, err := res.Commit(ctx); !ok || err == nil {
		t.Fatalf("commit with a failing store: %v %v", ok, err)
	}
	if ok, err := res.Commit(ctx); ok || err != nil {
		t.Fatalf("commit after a failed commit: %v %v", ok, err)
	}
	if st.records != 2 {
		t.Fatalf("record attempts %d, want 2 (the second Commit must not retry)", st.records)
	}
	if d, _ := l.CheckGlobal(ctx); !d.Allowed {
		t.Fatal("booking kept after its commit failed")
	}
}

// Record keeps an explicit time (only a zero one is stamped with now).
func TestCovRecordKeepsExplicitTime(t *testing.T) {
	now := time.Date(2026, 3, 10, 14, 0, 0, 0, msk)
	st := &usageStore{}
	l := New(st, 0, nil, func() time.Time { return now })
	at := now.Add(-10 * time.Minute)
	if err := l.Record(context.Background(), core.Usage{UserID: 1, Kind: core.UsageChat, At: at}); err != nil {
		t.Fatal(err)
	}
	if len(st.recs) != 1 || !st.recs[0].At.Equal(at) {
		t.Fatalf("explicit time replaced: %+v", st.recs)
	}
}
