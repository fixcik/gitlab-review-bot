package sysmap

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// memStore is an in-memory Store.
type memStore struct {
	mu    sync.Mutex
	repos map[string]RepoState
	files map[string]map[string]FileRecord
	runs  []RunRecord
	cache map[string]FileRecord // version|hash
}

func newMemStore() *memStore {
	return &memStore{repos: map[string]RepoState{}, files: map[string]map[string]FileRecord{}}
}

func (s *memStore) ListSysmapRepos(context.Context) ([]RepoState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []RepoState
	for _, r := range s.repos {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
func (s *memStore) SaveSysmapRepo(_ context.Context, r RepoState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos[r.Path] = r
	return nil
}
func (s *memStore) DeleteSysmapRepo(_ context.Context, repo string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.repos, repo)
	delete(s.files, repo)
	return nil
}
func (s *memStore) ListSysmapFiles(_ context.Context, repo string) ([]FileRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []FileRecord
	for _, f := range s.files[repo] {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
func (s *memStore) ListAllSysmapFiles(ctx context.Context) (map[string][]FileRecord, error) {
	out := map[string][]FileRecord{}
	s.mu.Lock()
	var repos []string
	for r := range s.files {
		repos = append(repos, r)
	}
	s.mu.Unlock()
	for _, r := range repos {
		fs, _ := s.ListSysmapFiles(ctx, r)
		out[r] = fs
	}
	return out, nil
}
func (s *memStore) SaveSysmapFiles(_ context.Context, repo string, files []FileRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files[repo] == nil {
		s.files[repo] = map[string]FileRecord{}
	}
	for _, f := range files {
		s.files[repo][f.Path] = f
	}
	return nil
}
func (s *memStore) DeleteSysmapFiles(_ context.Context, repo string, paths []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range paths {
		delete(s.files[repo], p)
	}
	return nil
}
func (s *memStore) RecordSysmapRun(_ context.Context, r RunRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs = append(s.runs, r)
	return nil
}
func (s *memStore) SysmapRunCount(_ context.Context, since time.Time, kind string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.runs {
		if r.Kind == kind && !r.At.Before(since) {
			n++
		}
	}
	return n, nil
}
func (s *memStore) SysmapRunStats(_ context.Context, since time.Time) (RunStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var st RunStats
	for _, r := range s.runs {
		if !r.At.Before(since) {
			st.Runs++
			if !r.OK {
				st.Failed++
			}
			st.CostUSD += r.CostUSD
			st.Tokens += r.Tokens
		}
	}
	return st, nil
}

func (s *memStore) SysmapEntriesByHash(_ context.Context, version string, hashes []string) (map[string]FileRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := map[string]bool{}
	for _, h := range hashes {
		want[h] = true
	}
	out := map[string]FileRecord{}
	for _, fs := range s.files {
		for _, f := range fs {
			if f.Version == version && want[f.Hash] {
				f.Path = ""
				out[f.Hash] = f
			}
		}
	}
	for k, f := range s.cache {
		if strings.HasPrefix(k, version+"|") && want[f.Hash] {
			if _, ok := out[f.Hash]; !ok {
				out[f.Hash] = f
			}
		}
	}
	return out, nil
}
func (s *memStore) SaveSysmapCache(_ context.Context, recs []FileRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		s.cache = map[string]FileRecord{}
	}
	for _, f := range recs {
		f.Path = ""
		s.cache[f.Version+"|"+f.Hash] = f
	}
	return nil
}

// fakeSnapshots serves directories as snapshots and counts releases.
type fakeSnapshots struct {
	mu       sync.Mutex
	dirs     []core.ContextDir
	released int
	acquired int
}

func (f *fakeSnapshots) Acquire(_ context.Context, req core.ContextRequest) ([]core.ContextDir, []core.ContextMissing, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquired++
	var out []core.ContextDir
	for _, d := range f.dirs {
		if req.Exclude != nil && req.Exclude(d.Name) {
			continue
		}
		out = append(out, d)
	}
	return out, nil, func() { f.mu.Lock(); f.released++; f.mu.Unlock() }
}

// extractRunner answers extraction runs: for every file in the prompt it
// returns the entries of answer(file, content); it records the files sent.
type extractRunner struct {
	mu      sync.Mutex
	sent    [][]string // files per run
	reqs    []core.RunRequest
	answer  func(file, content string) []rawEntry
	err     error
	output  string // raw output override
	onRun   func()
	ctxKind []string
}

var fileBlockRe = regexp.MustCompile(`(?s)<untrusted_file path="([^"]+)">\n(.*?)</untrusted_file>`)

func (r *extractRunner) Exec(context.Context, core.ExecRequest) (*core.ExecResult, error) {
	return nil, fmt.Errorf("no exec")
}
func (r *extractRunner) Run(ctx context.Context, req core.RunRequest) (*core.RunResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
	if k, ok := ctx.Value(kindKey{}).(string); ok {
		r.ctxKind = append(r.ctxKind, k)
	}
	if r.onRun != nil {
		r.onRun()
	}
	var files []string
	var entries []rawEntry
	for _, m := range fileBlockRe.FindAllStringSubmatch(req.Prompt, -1) {
		files = append(files, m[1])
		if r.answer != nil {
			entries = append(entries, r.answer(m[1], m[2])...)
		}
	}
	r.sent = append(r.sent, files)
	if r.err != nil {
		return nil, r.err
	}
	out := r.output
	if out == "" {
		if entries == nil {
			entries = []rawEntry{}
		}
		b, _ := json.Marshal(map[string]any{"entries": entries})
		out = string(b)
	}
	return &core.RunResult{Output: out, CostUSD: 0.01, Usage: core.TokenUsage{Input: 100, Output: 10}}, nil
}

func (r *extractRunner) sentFiles() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, fs := range r.sent {
		out = append(out, fs...)
	}
	sort.Strings(out)
	return out
}

type kindKey struct{}

// writeTree creates files under dir.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// lineOfText returns the 1-based line of the first line containing sub
// in a numbered file block ("N| ...").
func lineOfText(content, sub string) int {
	for i, l := range strings.Split(content, "\n") {
		if strings.Contains(l, sub) {
			return i + 1
		}
	}
	return 0
}
