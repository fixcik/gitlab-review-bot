package core

import (
	"context"
	"sync"
	"time"
)

// Decision is the result of a rate-limit check.
type Decision struct {
	Allowed bool
	RetryAt time.Time // when the request would be allowed; zero if Allowed
	Reason  string    // human-readable, Russian, for replying to the user
}

type Remaining struct {
	Reruns int
	Chat   int
	Global int
}

// UserLimits are the per-user limits for one check. They come from the
// effective (service + repo) config, so a repo can make them stricter.
// Zero or negative values mean "no limit" for that dimension.
type UserLimits struct {
	RerunsPerHour   int
	ChatPerHour     int
	MRRerunCooldown time.Duration
}

type Limiter interface {
	// CheckUser applies per-user limits for kind (UsageRerun or UsageChat),
	// including the per-MR rerun cooldown for UsageRerun. Exempt users are
	// always allowed. Windows are sliding: the last hour before now.
	CheckUser(ctx context.Context, userID int64, username string, kind UsageKind, mrKey string, lim UserLimits) (Decision, error)
	// CheckGlobal applies the global hourly limit on all Claude runs.
	CheckGlobal(ctx context.Context) (Decision, error)
	// Record stores one Claude run (call right before starting it).
	Record(ctx context.Context, u Usage) error
	Remaining(ctx context.Context, userID int64, username string, lim UserLimits) (Remaining, error)
	// Reserve checks the requested limits and books one run in one step,
	// so two jobs checked at the same time cannot both take the last slot.
	// A booked run counts toward every limit until it is committed (the
	// Claude run starts and is recorded) or released (the job ended without
	// a run). Denied: Allowed false and a nil Reservation.
	Reserve(ctx context.Context, req ReserveRequest) (Decision, Reservation, error)
}

// ReserveRequest is what Limiter.Reserve checks and books.
type ReserveRequest struct {
	// Usage is the run to book (Kind, MRKey, UserID; At is set at commit).
	Usage Usage
	// Username for exemptions; User, when set, applies the per-user limits
	// of Usage.Kind (CheckUser); Global applies the global hourly limit.
	Username string
	User     *UserLimits
	Global   bool
	// KindPerHour > 0 caps the runs of Usage.Kind in the last hour, of all
	// users and booked runs included (review.learn.max_per_hour for
	// core.UsageLearn). ReserveByChecks has no usage records and does not
	// apply it.
	KindPerHour int
}

// Reservation is a booked run.
type Reservation interface {
	// Commit records the booked run now. It returns false (and records
	// nothing) when the reservation was already committed or released, so
	// the caller records a further run itself.
	Commit(ctx context.Context) (bool, error)
	// Release drops an uncommitted booking; no-op after Commit.
	Release()
}

// ReserveByChecks is a non-atomic Reserve for simple Limiter
// implementations (tests): it runs the checks and books nothing until
// Commit, which calls Record.
func ReserveByChecks(ctx context.Context, l Limiter, req ReserveRequest) (Decision, Reservation, error) {
	if req.User != nil {
		dec, err := l.CheckUser(ctx, req.Usage.UserID, req.Username, req.Usage.Kind, req.Usage.MRKey, *req.User)
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
	return Decision{Allowed: true}, &recordOnCommit{l: l, u: req.Usage}, nil
}

type recordOnCommit struct {
	mu   sync.Mutex
	l    Limiter
	u    Usage
	done bool
}

func (r *recordOnCommit) Commit(ctx context.Context) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return false, nil
	}
	r.done = true
	r.u.At = time.Time{}
	return true, r.l.Record(ctx, r.u)
}

func (r *recordOnCommit) Release() {
	r.mu.Lock()
	r.done = true
	r.mu.Unlock()
}

type reservationKey struct{}

// WithReservation stores the job's booked run in ctx: the first Claude run
// of the job commits it instead of recording a new run.
func WithReservation(ctx context.Context, r Reservation) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, reservationKey{}, r)
}

// ReservationFrom returns the booked run stored by WithReservation.
func ReservationFrom(ctx context.Context) Reservation {
	r, _ := ctx.Value(reservationKey{}).(Reservation)
	return r
}
