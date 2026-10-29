package hooksync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
)

const (
	botURL = "https://reviewbot.internal/webhook"
	secret = "super-secret-token"
)

type call struct {
	Op        string // add, edit
	ProjectID int64
	HookID    int64
	Spec      gitlab.HookSpec
}

type fakeAPI struct {
	mu        sync.Mutex
	groups    map[string][]gitlab.Project
	groupErr  map[string]error
	hooks     map[int64][]gitlab.Hook
	listErr   map[int64]error
	addErr    map[int64]error
	editErr   map[int64]error
	calls     []call
	listed    []int64
	nextID    int64
	onListGrp func(group string)
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		groups:   map[string][]gitlab.Project{},
		groupErr: map[string]error{},
		hooks:    map[int64][]gitlab.Hook{},
		listErr:  map[int64]error{},
		addErr:   map[int64]error{},
		editErr:  map[int64]error{},
		nextID:   1000,
	}
}

func (f *fakeAPI) ListGroupProjects(ctx context.Context, g string) ([]gitlab.Project, error) {
	if f.onListGrp != nil {
		f.onListGrp(g)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.groupErr[g]; err != nil {
		return nil, err
	}
	ps, ok := f.groups[g]
	if !ok {
		return nil, &gitlab.Error{Method: "GET", Path: "/groups/" + g + "/projects", StatusCode: http.StatusNotFound}
	}
	return append([]gitlab.Project(nil), ps...), nil
}

func (f *fakeAPI) ListProjectHooks(ctx context.Context, pid int64) ([]gitlab.Hook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed = append(f.listed, pid)
	if err := f.listErr[pid]; err != nil {
		return nil, err
	}
	return append([]gitlab.Hook(nil), f.hooks[pid]...), nil
}

func (f *fakeAPI) AddProjectHook(ctx context.Context, pid int64, s gitlab.HookSpec) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{Op: "add", ProjectID: pid, Spec: s})
	if err := f.addErr[pid]; err != nil {
		return 0, err
	}
	f.nextID++
	f.hooks[pid] = append(f.hooks[pid], gitlab.Hook{
		ID: f.nextID, URL: s.URL, MergeRequestsEvents: s.MergeRequestsEvents,
		NoteEvents: s.NoteEvents, PipelineEvents: s.PipelineEvents, EnableSSLVerification: s.EnableSSLVerification,
	})
	return f.nextID, nil
}

func (f *fakeAPI) EditProjectHook(ctx context.Context, pid, hid int64, s gitlab.HookSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{Op: "edit", ProjectID: pid, HookID: hid, Spec: s})
	if err := f.editErr[pid]; err != nil {
		return err
	}
	for i, h := range f.hooks[pid] {
		if h.ID == hid {
			f.hooks[pid][i] = gitlab.Hook{
				ID: hid, URL: s.URL, MergeRequestsEvents: s.MergeRequestsEvents,
				NoteEvents: s.NoteEvents, PipelineEvents: s.PipelineEvents, EnableSSLVerification: s.EnableSSLVerification,
			}
		}
	}
	return nil
}

func (f *fakeAPI) getCalls() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

func (f *fakeAPI) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.listed = nil, nil
}

func httpErr(code int) error {
	return &gitlab.Error{Method: "GET", Path: "/x", StatusCode: code, Body: http.StatusText(code)}
}

func goodHook(id int64, url string) gitlab.Hook {
	return gitlab.Hook{ID: id, URL: url, MergeRequestsEvents: true, NoteEvents: true, EnableSSLVerification: true}
}

