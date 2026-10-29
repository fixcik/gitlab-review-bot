package crossrepo_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/crossrepo"
	"github.com/fixcik/gitlab-review-bot/internal/review/jsonschema"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

const seqDiagram = "sequenceDiagram\n    participant C as Клиент\n    participant A as API\n    participant R as Redis\n    C->>A: GET /items\n    A->>R: GET cache:items\n    Note over A,R: новое: кэш LRU\n    R-->>A: hit / miss\n    A-->>C: 200 OK"

const flowDiagram = "flowchart TD\n    A[Запрос] --> B{Есть в кэше?}\n    B -- да --> C[Ответ из кэша]:::changed\n    B -- нет --> D[Запрос в БД]\n    classDef changed fill:#fff3bf,stroke:#f08c00"

func reviewOutDiagram(t *testing.T, diagram string) string {
	return reviewtest.MustJSON(t, output.ReviewOutput{
		Summary:     "Добавлен LRU-кэш.",
		Walkthrough: []output.FileSummary{{Path: "cache.go", Summary: "кэш"}},
		Diagram:     diagram,
	})
}

func TestSanitizeDiagramValid(t *testing.T) {
	for name, in := range map[string]string{
		"sequence":        seqDiagram,
		"flowchart":       flowDiagram,
		"graph LR":        "graph LR\n  a --> b",
		"fenced":          "```mermaid\n" + flowDiagram + "\n```",
		"crlf":            strings.ReplaceAll(seqDiagram, "\n", "\r\n"),
		"comparison text": "flowchart TD\n  a[\"size < limit\"] --> b",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := crossrepo.SanitizeDiagram(in)
			if err != nil || got == "" {
				t.Fatalf("rejected: %v", err)
			}
			if strings.Contains(got, "```") || strings.Contains(got, "\r") {
				t.Fatalf("not normalized: %q", got)
			}
		})
	}
	if got, err := crossrepo.SanitizeDiagram("  \n "); got != "" || err != nil {
		t.Fatalf("empty: %q %v", got, err)
	}
}

func TestRenderDiagramSection(t *testing.T) {
	if crossrepo.RenderDiagramSection("") != "" {
		t.Fatal("empty diagram must render nothing")
	}
	s := crossrepo.RenderDiagramSection(seqDiagram)
	for _, want := range []string{marker.DiagramStart, "### 🗺 Схема изменений", "```mermaid\nsequenceDiagram", marker.DiagramEnd} {
		if !strings.Contains(s, want) {
			t.Errorf("section lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "<details>") {
		t.Fatal("short diagram must not be collapsed")
	}
	big := "flowchart TD" + strings.Repeat("\n  a --> b", 25)
	s = crossrepo.RenderDiagramSection(big)
	if !strings.Contains(s, "<details>\n<summary>🗺 Схема изменений</summary>") || !strings.Contains(s, "</details>") {
		t.Fatalf("long diagram must be collapsed:\n%s", s)
	}
	if crossrepo.PreviousDiagram("x\n"+s) != big {
		t.Fatal("previousDiagram must round-trip")
	}
}

func TestDiagramRenderedInSummary(t *testing.T) {
	for name, d := range map[string]string{"sequence": seqDiagram, "flowchart": flowDiagram} {
		t.Run(name, func(t *testing.T) {
			h := reviewtest.New(t)
			h.Runner.Resps = []reviewtest.RunResp{{Out: reviewOutDiagram(t, d)}}
			if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
				t.Fatal(err)
			}
			req := h.Runner.Reqs[0]
			props, _ := reviewtest.SchemaProps(t, req.JSONSchema)
			if props["diagram"] == nil {
				t.Fatal("schema lacks diagram")
			}
			for _, want := range []string{"sequenceDiagram", "flowchart TD", "merge request IMPLEMENTS", "Draw it for every merge request that changes behavior", "Return \"\" only for trivial changes", "25 nodes", "Russian"} {
				if !strings.Contains(req.SystemPrompt, want) {
					t.Errorf("system prompt lacks %q", want)
				}
			}
			body := reviewtest.SummaryBody(t, h)
			sec := strings.Index(body, "🗺 Схема изменений")
			sum := strings.Index(body, "Добавлен LRU-кэш.")
			walk := strings.Index(body, "Обзор изменений")
			if sec < 0 || sum < 0 || walk < 0 || !(sum < sec && sec < walk) {
				t.Fatalf("order summary(%d) < diagram(%d) < walkthrough(%d):\n%s", sum, sec, walk, body)
			}
			if !strings.Contains(body, "```mermaid\n"+strings.SplitN(d, "\n", 2)[0]) {
				t.Fatalf("no mermaid block:\n%s", body)
			}
		})
	}
}

func TestDiagramDangerousOmitted(t *testing.T) {
	h := reviewtest.New(t)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewOutDiagram(t, "flowchart TD\n  a --> b\n  click a \"javascript:alert(1)\"")}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	body := reviewtest.SummaryBody(t, h)
	if strings.Contains(body, "Схема изменений") || strings.Contains(body, "mermaid") || strings.Contains(body, "javascript") {
		t.Fatalf("dangerous diagram rendered:\n%s", body)
	}
	if !strings.Contains(body, "Добавлен LRU-кэш.") {
		t.Fatal("review must still be published")
	}
}

