package webhook

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/events"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// Without a Router the handler routes with events.NewRouter(events.Options{}):
// no mention is configured, so a note that would be a command elsewhere
// produces no job, while MR events of any project still do.
func TestNilRouterDefaults(t *testing.T) {
	rec := &recorder{}
	h := NewHandler(Options{Secret: secret, Enqueue: rec.enqueue})
	w := do(h, http.MethodPost, "Note Hook", secret, noteHook("alice", "@reviewbot review", false, "MergeRequest"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ignored: no mention configured") {
		t.Fatalf("note: %d %q", w.Code, w.Body.String())
	}
	if len(rec.jobs) != 0 {
		t.Fatalf("note enqueued without a mention: %+v", rec.jobs)
	}
	w = do(h, http.MethodPost, "Merge Request Hook", secret, mrHook("open", "opened", false, "", ""))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "accepted") || len(rec.jobs) != 1 ||
		rec.jobs[0].Kind != core.JobAutoReview {
		t.Fatalf("mr: %d %q %+v", w.Code, w.Body.String(), rec.jobs)
	}
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// A body that fails to read (not because it is too large) is a 400 and no
// job; the secret is still checked first.
func TestBodyReadError(t *testing.T) {
	rec := &recorder{}
	h := newTestHandler(rec, "")
	send := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/webhook", failingBody{})
		req.Header.Set("X-Gitlab-Event", "Note Hook")
		req.Header.Set("X-Gitlab-Token", token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if w := send(secret); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "read error") {
		t.Fatalf("read error: %d %q", w.Code, w.Body.String())
	}
	if w := send("wrong"); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad token with a failing body: %d", w.Code)
	}
	if len(rec.jobs) != 0 {
		t.Fatalf("jobs: %+v", rec.jobs)
	}
}

// parseHookTime reads the Pipeline Hook's "YYYY-MM-DD hh:mm:ss UTC", a
// numeric offset and RFC 3339 (always returned in UTC); anything else is
// zero (unknown), never an error.
func TestParseHookTime(t *testing.T) {
	want := time.Date(2026, 10, 24, 10, 5, 0, 0, time.UTC)
	for _, tc := range []struct {
		in   string
		want time.Time
	}{
		{"2026-10-24 10:05:00 UTC", want},
		{"  2026-10-24 10:05:00 UTC\n", want},
		{"2026-10-24 13:05:00 +0300", want},
		{"2026-10-24T10:05:00Z", want},
		{"2026-10-24T12:05:00.5+02:00", want.Add(500 * time.Millisecond)},
		{"", time.Time{}},
		{"null", time.Time{}},
		{"24.10.2026 10:05", time.Time{}},
		{"2026-10-24", time.Time{}},
	} {
		got := parseHookTime(tc.in)
		if !got.Equal(tc.want) || (!got.IsZero() && got.Location() != time.UTC) {
			t.Errorf("parseHookTime(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// A Pipeline Hook whose finished_at is unreadable is still accepted, with
// an unknown time (the dedup then compares statuses only).
func TestPipelineHookUnreadableFinishedAt(t *testing.T) {
	rec := &recorder{}
	h := newPipelineHandler(rec, true, nil)
	body := strings.Replace(pipelineHook(31, "main", "failed", false, ""),
		`"finished_at": "2026-10-24 10:05:00 UTC"`, `"finished_at": "soon"`, 1)
	if !strings.Contains(body, `"soon"`) {
		t.Fatal("fixture did not change")
	}
	w := do(h, http.MethodPost, "Pipeline Hook", secret, body)
	if w.Code != http.StatusOK || len(rec.jobs) != 1 {
		t.Fatalf("%d %q %+v", w.Code, w.Body.String(), rec.jobs)
	}
	if p := rec.jobs[0].Pipeline; p == nil || !p.At.IsZero() || p.ID != 31 {
		t.Fatalf("pipeline ref %+v", p)
	}
	// The readable time of the fixture is carried as is.
	rec.jobs = nil
	do(h, http.MethodPost, "Pipeline Hook", secret, pipelineHook(31, "main", "failed", false, ""))
	if len(rec.jobs) != 1 || !rec.jobs[0].Pipeline.At.Equal(time.Date(2026, 10, 24, 10, 5, 0, 0, time.UTC)) {
		t.Fatalf("finished_at: %+v", rec.jobs)
	}
}

// The merge_request object of a Pipeline Hook names the MR only with a
// positive iid; otherwise the pipeline is routed by its ref.
func TestPipelineHookMergeRequestIID(t *testing.T) {
	for _, tc := range []struct {
		mr   string
		want int64
	}{
		{`{"iid": 1, "state": "opened"}`, 1},
		{`{"iid": 12, "state": "opened"}`, 12},
		{`{"iid": 0, "state": "opened"}`, 0},
		{`{"iid": -3, "state": "opened"}`, 0},
		{`null`, 0},
	} {
		rec := &recorder{}
		h := newPipelineHandler(rec, true, nil)
		do(h, http.MethodPost, "Pipeline Hook", secret, pipelineHook(31, "feature", "failed", false, tc.mr))
		if len(rec.jobs) != 1 || rec.jobs[0].MRIID != tc.want {
			t.Errorf("merge_request %s: jobs %+v, want MR %d", tc.mr, rec.jobs, tc.want)
		}
	}
}

// A 405 names the allowed method.
func TestMethodNotAllowedAllowHeader(t *testing.T) {
	w := do(newTestHandler(&recorder{}, ""), http.MethodGet, "Merge Request Hook", secret, "")
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("%d Allow=%q", w.Code, w.Header().Get("Allow"))
	}
}

// Without Enqueue an accepted event is still answered "accepted".
func TestNilEnqueueAccepts(t *testing.T) {
	h := NewHandler(Options{Secret: secret, Router: events.NewRouter(events.Options{BotUsername: "reviewbot"})})
	w := do(h, http.MethodPost, "Merge Request Hook", secret, mrHook("open", "opened", false, "", ""))
	if w.Code != http.StatusOK || w.Body.String() != "accepted\n" {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
}

// Behind the tracing middleware an ignored event marks its request span.
func TestIgnoredEventSpanResult(t *testing.T) {
	s := &spanSink{}
	tr := tracing.New(tracing.Options{Exporter: s})
	h := tr.Handler("/webhook", newTestHandler(&recorder{}, ""), nil)
	w := do(h, http.MethodPost, "Push Hook", secret, `{}`)
	if w.Code != http.StatusOK || w.Body.String() != "ignored: unsupported event\n" {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
	if len(s.spans) != 1 {
		t.Fatalf("%d spans", len(s.spans))
	}
	if v, _ := s.spans[0].AttrValue("reviewbot.webhook.result"); v.Str != "ignored" {
		t.Fatalf("result attr %v", v)
	}
}
