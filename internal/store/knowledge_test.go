package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/learn"
	"github.com/fixcik/gitlab-review-bot/internal/memory"
)

func TestKnowledgeQueries(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)

	// Rules: global, two projects, one deleted.
	a, _, _ := s.AddLearning(ctx, learn.Rule{Scope: learn.ScopeGlobal, ProjectPath: "g/a", Text: "Всегда проверяй ошибки"})
	b, _, _ := s.AddLearning(ctx, learn.Rule{Scope: learn.ScopeProject, ProjectPath: "g/b", Text: "Не трогай vendor"})
	c, _, _ := s.AddLearning(ctx, learn.Rule{Scope: learn.ScopeProject, ProjectPath: "g/c", Text: "Лишнее"})
	if err := s.DeleteLearning(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListAllLearnings(ctx, 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("all: %+v %v", all, err)
	}
	ids := map[int64]bool{all[0].ID: true, all[1].ID: true}
	if !ids[a.ID] || !ids[b.ID] {
		t.Fatalf("ids %v", ids)
	}
	if one, err := s.ListAllLearnings(ctx, 1); err != nil || len(one) != 1 {
		t.Fatalf("limit: %+v %v", one, err)
	}

	// Memory of two projects, section order inside a project.
	for _, m := range []memory.Section{
		{ProjectID: 2, ProjectPath: "g/z", Key: memory.SectionTest, Content: "- t"},
		{ProjectID: 1, ProjectPath: "g/a", Key: memory.SectionTest, Content: "- t"},
		{ProjectID: 1, ProjectPath: "g/a", Key: memory.SectionBuild, Content: "- b"},
	} {
		if err := s.SaveMemorySection(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	mem, err := s.ListAllMemory(ctx)
	if err != nil || len(mem) != 3 || mem[0].ProjectPath != "g/a" || mem[0].Key != memory.SectionBuild ||
		mem[1].Key != memory.SectionTest || mem[2].ProjectPath != "g/z" {
		t.Fatalf("memory: %+v %v", mem, err)
	}

	// Outcomes: grouped per project (lower-cased), the period applies.
	old := time.Now().Add(-48 * time.Hour)
	rec := func(p, id, st string, at time.Time) {
		if _, err := s.RecordOutcome(ctx, learn.Outcome{ProjectPath: p, FindingID: id, Status: st, CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	rec("g/a", "1", learn.OutcomeFixed, now)
	rec("G/A", "2", learn.OutcomeDismissed, now)
	rec("g/a", "3", learn.OutcomeFixed, old)
	rec("g/b", "1", learn.OutcomeOpen, now)
	rec("g/b", "2", learn.OutcomeResolvedByBot, now)
	rec("g/b", "3", learn.OutcomeClosed, now)
	rec("g/b", "4", learn.OutcomeRejectedByVerifier, now)
	stats, err := s.OutcomeStatsAll(ctx, now.Add(-time.Hour))
	if err != nil || len(stats) != 2 {
		t.Fatalf("stats: %+v %v", stats, err)
	}
	if sa := stats["g/a"]; sa.Fixed != 1 || sa.Dismissed != 1 {
		t.Fatalf("g/a: %+v", sa)
	}
	if sb := stats["g/b"]; sb.Open != 1 || sb.ResolvedByBot != 1 || sb.Closed != 1 || sb.RejectedByVerifier != 1 {
		t.Fatalf("g/b: %+v", sb)
	}
	if wide, _ := s.OutcomeStatsAll(ctx, time.Time{}); wide["g/a"].Fixed != 2 {
		t.Fatalf("all time: %+v", wide)
	}
}

func TestBulkRuleChangesAndRestore(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	add := func(scope, project, text string) learn.Rule {
		t.Helper()
		r, _, err := s.AddLearning(ctx, learn.Rule{Scope: scope, ProjectPath: project, Text: text})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	g := add(learn.ScopeGlobal, "g/a", "Всегда проверяй ошибки")
	p := add(learn.ScopeProject, "g/b", "Не трогай vendor")
	n := add(learn.ScopeGlobal, "", "Без проекта")

	// Delete: all or nothing.
	if err := s.DeleteLearnings(ctx, []int64{g.ID, 999}); !errors.Is(err, learn.ErrRuleNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if live, _ := s.ListAllLearnings(ctx, 0); len(live) != 3 {
		t.Fatalf("a refused delete changed rules: %+v", live)
	}
	if err := s.DeleteLearnings(ctx, []int64{g.ID, p.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteLearnings(ctx, []int64{g.ID}); !errors.Is(err, learn.ErrRuleNotFound) {
		t.Fatalf("already deleted: %v", err)
	}
	del, err := s.ListDeletedLearnings(ctx, 0)
	if err != nil || len(del) != 2 || del[0].DeletedAt.IsZero() || del[0].Text == "" {
		t.Fatalf("deleted: %+v %v", del, err)
	}
	if one, _ := s.ListDeletedLearnings(ctx, 1); len(one) != 1 {
		t.Fatalf("limit: %+v", one)
	}

	// Scope: all or nothing; the project stays.
	if err := s.SetLearningsScope(ctx, []int64{n.ID}, learn.ScopeProject); !errors.Is(err, learn.ErrRuleNoProject) {
		t.Fatalf("no project: %v", err)
	}
	if err := s.SetLearningsScope(ctx, []int64{n.ID, g.ID}, learn.ScopeGlobal); !errors.Is(err, learn.ErrRuleNotFound) {
		t.Fatalf("deleted rule: %v", err)
	}

	// Restore: an equal live rule blocks it (AddLearning's de-dup key), and
	// nothing of the call is restored.
	again := add(learn.ScopeGlobal, "g/z", "  всегда   ПРОВЕРЯЙ ошибки ")
	conflicts, err := s.UndeleteLearnings(ctx, []int64{p.ID, g.ID})
	if err != nil || conflicts[g.ID] != again.ID || len(conflicts) != 1 {
		t.Fatalf("conflict: %v %v", conflicts, err)
	}
	if r, _ := s.GetLearning(ctx, p.ID); r != nil {
		t.Fatal("a refused restore restored a rule")
	}
	if err := s.UndeleteLearning(ctx, g.ID); !errors.Is(err, learn.ErrRuleExists) {
		t.Fatalf("single conflict: %v", err)
	}
	if err := s.UndeleteLearning(ctx, n.ID); !errors.Is(err, learn.ErrRuleNotFound) {
		t.Fatalf("live rule: %v", err)
	}
	if _, err := s.UndeleteLearnings(ctx, []int64{12345}); !errors.Is(err, learn.ErrRuleNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	// A project rule conflicts only within its project (case-insensitive).
	other := add(learn.ScopeProject, "g/c", "не трогай vendor")
	if err := s.UndeleteLearning(ctx, p.ID); err != nil {
		t.Fatalf("other project must not block: %v", err)
	}
	r, _ := s.GetLearning(ctx, p.ID)
	if r == nil || !r.UpdatedAt.Equal(p.UpdatedAt) || r.Scope != learn.ScopeProject || r.ProjectPath != "g/b" {
		t.Fatalf("restored: %+v (was %+v)", r, p)
	}
	_ = other

	// Two equal deleted rules in one call: the second conflicts with the first.
	if err := s.DeleteLearnings(ctx, []int64{again.ID}); err != nil {
		t.Fatal(err)
	}
	conflicts, err = s.UndeleteLearnings(ctx, []int64{g.ID, again.ID})
	if err != nil || conflicts[again.ID] != g.ID || len(conflicts) != 1 {
		t.Fatalf("intra-batch: %v %v", conflicts, err)
	}
	if r, _ := s.GetLearning(ctx, g.ID); r != nil {
		t.Fatal("intra-batch conflict must restore nothing")
	}
	if err := s.UndeleteLearning(ctx, g.ID); err != nil {
		t.Fatal(err)
	}

	// Scope change of several rules.
	if err := s.SetLearningsScope(ctx, []int64{g.ID, p.ID}, learn.ScopeProject); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{g.ID, p.ID} {
		if r, _ := s.GetLearning(ctx, id); r == nil || r.Scope != learn.ScopeProject || r.ProjectPath == "" {
			t.Fatalf("scope %d: %+v", id, r)
		}
	}
	if err := s.SetLearningsScope(ctx, []int64{g.ID}, learn.ScopeGlobal); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.GetLearning(ctx, g.ID); r.Scope != learn.ScopeGlobal || r.ProjectPath != "g/a" {
		t.Fatalf("global keeps the origin: %+v", r)
	}

	// A closed database fails every call.
	s.Close()
	if _, err := s.ListDeletedLearnings(ctx, 0); err == nil {
		t.Error("list deleted on a closed db")
	}
	if err := s.DeleteLearnings(ctx, []int64{1}); err == nil {
		t.Error("delete on a closed db")
	}
	if err := s.SetLearningsScope(ctx, []int64{1}, learn.ScopeGlobal); err == nil {
		t.Error("scope on a closed db")
	}
	if _, err := s.UndeleteLearnings(ctx, []int64{1}); err == nil {
		t.Error("restore on a closed db")
	}
	if err := s.UndeleteLearning(ctx, 1); err == nil {
		t.Error("restore one on a closed db")
	}
}

func TestDecisions(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	now := time.Now()
	rec := func(p, id, st string, created time.Time, decided int) int64 {
		t.Helper()
		oid, err := s.RecordOutcome(ctx, learn.Outcome{ProjectPath: p, FindingID: id, Title: "t" + id, BodyExcerpt: "b" + id, CreatedAt: created})
		if err != nil {
			t.Fatal(err)
		}
		if st != learn.OutcomeOpen {
			if _, err := s.db.ExecContext(ctx, `UPDATE finding_outcomes SET status = ?, updated_at = ? WHERE id = ?`,
				st, toNanos(now.Add(time.Duration(decided)*time.Minute)), oid); err != nil {
				t.Fatal(err)
			}
		}
		return oid
	}
	f1 := rec("g/a", "1", learn.OutcomeFixed, now.Add(-time.Hour), 3)
	d1 := rec("G/A", "2", learn.OutcomeDismissed, now.Add(-time.Hour), 1)
	r1 := rec("g/a", "3", learn.OutcomeRejectedByVerifier, now.Add(-time.Hour), 2)
	rec("g/a", "4", learn.OutcomeOpen, now.Add(-time.Hour), 0)
	rec("g/a", "5", learn.OutcomeResolvedByBot, now.Add(-time.Hour), 4)
	old := rec("g/a", "6", learn.OutcomeFixed, now.Add(-72*time.Hour), 5)
	other := rec("g/b", "7", learn.OutcomeDismissed, now.Add(-time.Hour), 6)

	since := now.Add(-2 * time.Hour)
	ids := func(os []learn.Outcome) []int64 {
		var out []int64
		for _, o := range os {
			out = append(out, o.ID)
		}
		return out
	}
	eq := func(name string, got []learn.Outcome, err error, want ...int64) {
		t.Helper()
		if err != nil || fmt.Sprint(ids(got)) != fmt.Sprint(want) {
			t.Errorf("%s: %v %v, want %v", name, ids(got), err, want)
		}
	}
	got, err := s.ListDecisions(ctx, learn.DecisionFilter{ProjectPath: "g/A", Since: since})
	eq("newest decision first", got, err, f1, r1, d1)
	if len(got) > 0 && got[0].BodyExcerpt != "b1" {
		t.Errorf("excerpt: %+v", got[0])
	}
	got, err = s.ListDecisions(ctx, learn.DecisionFilter{Since: since, Limit: 2, Offset: 1})
	eq("page", got, err, f1, r1)
	got, err = s.ListDecisions(ctx, learn.DecisionFilter{ProjectPath: "g/a", Status: learn.OutcomeDismissed})
	eq("status, all time", got, err, d1)
	got, err = s.ListDecisions(ctx, learn.DecisionFilter{ProjectPath: "g/a"})
	eq("all time", got, err, old, f1, r1, d1)
	got, err = s.DecisionSeries(ctx, "", since, 0)
	eq("series oldest first, fixed and dismissed only", got, err, d1, f1, other)
	got, err = s.DecisionSeries(ctx, "g/a", time.Time{}, 2)
	eq("series limit", got, err, d1, f1)

	s.Close()
	if _, err := s.ListDecisions(ctx, learn.DecisionFilter{}); err == nil {
		t.Error("closed db")
	}
	if _, err := s.DecisionSeries(ctx, "", time.Time{}, 0); err == nil {
		t.Error("closed db")
	}
}
