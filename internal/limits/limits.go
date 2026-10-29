// Package limits implements core.Limiter with sliding one-hour windows over
// usage records kept in a core.Store.
package limits

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

const (
	window        = time.Hour
	retention     = 24 * time.Hour // >= config.MaxRerunCooldown
	pruneInterval = time.Hour
)

// Limiter is a core.Limiter backed by a core.Store.
type Limiter struct {
	st            core.Store
	globalPerHour int
	exempt        map[string]struct{}
	now           func() time.Time

	mu        sync.Mutex
	lastPrune time.Time

	// reserveMu serializes Reserve (check + book); pendMu guards pending:
	// booked runs not yet committed, counted by every check.
	reserveMu sync.Mutex
	pendMu    sync.Mutex
	pending   map[uint64]core.Usage
	seq       uint64
}

var _ core.Limiter = (*Limiter)(nil)

// New creates a Limiter. globalPerHour <= 0 disables the global limit.
// exemptUsernames are compared case-insensitively (a leading "@" is ignored).
// now defaults to time.Now.
func New(st core.Store, globalPerHour int, exemptUsernames []string, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	ex := make(map[string]struct{}, len(exemptUsernames))
	for _, u := range exemptUsernames {
		if n := normUser(u); n != "" {
			ex[n] = struct{}{}
		}
	}
	return &Limiter{st: st, globalPerHour: globalPerHour, exempt: ex, now: now}
}

func normUser(u string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(u), "@"))
}

func (l *Limiter) isExempt(username string) bool {
	n := normUser(username)
	if n == "" {
		return false
	}
	_, ok := l.exempt[n]
	return ok
}

func allow() core.Decision { return core.Decision{Allowed: true} }

// hhmm formats t as HH:MM in the location of now.
func hhmm(t, now time.Time) string { return t.In(now.Location()).Format("15:04") }