func TestDiagramEmptyNoSection(t *testing.T) {
	h := reviewtest.New(t)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewOutDiagram(t, "")}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if body := reviewtest.SummaryBody(t, h); strings.Contains(body, "Схема изменений") || strings.Contains(body, marker.DiagramStart) {
		t.Fatalf("empty diagram rendered:\n%s", body)
	}
}

func incrementalHarness(t *testing.T, prevBody string) *reviewtest.Harness {
	h := reviewtest.New(t)
	h.St.States["42!7"] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: "old0000000000000", SummaryNoteID: 500}
	h.GL.Discussions = []core.Discussion{{ID: "s", Notes: []core.Note{reviewtest.BotNote(500, prevBody, nil)}}}
	h.Repos.Ancestors["old0000000000000"] = true
	return h
}

func TestDiagramIncrementalKeepsPrevious(t *testing.T) {
	prev := marker.Summary("old0000000000000") + "\n" + summary.Title + "\n\nСтарое.\n\n" + crossrepo.RenderDiagramSection(flowDiagram) + "footer"
	h := incrementalHarness(t, prev)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewOutDiagram(t, "")}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.Runner.Reqs[0].SystemPrompt, "keeps the previous diagram") {
		t.Error("incremental prompt must say the previous diagram is kept")
	}
	body := h.GL.Notes[500]
	if !strings.Contains(body, "```mermaid\n"+flowDiagram+"\n```") {
		t.Fatalf("previous diagram lost:\n%s", body)
	}

	// A new diagram replaces the previous one.
	h = incrementalHarness(t, prev)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewOutDiagram(t, seqDiagram)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	body = h.GL.Notes[500]
	if !strings.Contains(body, "sequenceDiagram") || strings.Contains(body, "flowchart TD") || strings.Count(body, marker.DiagramStart) != 1 {
		t.Fatalf("diagram not replaced:\n%s", body)
	}
}

func TestDiagramFullReviewDropsPrevious(t *testing.T) {
	prev := marker.Summary("old0000000000000") + "\n" + summary.Title + "\n\n" + crossrepo.RenderDiagramSection(flowDiagram)
	h := incrementalHarness(t, prev)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewOutDiagram(t, "")}}
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobFullReview)); err != nil {
		t.Fatal(err)
	}
	if body := h.GL.Notes[500]; strings.Contains(body, "mermaid") {
		t.Fatalf("full review must not keep the old diagram:\n%s", body)
	}
}

func TestDiagramSwitches(t *testing.T) {
	for name, tc := range map[string]struct {
		svc  bool
		repo string
	}{
		"service off": {svc: false},
		"repo off":    {svc: true, repo: "diagram: false\n"},
	} {
		t.Run(name, func(t *testing.T) {
			h := reviewtest.New(t)
			h.Cfg.Review.Diagram = tc.svc
			h.GL.RepoConfig = tc.repo
			h.Runner.Resps = []reviewtest.RunResp{{Out: reviewOutDiagram(t, seqDiagram)}}
			if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
				t.Fatal(err)
			}
			req := h.Runner.Reqs[0]
			if props, _ := reviewtest.SchemaProps(t, req.JSONSchema); props["diagram"] != nil {
				t.Error("schema must not ask for a diagram")
			}
			if strings.Contains(req.SystemPrompt, "Diagram (") {
				t.Error("system prompt must not mention the diagram")
			}
			if body := reviewtest.SummaryBody(t, h); strings.Contains(body, "mermaid") {
				t.Fatalf("diagram rendered while off:\n%s", body)
			}
		})
	}
	// An explicit diagram: true in the repo keeps it on.
	h := reviewtest.New(t)
	h.GL.RepoConfig = "diagram: true\n"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewOutDiagram(t, seqDiagram)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if body := reviewtest.SummaryBody(t, h); !strings.Contains(body, "mermaid") {
		t.Fatal("diagram: true must keep it on")
	}
}

func TestDiagramSectionHelper(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	on := config.Effective{Diagram: true}
	prev := crossrepo.RenderDiagramSection(flowDiagram)
	out := &output.ReviewOutput{Diagram: "pie\n  \"a\": 1"}
	if s := crossrepo.DiagramSection(log, on, out, prev, true); !strings.Contains(s, flowDiagram) {
		t.Fatal("invalid new diagram on incremental run must fall back to the previous one")
	}
	if s := crossrepo.DiagramSection(log, on, out, prev, false); s != "" {
		t.Fatal("invalid diagram on a full run: no section")
	}
	if s := crossrepo.DiagramSection(log, config.Effective{}, &output.ReviewOutput{Diagram: seqDiagram}, prev, true); s != "" {
		t.Fatal("disabled: no section")
	}
	if schema := crossrepo.DiagramSchema(jsonschema.Review, false); strings.Contains(schema, "\"diagram\"") {
		t.Fatal("diagramSchema(false) must drop the property")
	}
	if crossrepo.DiagramSchema(jsonschema.Review, true) != jsonschema.Review {
		t.Fatal("diagramSchema(true) must keep the schema")
	}
}
