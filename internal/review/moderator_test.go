package review_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

func TestModeratorBlockingAndNonBlocking(t *testing.T) {
	h := reviewtest.New(t)
	m := reviewtest.WithModerator(h)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{
		{Path: "cache.go", Line: 2, Severity: "critical", Title: "Data loss", Body: "writes are dropped"},
		{Path: "cache.go", Line: 4, Severity: "major", Title: "Slow path", Body: "O(n^2)"},
		{Path: "cache.go", Line: 5, Severity: "nit", Title: "Naming", Body: "rename"},
	}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(m.Reviewers) != 1 || m.Reviewers[0] != reviewtest.BotID {
		t.Fatalf("bot must assign itself as reviewer: %v", m.Reviewers)
	}
	if len(h.GL.Published) != 3 {
		t.Fatalf("published: %+v", h.GL.Published)
	}
	crit, major, nit := h.GL.Published[0].Body, h.GL.Published[1].Body, h.GL.Published[2].Body
	if !strings.Contains(crit, marker.Blocking) || !strings.Contains(crit, "Блокирует мерж") {
		t.Errorf("critical must block:\n%s", crit)
	}
	for _, b := range []string{major, nit} {
		if strings.Contains(b, marker.Blocking) || !strings.Contains(b, "Не блокирует мерж") {
			t.Errorf("non-blocking label wrong:\n%s", b)
		}
	}
	// Exactly the two non-blocking threads were resolved by the service
	// token; the critical one stays open.
	byBody := map[string]string{}
	for _, d := range h.GL.Discussions {
		byBody[d.ID] = d.Notes[0].Body
	}
	if len(m.Resolved) != 2 {
		t.Fatalf("resolved %v", m.Resolved)
	}
	for _, id := range m.Resolved {
		if strings.Contains(byBody[id], marker.Blocking) {
			t.Errorf("blocking thread %s was resolved", id)
		}
	}
	sum := reviewtest.SummaryBody(t, h)
	if !strings.Contains(sum, "⛔ **Мерж заблокирован:** 1 блокирующее замечание") {
		t.Errorf("summary gate line:\n%s", sum)
	}
}

func TestModeratorResolvesFixedThread(t *testing.T) {
	h := reviewtest.New(t)
	m := reviewtest.WithModerator(h)
	id := finding.ID("cache.go", "Data loss")
	pos := &core.Position{NewPath: "cache.go", OldPath: "cache.go", NewLine: 2}
	h.GL.Discussions = []core.Discussion{{ID: "d1", Resolvable: true, Notes: []core.Note{
		reviewtest.BotNote(1, finding.NoteBodyOpts(id, output.Finding{Severity: "critical", Title: "Data loss"}, false, 0, finding.NoteOpts{Blocking: true}), pos),
	}}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, []output.ResolvedFinding{{ID: id, Comment: "запись больше не теряется"}})}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(m.Resolved) != 1 || m.Resolved[0] != "d1" {
		t.Fatalf("fixed blocking thread must be resolved by the service token: %v", m.Resolved)
	}
	if r := reviewtest.LastReply(t, h.GL); !strings.Contains(r.Body, "✅ Исправлено: запись больше не теряется") {
		t.Errorf("reply: %q", r.Body)
	}
	if sum := reviewtest.SummaryBody(t, h); !strings.Contains(sum, "✅ Блокирующих замечаний нет.") {
		t.Errorf("summary gate line:\n%s", sum)
	}
}

// Without a service token the bot resolves its own non-blocking threads with
// its token (GitLab lets the author of a thread resolve it), so the promise
// "does not block" holds.
func TestNoModeratorStillLabelsNonBlocking(t *testing.T) {
	h := reviewtest.New(t)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{
		{Path: "cache.go", Line: 4, Severity: "major", Title: "Slow path", Body: "O(n^2)"},
	}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if b := h.GL.Published[0].Body; !strings.Contains(b, "Не блокирует") || strings.Contains(b, marker.Blocking) {
		t.Errorf("body:\n%s", b)
	}
}

func TestBlockMinSeverityMajor(t *testing.T) {
	h := reviewtest.New(t)
	h.Cfg.Review.BlockMinSeverity = "major"
	reviewtest.WithModerator(h)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{
		{Path: "cache.go", Line: 4, Severity: "major", Title: "Slow path", Body: "O(n^2)"},
	}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.GL.Published[0].Body, marker.Blocking) {
		t.Errorf("major must block with block_min_severity=major")
	}
}