// listUsage is the store's ListUsage plus booked (uncommitted) runs that
// match f, oldest first.
func (l *Limiter) listUsage(ctx context.Context, f core.UsageFilter) ([]core.Usage, error) {
	recs, err := l.st.ListUsage(ctx, f)
	if err != nil {
		return nil, err
	}
	l.pendMu.Lock()
	var extra []core.Usage
	for _, u := range l.pending {
		if (f.UserID == nil || u.UserID == *f.UserID) && (f.Kind == nil || u.Kind == *f.Kind) &&
			(f.MRKey == nil || u.MRKey == *f.MRKey) && !u.At.Before(f.Since) {
			extra = append(extra, u)
		}
	}
	l.pendMu.Unlock()
	if len(extra) == 0 {
		return recs, nil
	}
	out := append(append([]core.Usage(nil), recs...), extra...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// list returns usage records in the window (now-1h, now]. A record exactly
// one hour old has left the window.
func (l *Limiter) list(ctx context.Context, now time.Time, f core.UsageFilter) ([]core.Usage, error) {
	since := now.Add(-window)
	f.Since = since
	recs, err := l.listUsage(ctx, f)
	if err != nil {
		return nil, err
	}
	// Store filter is inclusive (>= since); drop records exactly at the edge.
	out := recs[:0]
	for _, r := range recs {
		if r.At.After(since) {
			out = append(out, r)
		}
	}
	return out, nil
}

// retryAt returns when the window frees up enough to fit one more record:
// records must drop to limit-1, so the (len-limit)-th oldest has to leave.
func retryAt(recs []core.Usage, limit int) time.Time {
	return recs[max(len(recs)-limit, 0)].At.Add(window)
}

// Reasons of a denied run: the limit (%d) and when the next run fits (%s).
const (
	reasonReruns = "Лимит перезапусков исчерпан (%d в час). Следующий будет доступен в %s."
	reasonChat   = "Лимит вопросов исчерпан (%d в час). Следующий будет доступен в %s."
	reasonGlobal = "Глобальный лимит ревью исчерпан (%d в час). Следующее будет доступно в %s."
)

// Reasons of a denied per-kind cap (ReserveRequest.KindPerHour): the cap.
var reasonKind = map[core.UsageKind]string{
	core.UsageLearn: "Лимит обучения исчерпан (%d в час).",
}

const reasonKindOther = "Лимит запусков «%s» исчерпан (%d в час)."

// checkKind applies a per-kind hourly cap (ReserveRequest.KindPerHour).
func (l *Limiter) checkKind(ctx context.Context, kind core.UsageKind, limit int) (core.Decision, error) {
	if limit <= 0 {
		return allow(), nil
	}
	now := l.now()
	recs, err := l.list(ctx, now, core.UsageFilter{Kind: &kind})
	if err != nil {
		return core.Decision{}, fmt.Errorf("limits: %w", err)
	}
	if len(recs) < limit {
		return allow(), nil
	}
	reason := fmt.Sprintf(reasonKindOther, kind, limit)
	if f, ok := reasonKind[kind]; ok {
		reason = fmt.Sprintf(f, limit)
	}
	return core.Decision{RetryAt: retryAt(recs, limit), Reason: reason}, nil
}

// checkHourly denies a run when the window already holds limit records
// matching f; reason is one of the reason* formats.
func (l *Limiter) checkHourly(ctx context.Context, now time.Time, f core.UsageFilter, limit int, reason string) (core.Decision, error) {
	recs, err := l.list(ctx, now, f)
	if err != nil {
		return core.Decision{}, fmt.Errorf("limits: %w", err)
	}
	if len(recs) < limit {
		return allow(), nil
	}
	at := retryAt(recs, limit)
	return core.Decision{RetryAt: at, Reason: fmt.Sprintf(reason, limit, hhmm(at, now))}, nil
}

// CheckUser applies the per-user limits for kind (UsageRerun or UsageChat).
func (l *Limiter) CheckUser(ctx context.Context, userID int64, username string, kind core.UsageKind, mrKey string, lim core.UserLimits) (core.Decision, error) {
	if l.isExempt(username) {
		return allow(), nil
	}
	now := l.now()
	switch kind {
	case core.UsageRerun:
		return l.checkRerun(ctx, now, userID, mrKey, lim)
	case core.UsageChat:
		if lim.ChatPerHour <= 0 {
			return allow(), nil
		}
		k := core.UsageChat
		return l.checkHourly(ctx, now, core.UsageFilter{UserID: &userID, Kind: &k}, lim.ChatPerHour, reasonChat)
	default:
		// No per-user limits for other kinds (e.g. auto reviews).
		return allow(), nil
	}
}

// checkRerun applies the hourly rerun limit and the MR's rerun cooldown;
// when both deny, the later retry time wins.
func (l *Limiter) checkRerun(ctx context.Context, now time.Time, userID int64, mrKey string, lim core.UserLimits) (core.Decision, error) {
	k := core.UsageRerun
	dec := allow()
	if lim.RerunsPerHour > 0 {
		var err error
		dec, err = l.checkHourly(ctx, now, core.UsageFilter{UserID: &userID, Kind: &k}, lim.RerunsPerHour, reasonReruns)
		if err != nil {
			return core.Decision{}, err
		}
	}
	if lim.MRRerunCooldown <= 0 || mrKey == "" {
		return dec, nil
	}
	recs, err := l.listUsage(ctx, core.UsageFilter{MRKey: &mrKey, Kind: &k, Since: now.Add(-lim.MRRerunCooldown)})
	if err != nil {
		return core.Decision{}, fmt.Errorf("limits: %w", err)
	}
	if len(recs) == 0 {
		return dec, nil
	}
	end := recs[len(recs)-1].At.Add(lim.MRRerunCooldown)
	if end.After(now) && (dec.Allowed || end.After(dec.RetryAt)) {
		dec = core.Decision{
			RetryAt: end,
			Reason: fmt.Sprintf("Перезапуск этого MR возможен не чаще раза в %s. Следующий — в %s.",
				fmtDuration(lim.MRRerunCooldown), hhmm(end, now)),
		}
	}
	return dec, nil
}

// CheckGlobal applies the global hourly limit on all Claude runs.
func (l *Limiter) CheckGlobal(ctx context.Context) (core.Decision, error) {
	if l.globalPerHour <= 0 {
		return allow(), nil
	}
	return l.checkHourly(ctx, l.now(), core.UsageFilter{}, l.globalPerHour, reasonGlobal)
}

// Reserve checks the requested limits and books one run atomically with
// respect to other Reserve calls (one process: the queue runs in it).
func (l *Limiter) Reserve(ctx context.Context, req core.ReserveRequest) (core.Decision, core.Reservation, error) {
	l.reserveMu.Lock()
	defer l.reserveMu.Unlock()
	u := req.Usage
	if req.User != nil {
		dec, err := l.CheckUser(ctx, u.UserID, req.Username, u.Kind, u.MRKey, *req.User)
		if err != nil || !dec.Allowed {
			return dec, nil, err
		}
	}
	if req.Global {
		dec, err := l.CheckGlobal(ctx)
		if err != nil || !dec.Allowed {
			return dec, nil, err
		}
	}
	if dec, err := l.checkKind(ctx, u.Kind, req.KindPerHour); err != nil || !dec.Allowed {
		return dec, nil, err
	}
	u.At = l.now()
	l.pendMu.Lock()
	if l.pending == nil {
		l.pending = map[uint64]core.Usage{}
	}
	l.seq++
	id := l.seq
	l.pending[id] = u
	l.pendMu.Unlock()
	return allow(), &reservation{l: l, id: id}, nil
}

// reservation is one booked run. It is done (committed or released) once
// its booking is gone from l.pending: only the reservation itself drops it,
// under mu, so a second Commit or Release is a no-op.
type reservation struct {
	mu sync.Mutex
	l  *Limiter
	id uint64
}

// Commit records the booked run (at the current time) and only then drops
// the booking, so the run is counted throughout.
func (r *reservation) Commit(ctx context.Context) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.l.pendMu.Lock()
	u, ok := r.l.pending[r.id]
	r.l.pendMu.Unlock()
	if !ok {
		return false, nil // already committed or released
	}
	u.At = time.Time{}
	err := r.l.Record(ctx, u)
	r.l.drop(r.id)
	return true, err
}

func (r *reservation) Release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.l.drop(r.id)
}

