package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/diag"
	"github.com/fixcik/gitlab-review-bot/internal/store"
)

func seedRuns(t *testing.T) (dataDir string, ids []string) {
	t.Helper()
	dataDir = t.TempDir()
	st, err := store.OpenSQLite(filepath.Join(dataDir, "reviewbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rec, err := diag.New(diag.Options{Root: filepath.Join(dataDir, "diag"), Index: st})
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range []error{nil, errors.New("checkout: exit status 128")} {
		_ = rec.Handler(func(ctx context.Context, j core.Job) error {
			ids = append(ids, diag.FromContext(ctx).ID())
			end := diag.Step(ctx, "prepare")
			end(diag.StepOK, diag.Fields{"files": 3})
			return res
		})(context.Background(), core.Job{Kind: core.JobAutoReview, ProjectPath: "g/api", MRIID: 5, EnqueuedAt: time.Now().Add(-time.Second)})
	}
	return dataDir, ids
}

func TestRunsCommand(t *testing.T) {
	dataDir, ids := seedRuns(t)
	var out bytes.Buffer
	if err := runRuns(context.Background(), []string{"-data-dir", dataDir}, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.HasPrefix(s, "ID ") || !strings.Contains(s, ids[0]) || !strings.Contains(s, ids[1]) || !strings.Contains(s, "g/api!5") ||
		!strings.Contains(s, "failed") || !strings.Contains(s, "checkout: exit status 128") {
		t.Fatalf("list:\n%s", s)
	}
	if strings.Index(s, ids[1]) > strings.Index(s, ids[0]) {
		t.Fatalf("not newest first:\n%s", s)
	}

	out.Reset()
	if err := runRuns(context.Background(), []string{"-data-dir", dataDir, "-status", "failed", "-json"}, &out); err != nil {
		t.Fatal(err)
	}
	var rows []diag.IndexRow
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0].ID != ids[1] {
		t.Fatalf("json: %v %s", err, out.String())
	}

	out.Reset()
	if err := runRuns(context.Background(), []string{"-data-dir", dataDir, "-since", "1ns"}, &out); err != nil || strings.TrimSpace(out.String()) != "no runs" {
		t.Fatalf("empty window: %v %q", err, out.String())
	}

	out.Reset()
	if err := runRuns(context.Background(), []string{"show", ids[1], "-data-dir", dataDir}, &out); err != nil {
		t.Fatal(err)
	}
	s = out.String()
	for _, want := range []string{"Run " + ids[1], "auto_review", "failed", "MR:       g/api!5", "Error:    checkout: exit status 128", "Timeline:", "queue", "prepare"} {
		if !strings.Contains(s, want) {
			t.Errorf("show: missing %q in\n%s", want, s)
		}
	}
	out.Reset()
	if err := runRuns(context.Background(), []string{"show", "-data-dir", dataDir, "-json", ids[0]}, &out); err != nil || !strings.Contains(out.String(), `"steps"`) {
		t.Fatalf("show json: %v %s", err, out.String())
	}
	if err := runRuns(context.Background(), []string{"show", "20200101-000000-00000000", "-data-dir", dataDir}, &out); err == nil {
		t.Fatal("unknown id: no error")
	}
	if err := runRuns(context.Background(), []string{"-data-dir", filepath.Join(dataDir, "nope")}, &out); err == nil {
		t.Fatal("missing db: no error")
	}
	// -config reads data_dir from the file.
	cfg := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(cfg, []byte("data_dir: "+dataDir+"\ndiag:\n  retention: 1h\n"), 0o600)
	out.Reset()
	if err := runRuns(context.Background(), []string{"-config", cfg}, &out); err != nil || !strings.Contains(out.String(), ids[0]) {
		t.Fatalf("config: %v %s", err, out.String())
	}
}
