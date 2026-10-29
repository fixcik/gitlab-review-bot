package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
)

func TestSysmapRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	st := sysmap.RepoState{Path: "g/orders", SHA: "abc", Branch: "main", ScannedAt: now, Status: sysmap.StatusPartial,
		Error: "e", Candidates: 3, Specs: 1, Pending: 2, Capped: 4, TooLarge: 5, Version: "v"}
	if err := s.SaveSysmapRepo(ctx, st); err != nil {
		t.Fatal(err)
	}
	st.Status, st.Pending = sysmap.StatusOK, 0
	if err := s.SaveSysmapRepo(ctx, st); err != nil { // upsert
		t.Fatal(err)
	}
	repos, err := s.ListSysmapRepos(ctx)
	if err != nil || len(repos) != 1 || !reflect.DeepEqual(repos[0], st) {
		t.Fatalf("repos = %+v, %v", repos, err)
	}

	e := sysmap.Entry{Side: sysmap.Provides, Protocol: sysmap.HTTP, Method: "POST", Path: "/orders", Raw: "x", File: "a.py", Line: 3, Source: sysmap.SourceCode}
	files := []sysmap.FileRecord{
		{Path: "a.py", Hash: "h1", Version: sysmap.CodeVersion, Entries: []sysmap.Entry{e}, ExtractedAt: now},
		{Path: "b.py", Hash: "h2", Version: sysmap.CodeVersion, Error: "bad answer", ExtractedAt: now},
	}
	if err := s.SaveSysmapFiles(ctx, "g/orders", files); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSysmapFiles(ctx, "g/web", []sysmap.FileRecord{{Path: "c.ts", Hash: "h3", Version: sysmap.CodeVersion}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListSysmapFiles(ctx, "g/orders")
	if err != nil || len(got) != 2 || !reflect.DeepEqual(got[0].Entries, []sysmap.Entry{e}) || got[1].Error != "bad answer" || len(got[1].Entries) != 0 || !got[0].ExtractedAt.Equal(now) {
		t.Fatalf("files = %+v, %v", got, err)
	}
	all, err := s.ListAllSysmapFiles(ctx)
	if err != nil || len(all) != 2 || len(all["g/web"]) != 1 {
		t.Fatalf("all = %+v, %v", all, err)
	}
	if err := s.DeleteSysmapFiles(ctx, "g/orders", []string{"b.py", "missing"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListSysmapFiles(ctx, "g/orders"); len(got) != 1 {
		t.Fatalf("after delete: %+v", got)
	}
	if err := s.DeleteSysmapRepo(ctx, "g/orders"); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.ListAllSysmapFiles(ctx); len(all) != 1 {
		t.Fatalf("repo files not deleted: %+v", all)
	}
	if repos, _ := s.ListSysmapRepos(ctx); len(repos) != 0 {
		t.Fatalf("repo not deleted: %+v", repos)
	}

	for i, ok := range []bool{true, false, true} {
		if err := s.RecordSysmapRun(ctx, sysmap.RunRecord{At: now.Add(time.Duration(-i) * 40 * time.Minute), Repo: "g/web", Files: 2, OK: ok, CostUSD: 0.5, Tokens: 100}); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := s.SysmapRunStats(ctx, now.Add(-time.Hour))
	if err != nil || stats.Runs != 2 || stats.Failed != 1 || stats.CostUSD != 1 || stats.Tokens != 200 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
	if all, _ := s.SysmapRunStats(ctx, time.Time{}); all.Runs != 3 {
		t.Fatalf("all runs = %+v", all)
	}
	if empty, err := s.SysmapRunStats(ctx, now.Add(time.Hour)); err != nil || empty.Runs != 0 || empty.CostUSD != 0 {
		t.Fatalf("empty stats = %+v, %v", empty, err)
	}
	// Budgets count per kind; the cost stats cover every kind.
	if err := s.RecordSysmapRun(ctx, sysmap.RunRecord{At: now, Repo: "g/web", OK: true, Kind: sysmap.RunKindDeps}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SysmapRunCount(ctx, now.Add(-time.Hour), sysmap.RunKindScan); err != nil || n != 2 {
		t.Fatalf("scan runs = %d, %v", n, err)
	}
	if n, err := s.SysmapRunCount(ctx, now.Add(-time.Hour), sysmap.RunKindDeps); err != nil || n != 1 {
		t.Fatalf("deps runs = %d, %v", n, err)
	}
	if all, _ := s.SysmapRunStats(ctx, now.Add(-time.Hour)); all.Runs != 3 {
		t.Fatalf("stats with deps = %+v", all)
	}
}

func TestSysmapCacheByHash(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	e := sysmap.Entry{Side: sysmap.Consumes, Protocol: sysmap.HTTP, Method: "GET", Path: "/orders", File: "a.ts", Line: 2, Source: sysmap.SourceCode}
	if err := s.SaveSysmapFiles(ctx, "g/web", []sysmap.FileRecord{
		{Path: "a.ts", Hash: "main-hash", Version: sysmap.CodeVersion, Entries: []sysmap.Entry{e}, ExtractedAt: now},
		{Path: "old.ts", Hash: "old-hash", Version: "code-0", Entries: []sysmap.Entry{e}, ExtractedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSysmapCache(ctx, []sysmap.FileRecord{
		{Hash: "mr-hash", Version: sysmap.CodeVersion, Entries: []sysmap.Entry{e}, ExtractedAt: now},
		{Hash: "bad-hash", Version: sysmap.CodeVersion, Error: "unusable", ExtractedAt: now},
		{Hash: "stale", Version: sysmap.CodeVersion, ExtractedAt: now.Add(-40 * 24 * time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.SysmapEntriesByHash(ctx, sysmap.CodeVersion, []string{"main-hash", "mr-hash", "bad-hash", "old-hash", "stale", "none"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || len(got["main-hash"].Entries) != 1 || len(got["mr-hash"].Entries) != 1 || got["bad-hash"].Error != "unusable" {
		t.Errorf("by hash = %+v", got)
	}
	if got["mr-hash"].Entries[0].Path != "/orders" {
		t.Errorf("entries = %+v", got["mr-hash"].Entries)
	}
}