func (l *Limiter) drop(id uint64) {
	l.pendMu.Lock()
	delete(l.pending, id)
	l.pendMu.Unlock()
}

// Record stores one Claude run. A zero u.At is set to now(). Records older
// than 24h are pruned at most once per hour.
func (l *Limiter) Record(ctx context.Context, u core.Usage) error {
	now := l.now()
	if u.At.IsZero() {
		u.At = now
	}
	if err := l.st.RecordUsage(ctx, u); err != nil {
		return fmt.Errorf("limits: %w", err)
	}
	l.mu.Lock()
	due := l.lastPrune.IsZero() || now.Sub(l.lastPrune) >= pruneInterval
	if due {
		l.lastPrune = now
	}
	l.mu.Unlock()
	if due {
		if err := l.st.PruneUsage(ctx, now.Add(-retention)); err != nil {
			return fmt.Errorf("limits: prune: %w", err)
		}
	}
	return nil
}

// Remaining reports how many runs are left in the current window for each
// dimension; -1 means unlimited. Exempt users get -1 for per-user values.
func (l *Limiter) Remaining(ctx context.Context, userID int64, username string, lim core.UserLimits) (core.Remaining, error) {
	now := l.now()
	exempt := l.isExempt(username)
	r := core.Remaining{Reruns: -1, Chat: -1, Global: -1}

	count := func(f core.UsageFilter) (int, error) {
		recs, err := l.list(ctx, now, f)
		if err != nil {
			return 0, fmt.Errorf("limits: %w", err)
		}
		return len(recs), nil
	}
	left := func(limit, used int) int { return max(limit-used, 0) }

	if !exempt && lim.RerunsPerHour > 0 {
		k := core.UsageRerun
		n, err := count(core.UsageFilter{UserID: &userID, Kind: &k})
		if err != nil {
			return r, err
		}
		r.Reruns = left(lim.RerunsPerHour, n)
	}
	if !exempt && lim.ChatPerHour > 0 {
		k := core.UsageChat
		n, err := count(core.UsageFilter{UserID: &userID, Kind: &k})
		if err != nil {
			return r, err
		}
		r.Chat = left(lim.ChatPerHour, n)
	}
	if l.globalPerHour > 0 {
		n, err := count(core.UsageFilter{})
		if err != nil {
			return r, err
		}
		r.Global = left(l.globalPerHour, n)
	}
	return r, nil
}

// fmtDuration renders a duration in short Russian form: "10 мин", "1 ч",
// "1 ч 30 мин", "45 с".
func fmtDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d с", int(d.Round(time.Second)/time.Second))
	}
	d = d.Round(time.Minute)
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	switch {
	case h == 0:
		return fmt.Sprintf("%d мин", m)
	case m == 0:
		return fmt.Sprintf("%d ч", h)
	default:
		return fmt.Sprintf("%d ч %d мин", h, m)
	}
}
