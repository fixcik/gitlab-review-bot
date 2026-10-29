package diag

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// LogHandler wraps a slog.Handler: log records that carry an "mr" attribute
// (the MR key, as internal/review logs it) of a job being recorded are also
// added to that job's timeline. This captures the review flow's own
// decisions (skips, fallbacks, warnings) without editing it. Debug records
// are captured even when the wrapped handler drops them.
//
// The queue runs at most one job per MR at a time, so the MR key identifies
// the job.
func (rec *Recorder) LogHandler(h slog.Handler) slog.Handler {
	if rec == nil {
		return h
	}
	return &logHandler{next: h, rec: rec}
}

// Logger returns a logger whose records also go to the job timelines.
func (rec *Recorder) Logger(l *slog.Logger) *slog.Logger {
	if rec == nil || l == nil {
		return l
	}
	return slog.New(rec.LogHandler(l.Handler()))
}

type logHandler struct {
	next  slog.Handler
	rec   *Recorder
	attrs []slog.Attr // accumulated With attrs (groups are flattened)
	mrKey string
}

func (h *logHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelDebug || h.next.Enabled(ctx, l)
}

func (h *logHandler) Handle(ctx context.Context, r slog.Record) error {
	h.capture(ctx, r)
	if !h.next.Enabled(ctx, r.Level) {
		return nil
	}
	return h.next.Handle(ctx, r)
}

func (h *logHandler) WithAttrs(as []slog.Attr) slog.Handler {
	n := &logHandler{next: h.next.WithAttrs(as), rec: h.rec, mrKey: h.mrKey}
	n.attrs = append(append([]slog.Attr(nil), h.attrs...), as...)
	for _, a := range as {
		if k, ok := mrKeyOf(a); ok {
			n.mrKey = k
		}
	}
	return n
}

// mrKeyOf returns the MR key of an "mr" attribute.
func mrKeyOf(a slog.Attr) (string, bool) {
	if a.Key == "mr" && a.Value.Kind() == slog.KindString {
		return a.Value.String(), true
	}
	return "", false
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	return &logHandler{next: h.next.WithGroup(name), rec: h.rec, attrs: h.attrs, mrKey: h.mrKey}
}

// ignoredPrefixes are log sources that are not part of the job.
var ignoredPrefixes = []string{"webhook:", "queue:"}

func (h *logHandler) capture(ctx context.Context, r slog.Record) {
	run := FromContext(ctx)
	if run == nil {
		key := h.mrKey
		r.Attrs(func(a slog.Attr) bool {
			if k, ok := mrKeyOf(a); ok {
				key = k
			}
			return true
		})
		if key == "" {
			return
		}
		for _, p := range ignoredPrefixes {
			if strings.HasPrefix(r.Message, p) {
				return
			}
		}
		h.rec.mu.Lock()
		run = h.rec.active[key]
		h.rec.mu.Unlock()
		if run == nil {
			return
		}
	}
	fields := Fields{}
	for _, a := range h.attrs {
		addAttr(fields, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		addAttr(fields, a)
		return true
	})
	delete(fields, "mr")
	status := StepInfo
	switch {
	case r.Level >= slog.LevelError:
		status = StepError
	case r.Level >= slog.LevelWarn:
		status = StepWarn
	}
	if isSkip(r.Message) {
		run.mu.Lock()
		if run.skip == "" {
			run.skip = run.rec.scrub.str(r.Message, run.safe)
		}
		run.mu.Unlock()
	}
	run.event(KindLog, r.Level.String()+": "+r.Message, status, "", fields)
}

func addAttr(f Fields, a slog.Attr) {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindGroup:
		for _, g := range v.Group() {
			addAttr(f, slog.Attr{Key: a.Key + "." + g.Key, Value: g.Value})
		}
	case slog.KindString:
		f[a.Key] = v.String()
	case slog.KindInt64, slog.KindUint64, slog.KindFloat64, slog.KindBool:
		f[a.Key] = v.Any()
	default:
		f[a.Key] = fmt.Sprint(v.Any())
	}
}

// skipWords mark a log line that explains why a job did nothing.
var skipWords = []string{"skipping", "skipped", "nothing to review", "already reviewed", "disabled for repo",
	"auto review disabled", "not a reply", "not started by the bot", "below developer", "already processed", "nothing to learn"}

func isSkip(msg string) bool {
	m := strings.ToLower(msg)
	for _, w := range skipWords {
		if strings.Contains(m, w) {
			return true
		}
	}
	return false
}