func newSyncer(api API, groups []string, opts ...func(*Options)) (*Syncer, *bytes.Buffer) {
	var buf bytes.Buffer
	o := Options{
		API:       api,
		Groups:    groups,
		URL:       botURL,
		Secret:    secret,
		SSLVerify: true,
		Logger:    slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	for _, f := range opts {
		f(&o)
	}
	return New(o), &buf
}

func wantSpec(t *testing.T, s gitlab.HookSpec, ssl bool) {
	t.Helper()
	if s.URL != botURL || s.Token != secret || !s.MergeRequestsEvents || !s.NoteEvents || s.EnableSSLVerification != ssl {
		t.Errorf("unexpected spec: url=%q mr=%v note=%v ssl=%v tokenOK=%v",
			s.URL, s.MergeRequestsEvents, s.NoteEvents, s.EnableSSLVerification, s.Token == secret)
	}
}

func TestSyncCreatesUpdatesAndKeeps(t *testing.T) {
	api := newFakeAPI()
	api.groups["backend"] = []gitlab.Project{
		{ID: 1, PathWithNamespace: "backend/missing"},
		{ID: 2, PathWithNamespace: "backend/ok"},
		{ID: 3, PathWithNamespace: "backend/no-notes"},
		{ID: 4, PathWithNamespace: "backend/ssl-off"},
		{ID: 5, PathWithNamespace: "backend/old", Archived: true},
		{ID: 6, PathWithNamespace: "backend/no-mr"},
	}
	api.hooks[1] = []gitlab.Hook{{ID: 10, URL: "https://ci.example/hook", PushEvents: true}}
	api.hooks[2] = []gitlab.Hook{goodHook(20, botURL)}
	api.hooks[3] = []gitlab.Hook{{ID: 30, URL: botURL, MergeRequestsEvents: true, EnableSSLVerification: true}}
	api.hooks[4] = []gitlab.Hook{{ID: 40, URL: botURL, MergeRequestsEvents: true, NoteEvents: true}}
	api.hooks[6] = []gitlab.Hook{{ID: 60, URL: botURL, NoteEvents: true, EnableSSLVerification: true}}

	s, _ := newSyncer(api, []string{"backend"})
	rep, err := s.Sync(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	want := Report{Projects: 5, Created: 1, Updated: 3, Unchanged: 1}
	if rep.Projects != want.Projects || rep.Created != want.Created || rep.Updated != want.Updated ||
		rep.Unchanged != want.Unchanged || rep.Skipped != 0 || rep.Failed != 0 || len(rep.Errors) != 0 {
		t.Fatalf("report %+v", rep)
	}
	calls := api.getCalls()
	got := map[string]call{}
	for _, c := range calls {
		got[fmt.Sprintf("%s:%d", c.Op, c.ProjectID)] = c
		wantSpec(t, c.Spec, true)
	}
	for _, k := range []string{"add:1", "edit:3", "edit:4", "edit:6"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing call %s in %+v", k, calls)
		}
	}
	if len(calls) != 4 {
		t.Errorf("calls: %+v", calls)
	}
	if got["edit:3"].HookID != 30 || got["edit:4"].HookID != 40 || got["edit:6"].HookID != 60 {
		t.Errorf("wrong hook ids: %+v", calls)
	}
	for _, pid := range api.listed {
		if pid == 5 {
			t.Error("archived project was touched")
		}
	}

	// Second pass: everything is in place now.
	api.resetCalls()
	rep, err = s.Sync(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Unchanged != 5 || rep.Created+rep.Updated != 0 || len(api.getCalls()) != 0 {
		t.Fatalf("second pass not idempotent: %+v %+v", rep, api.getCalls())
	}
}

func TestSyncResetTokensEditsCorrectHooks(t *testing.T) {
	api := newFakeAPI()
	api.groups["g"] = []gitlab.Project{{ID: 1, PathWithNamespace: "g/a"}, {ID: 2, PathWithNamespace: "g/b"}}
	api.hooks[1] = []gitlab.Hook{goodHook(11, botURL)}

	s, _ := newSyncer(api, []string{"g"})
	rep, err := s.Sync(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Updated != 1 || rep.Created != 1 || rep.Unchanged != 0 {
		t.Fatalf("report %+v", rep)
	}
	calls := api.getCalls()
	if len(calls) != 2 {
		t.Fatalf("calls %+v", calls)
	}
	for _, c := range calls {
		wantSpec(t, c.Spec, true)
		if c.Op == "edit" && (c.ProjectID != 1 || c.HookID != 11) {
			t.Errorf("bad edit %+v", c)
		}
	}
}

func TestSyncSSLVerifyOption(t *testing.T) {
	api := newFakeAPI()
	api.groups["g"] = []gitlab.Project{{ID: 1, PathWithNamespace: "g/a"}, {ID: 2, PathWithNamespace: "g/b"}}
	api.hooks[1] = []gitlab.Hook{{ID: 11, URL: botURL, MergeRequestsEvents: true, NoteEvents: true}} // ssl off: matches
	api.hooks[2] = []gitlab.Hook{goodHook(22, botURL)}                                               // ssl on: drift

	s, _ := newSyncer(api, []string{"g"}, func(o *Options) { o.SSLVerify = false })
	rep, err := s.Sync(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Unchanged != 1 || rep.Updated != 1 {
		t.Fatalf("report %+v", rep)
	}
	c := api.getCalls()
	if len(c) != 1 || c[0].ProjectID != 2 {
		t.Fatalf("calls %+v", c)
	}
	wantSpec(t, c[0].Spec, false)
}

func TestSyncURLMatching(t *testing.T) {
	variants := []string{
		botURL,
		botURL + "/",
		"HTTPS://ReviewBot.Internal/webhook",
		"https://REVIEWBOT.internal/webhook//",
		" https://reviewbot.internal/webhook ",
	}
	for i, v := range variants {
		t.Run(v, func(t *testing.T) {
			api := newFakeAPI()
			api.groups["g"] = []gitlab.Project{{ID: 1, PathWithNamespace: "g/a"}}
			api.hooks[1] = []gitlab.Hook{goodHook(int64(i+1), v)}
			s, _ := newSyncer(api, []string{"g"})
			rep, err := s.Sync(context.Background(), false)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Unchanged != 1 || len(api.getCalls()) != 0 {
				t.Fatalf("variant %q not matched: %+v %+v", v, rep, api.getCalls())
			}
		})
	}
	// Different path, port, scheme or path case are different hooks.
	for _, v := range []string{
		"https://reviewbot.internal/other",
		"https://reviewbot.internal:8443/webhook",
		"http://reviewbot.internal/webhook",
		"https://reviewbot.internal/Webhook",
	} {
		t.Run("nomatch "+v, func(t *testing.T) {
			api := newFakeAPI()
			api.groups["g"] = []gitlab.Project{{ID: 1, PathWithNamespace: "g/a"}}
			api.hooks[1] = []gitlab.Hook{goodHook(5, v)}
			s, _ := newSyncer(api, []string{"g"})
			rep, err := s.Sync(context.Background(), false)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Created != 1 {
				t.Fatalf("%q should not match: %+v", v, rep)
			}
		})
	}
	// Configured URL with trailing slash / upper-case host also matches.
	api := newFakeAPI()
	api.groups["g"] = []gitlab.Project{{ID: 1, PathWithNamespace: "g/a"}}
	api.hooks[1] = []gitlab.Hook{goodHook(5, botURL)}
	s, _ := newSyncer(api, []string{"g"}, func(o *Options) { o.URL = "https://ReviewBot.internal/webhook/" })
	if rep, _ := s.Sync(context.Background(), false); rep.Unchanged != 1 {
		t.Fatalf("configured URL normalization: %+v", rep)
	}
}

func TestSyncDuplicateHooksKeepsFirst(t *testing.T) {
	api := newFakeAPI()
	api.groups["g"] = []gitlab.Project{{ID: 1, PathWithNamespace: "g/a"}}
	api.hooks[1] = []gitlab.Hook{
		{ID: 7, URL: botURL, MergeRequestsEvents: true}, // first, drifted
		goodHook(8, botURL+"/"),
	}
	s, logs := newSyncer(api, []string{"g"})
	rep, err := s.Sync(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	c := api.getCalls()
	if rep.Updated != 1 || len(c) != 1 || c[0].Op != "edit" || c[0].HookID != 7 {
		t.Fatalf("report %+v calls %+v", rep, c)
	}
	if !strings.Contains(logs.String(), "several hooks") {
		t.Errorf("no duplicate warning in logs:\n%s", logs.String())
	}
}

func TestSyncDedupAcrossGroups(t *testing.T) {
	api := newFakeAPI()
	api.groups["platform"] = []gitlab.Project{
		{ID: 1, PathWithNamespace: "platform/a"},
		{ID: 2, PathWithNamespace: "platform/services/b"},
	}
	api.groups["platform/services"] = []gitlab.Project{{ID: 2, PathWithNamespace: "platform/services/b"}}
	s, _ := newSyncer(api, []string{"platform", " /platform/services/ ", "PLATFORM"})
	rep, err := s.Sync(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Projects != 2 || rep.Created != 2 || len(api.getCalls()) != 2 {
		t.Fatalf("report %+v calls %+v", rep, api.getCalls())
	}
}

func TestSyncSkipsAndFailures(t *testing.T) {
	api := newFakeAPI()
	api.groups["g"] = []gitlab.Project{
		{ID: 1, PathWithNamespace: "g/forbidden-list"},
		{ID: 2, PathWithNamespace: "g/notfound-list"},
		{ID: 3, PathWithNamespace: "g/forbidden-add"},
		{ID: 4, PathWithNamespace: "g/broken-list"},
		{ID: 5, PathWithNamespace: "g/broken-edit"},
		{ID: 6, PathWithNamespace: "g/fine"},
		{ID: 7, PathWithNamespace: "g/broken-add"},
	}
	api.listErr[1] = httpErr(http.StatusForbidden)
	api.listErr[2] = httpErr(http.StatusNotFound)
	api.addErr[3] = httpErr(http.StatusForbidden)
	api.listErr[4] = httpErr(http.StatusInternalServerError)
	api.hooks[5] = []gitlab.Hook{{ID: 50, URL: botURL}}
	api.editErr[5] = httpErr(http.StatusUnprocessableEntity)
	api.addErr[7] = errors.New("connection reset")

	s, logs := newSyncer(api, []string{"g"})
	rep, err := s.Sync(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Projects != 7 || rep.Skipped != 3 || rep.Failed != 3 || rep.Created != 1 || rep.Updated != 0 || rep.Unchanged != 0 {
		t.Fatalf("report %+v", rep)
	}
	if len(rep.Errors) != 6 {
		t.Fatalf("errors %q", rep.Errors)
	}
	joined := strings.Join(rep.Errors, "\n")
	for _, p := range []string{"g/forbidden-list", "g/notfound-list", "g/forbidden-add", "g/broken-list", "g/broken-edit", "g/broken-add"} {
		if !strings.Contains(joined, p) {
			t.Errorf("errors lack %s: %s", p, joined)
		}
	}
	if !strings.Contains(logs.String(), "Maintainer") {
		t.Errorf("no Maintainer warning logged")
	}
	if strings.Contains(logs.String(), secret) || strings.Contains(joined, secret) {
		t.Error("secret leaked into logs or report")
	}
}

func TestSyncGroupErrors(t *testing.T) {
	api := newFakeAPI()
	api.groups["ok"] = []gitlab.Project{{ID: 1, PathWithNamespace: "ok/a"}}
	api.groupErr["bad"] = httpErr(http.StatusInternalServerError)

	// One group fails: not an error, but counted and reported.
	s, _ := newSyncer(api, []string{"bad", "ok", "missing"})
	rep, err := s.Sync(context.Background(), false)
	if err != nil {
		t.Fatalf("partial failure must not be an error: %v", err)
	}
	if rep.Projects != 1 || rep.Created != 1 || rep.Failed != 2 || len(rep.Errors) != 2 {
		t.Fatalf("report %+v", rep)
	}

	// Every group fails: error.
	s, _ = newSyncer(api, []string{"bad", "missing"})
	rep, err = s.Sync(context.Background(), false)
	if err == nil {
		t.Fatal("expected error when every group fails")
	}
	if rep.Failed != 2 || rep.Projects != 0 {
		t.Fatalf("report %+v", rep)
	}
}

func TestSyncDryRun(t *testing.T) {
	api := newFakeAPI()
	api.groups["g"] = []gitlab.Project{
		{ID: 1, PathWithNamespace: "g/missing"},
		{ID: 2, PathWithNamespace: "g/drift"},
		{ID: 3, PathWithNamespace: "g/ok"},
		{ID: 4, PathWithNamespace: "g/forbidden"},
	}
	api.hooks[2] = []gitlab.Hook{{ID: 20, URL: botURL}}
	api.hooks[3] = []gitlab.Hook{goodHook(30, botURL)}
	api.listErr[4] = httpErr(http.StatusForbidden)

	s, logs := newSyncer(api, []string{"g"}, func(o *Options) { o.DryRun = true })
	rep, err := s.Sync(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Created != 1 || rep.Updated != 1 || rep.Unchanged != 1 || rep.Skipped != 1 {
		t.Fatalf("report %+v", rep)
	}
	if c := api.getCalls(); len(c) != 0 {
		t.Fatalf("dry run wrote: %+v", c)
	}
	if !strings.Contains(logs.String(), "would create") || !strings.Contains(logs.String(), "would update") {
		t.Errorf("dry-run logs:\n%s", logs.String())
	}

	// With resetTokens the correct hook would be updated too.
	rep, _ = s.Sync(context.Background(), true)
	if rep.Updated != 2 || rep.Unchanged != 0 || len(api.getCalls()) != 0 {
		t.Fatalf("dry run reset: %+v %+v", rep, api.getCalls())
	}
}

func TestSyncInvalidOptions(t *testing.T) {
	api := newFakeAPI()
	if _, err := New(Options{Groups: []string{"g"}, URL: botURL}).Sync(context.Background(), false); err == nil {
		t.Error("nil API accepted")
	}
	if _, err := New(Options{API: api, Groups: []string{"g"}}).Sync(context.Background(), false); err == nil {
		t.Error("empty URL accepted")
	}
	if _, err := New(Options{API: api, Groups: []string{" ", "/"}, URL: botURL}).Sync(context.Background(), false); err == nil {
		t.Error("no groups accepted")
	}
}

func TestSyncContextCancelled(t *testing.T) {
	api := newFakeAPI()
	api.groups["a"] = []gitlab.Project{{ID: 1, PathWithNamespace: "a/x"}}
	api.groups["b"] = []gitlab.Project{{ID: 2, PathWithNamespace: "b/y"}}
	ctx, cancel := context.WithCancel(context.Background())
	api.onListGrp = func(g string) {
		if g == "a" {
			cancel()
		}
	}
	s, _ := newSyncer(api, []string{"a", "b"})
	_, err := s.Sync(ctx, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if len(api.getCalls()) != 0 {
		t.Errorf("writes after cancel: %+v", api.getCalls())
	}
}

// TestRun observes resetTokens via edits of an already-correct hook.
func TestRun(t *testing.T) {
	api := newFakeAPI()
	api.groups["g"] = []gitlab.Project{{ID: 1, PathWithNamespace: "g/a"}}
	api.hooks[1] = []gitlab.Hook{goodHook(11, botURL)}

	passes := make(chan string, 16)
	api.onListGrp = func(string) {
		select {
		case passes <- "pass":
		default:
		}
	}

	s, logs := newSyncer(api, []string{"g"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx, 20*time.Millisecond); close(done) }()

	for i := 0; i < 3; i++ {
		select {
		case <-passes:
		case <-time.After(5 * time.Second):
			t.Fatal("sync pass did not happen")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}

	// Only the first pass resets tokens, so the correct hook was edited once.
	edits := 0
	for _, c := range api.getCalls() {
		if c.Op == "edit" {
			edits++
		}
		if c.Op == "add" {
			t.Errorf("unexpected add %+v", c)
		}
	}
	if edits != 1 {
		t.Errorf("edits = %d, want 1 (resetTokens only on first pass)", edits)
	}
	if !strings.Contains(logs.String(), "sync finished") {
		t.Errorf("report not logged:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), secret) {
		t.Error("secret leaked into logs")
	}
}

func TestURLKey(t *testing.T) {
	cases := map[string]string{
		"HTTPS://Host.Example/a/b/": "https://host.example/a/b",
		"https://host.example":      "https://host.example",
		"https://host.example/":     "https://host.example",
		"https://h/x?q=1":           "https://h/x?q=1",
		"https://h/x#frag":          "https://h/x",
		"not a url/":                "not a url",
	}
	keys := make([]string, 0, len(cases))
	for k := range cases {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, in := range keys {
		if got := urlKey(in); got != cases[in] {
			t.Errorf("urlKey(%q) = %q, want %q", in, got, cases[in])
		}
	}
}

func TestSyncSkipsExcluded(t *testing.T) {
	api := newFakeAPI()
	api.groups["team"] = []gitlab.Project{{ID: 1, PathWithNamespace: "team/api"}, {ID: 2, PathWithNamespace: "team/legacy"}}
	s := New(Options{API: api, Groups: []string{"team"}, URL: botURL, Secret: secret,
		Excluded: func(p string) bool { return p == "team/legacy" }})
	rep, err := s.Sync(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Projects != 2 || rep.Excluded != 1 || rep.Created != 1 {
		t.Fatalf("report %+v", rep)
	}
	for _, c := range api.calls {
		if c.ProjectID == 2 {
			t.Error("hook touched in an excluded project")
		}
	}
	for _, pid := range api.listed {
		if pid == 2 {
			t.Error("excluded project hooks must not even be listed")
		}
	}
}

// review.ci: the hooks are subscribed to pipeline events; a hook without
// them (or with them while the feature is off) is updated.
func TestSyncPipelineEvents(t *testing.T) {
	api := newFakeAPI()
	api.groups["g"] = []gitlab.Project{{ID: 1, PathWithNamespace: "g/a"}, {ID: 2, PathWithNamespace: "g/b"}, {ID: 3, PathWithNamespace: "g/c"}}
	api.hooks[1] = []gitlab.Hook{goodHook(11, botURL)} // no pipeline events: drift
	h := goodHook(22, botURL)
	h.PipelineEvents = true
	api.hooks[2] = []gitlab.Hook{h} // up to date

	s, _ := newSyncer(api, []string{"g"}, func(o *Options) { o.PipelineEvents = true })
	rep, err := s.Sync(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Created != 1 || rep.Updated != 1 || rep.Unchanged != 1 {
		t.Fatalf("report %+v", rep)
	}
	for _, c := range api.getCalls() {
		if !c.Spec.PipelineEvents {
			t.Errorf("%s %d without pipeline events", c.Op, c.ProjectID)
		}
		wantSpec(t, c.Spec, true)
	}
	// Feature off: the subscription is removed again.
	api.resetCalls()
	s, _ = newSyncer(api, []string{"g"})
	rep, err = s.Sync(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Updated != 3 {
		t.Fatalf("off: %+v", rep)
	}
	for _, c := range api.getCalls() {
		if c.Spec.PipelineEvents {
			t.Errorf("%d still subscribed", c.ProjectID)
		}
	}
}
