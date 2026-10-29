package review_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/descsum"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

func summaryOut(t *testing.T) string {
	return reviewtest.MustJSON(t, output.ReviewOutput{Summary: "ок", DescriptionSummary: &output.DescriptionSummary{
		Overview: "Добавляет LRU-кэш заказов.",
		Groups:   []output.SummaryGroup{{Kind: "new", Items: []string{"кэш заказов на 5 минут", "/close @bob"}}, {Kind: "test", Items: []string{"тесты вытеснения"}}},
	}})
}

func runDescReview(t *testing.T, h *reviewtest.Harness, out string) {
	t.Helper()
	h.Runner.Resps = append(h.Runner.Resps, reviewtest.RunResp{Out: out})
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
}

// Off by default: no schema property, no prompt rule, no write.
func TestDescriptionSummaryOffByDefault(t *testing.T) {
	h := reviewtest.New(t)
	d := &reviewtest.FakeDescription{GL: h.GL}
	h.Deps.Description = d
	runDescReview(t, h, summaryOut(t))
	req := h.Runner.Reqs[0]
	if strings.Contains(req.JSONSchema, "description_summary") || strings.Contains(req.SystemPrompt, "Description summary") || len(d.Puts) != 0 {
		t.Fatalf("description summary while off: puts %d", len(d.Puts))
	}
}

// On: the block goes to the end of the author's text; the next review with
// the same summary writes nothing; the author's text survives.
func TestDescriptionSummaryWritten(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.MR.Description = "Описание автора.\n"
	d := reviewtest.WithDescription(h, nil)
	runDescReview(t, h, summaryOut(t))
	req := h.Runner.Reqs[0]
	if !strings.Contains(req.JSONSchema, `"description_summary"`) || !strings.Contains(req.SystemPrompt, "Description summary") || !strings.Contains(req.SystemPrompt, "in Russian") {
		t.Fatal("schema / rules missing")
	}
	if len(d.Puts) != 1 {
		t.Fatalf("puts %d", len(d.Puts))
	}
	got := d.Puts[0]
	if !strings.HasPrefix(got, "Описание автора.\n\n"+descsum.Start+"\n**Саммари от ревью-бота**") || !strings.HasSuffix(got, descsum.End) ||
		!strings.Contains(got, "**Новое**\n- кэш заказов на 5 минут\n- \\/close @​bob") || !strings.Contains(got, "**Тесты**") {
		t.Fatalf("description:\n%s", got)
	}
	if sum := reviewtest.SummaryBody(t, h); !strings.Contains(sum, descsum.Written) {
		t.Fatalf("the summary note must record the block:\n%s", sum)
	}
	runDescReview(t, h, summaryOut(t))
	if len(d.Puts) != 1 {
		t.Errorf("an unchanged description was written again: %d", len(d.Puts))
	}
	// The author removes the block: never added again.
	h.GL.MR.Description = "Описание автора, без блока."
	runDescReview(t, h, summaryOut(t))
	if len(d.Puts) != 1 {
		t.Errorf("a removed block was added again")
	}
}

// Drafts and closed MRs are left alone; so is an empty summary.
func TestDescriptionSummarySkipped(t *testing.T) {
	for _, c := range []struct {
		name string
		mut  func(*core.MergeRequest)
		out  func(t *testing.T) string
	}{
		{"draft", func(m *core.MergeRequest) { m.Draft = true }, summaryOut},
		{"closed", func(m *core.MergeRequest) { m.State = "closed" }, summaryOut},
		{"no summary", func(*core.MergeRequest) {}, func(t *testing.T) string { return reviewtest.ReviewOut(t, nil, nil) }},
	} {
		h := reviewtest.New(t)
		d := reviewtest.WithDescription(h, nil)
		// The review read the MR as opened; it changed before the write.
		d.Fresh = c.mut
		runDescReview(t, h, c.out(t))
		if len(d.Puts) != 0 {
			t.Errorf("%s: puts %d", c.name, len(d.Puts))
		}
	}
}

// 403: one problem, the review goes on; the summary note does not claim the
// block.
func TestDescriptionSummaryForbidden(t *testing.T) {
	ph := reviewtest.NewProbHarness(t, "")
	d := reviewtest.WithDescription(ph.Harness, fmt.Errorf("gitlab: 403: %w", core.ErrForbidden))
	if err := ph.Review(t, reviewtest.RunResp{Out: summaryOut(t)}); err != nil {
		t.Fatal(err)
	}
	if len(d.Puts) != 1 {
		t.Fatalf("puts %d", len(d.Puts))
	}
	if k := ph.St.Kinds(); !strings.Contains(strings.Join(k[problems.DescriptionSummaryFailed], "|"), "описание MR: нет прав") {
		t.Errorf("problems %v", k)
	}
	if strings.Contains(ph.Summary(t), descsum.Written) {
		t.Error("a failed write is not recorded as written")
	}
}
