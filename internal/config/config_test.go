package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadServiceDefaultsAndEnv(t *testing.T) {
	t.Setenv("GITLAB_URL", "https://gitlab.example.com/")
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	cfg, err := LoadService("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitLab.URL != "https://gitlab.example.com" {
		t.Errorf("url not trimmed: %q", cfg.GitLab.URL)
	}
	if cfg.Limits.GlobalRunsPerHour != 15 || cfg.Limits.MRRerunCooldown.D() != 10*time.Minute {
		t.Errorf("defaults not applied: %+v", cfg.Limits)
	}
}

func TestLoadServiceFile(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("gitlab:\n  url: https://g.example\nlimits:\n  user_reruns_per_hour: 2\n  mr_rerun_cooldown: 3m\nsandbox:\n  mode: local\n"), 0o600)
	cfg, err := LoadService(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.UserRerunsPerHour != 2 || cfg.Limits.MRRerunCooldown.D() != 3*time.Minute || cfg.Sandbox.Mode != "local" {
		t.Errorf("file values not applied: %+v %+v", cfg.Limits, cfg.Sandbox)
	}
	if cfg.Limits.GlobalRunsPerHour != 15 {
		t.Errorf("unset field lost its default: %d", cfg.Limits.GlobalRunsPerHour)
	}
}

func TestLoadServiceValidation(t *testing.T) {
	t.Setenv("GITLAB_URL", "")
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "")
	if _, err := LoadService(""); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestMergeRepoOnlyStricter(t *testing.T) {
	svc := Defaults()
	f := false
	repo, err := ParseRepo([]byte(`
auto_review: false
language: en
ignore: ["docs/**"]
limits:
  user_reruns_per_hour: 50
  user_chat_per_hour: 3
  mr_rerun_cooldown: 1m
`))
	if err != nil {
		t.Fatal(err)
	}
	e := Merge(&svc, repo)
	if e.AutoReview != f || e.Language != "en" {
		t.Errorf("repo overrides lost: %+v", e)
	}
	if e.Limits.UserRerunsPerHour != 5 {
		t.Errorf("repo must not raise rerun limit, got %d", e.Limits.UserRerunsPerHour)
	}
	if e.Limits.UserChatPerHour != 3 {
		t.Errorf("repo should lower chat limit, got %d", e.Limits.UserChatPerHour)
	}
	if e.Limits.MRRerunCooldown.D() != 10*time.Minute {
		t.Errorf("repo must not shorten cooldown, got %v", e.Limits.MRRerunCooldown.D())
	}
	if e.Ignore[len(e.Ignore)-1] != "docs/**" || len(e.Ignore) != len(svc.Review.DefaultIgnore)+1 {
		t.Errorf("ignore not appended: %v", e.Ignore)
	}
	ul := e.UserLimits()
	if ul.ChatPerHour != 3 || ul.MRRerunCooldown != 10*time.Minute {
		t.Errorf("UserLimits: %+v", ul)
	}
}

func TestParseRepoEmptyAndInvalid(t *testing.T) {
	r, err := ParseRepo(nil)
	if err != nil || r == nil {
		t.Fatalf("empty: %v %v", r, err)
	}
	if _, err := ParseRepo([]byte("limits: [")); err == nil {
		t.Fatal("expected parse error")
	}
	if _, err := ParseRepo([]byte("mr_rerun_cooldown: x\nlimits:\n  mr_rerun_cooldown: nope")); err == nil {
		t.Fatal("expected duration error")
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGroupsDefaults(t *testing.T) {
	g := Defaults().GitLab
	if g.SyncHooks || g.SyncInterval.D() != time.Hour || !g.HookSSLVerify || len(g.Groups) != 0 {
		t.Errorf("unexpected defaults: %+v", g)
	}
}

func TestGroupsAndSyncHooksFromFile(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	t.Setenv("GITLAB_HOOKS_TOKEN", "hooks-tok")
	p := writeConfig(t, `gitlab:
  url: https://g.example
  groups: [" backend ", "/platform/services/", "", "Backend", " / "]
  webhook_url: " https://reviewbot.internal/webhook "
  sync_hooks: true
  sync_interval: 30m
  hook_ssl_verify: false
`)
	cfg, err := LoadService(p)
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.GitLab
	if len(g.Groups) != 2 || g.Groups[0] != "backend" || g.Groups[1] != "platform/services" {
		t.Errorf("groups not normalized: %q", g.Groups)
	}
	if g.WebhookURL != "https://reviewbot.internal/webhook" {
		t.Errorf("webhook_url: %q", g.WebhookURL)
	}
	if !g.SyncHooks || g.SyncInterval.D() != 30*time.Minute || g.HookSSLVerify {
		t.Errorf("sync settings: %+v", g)
	}
}

func TestSyncHooksValidation(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	t.Setenv("GITLAB_HOOKS_TOKEN", "hooks-tok")
	cases := []struct {
		name, yaml, want string
	}{
		{"no url", "gitlab:\n  url: https://g\n  groups: [a]\n  sync_hooks: true\n", "requires gitlab.webhook_url"},
		{"no groups", "gitlab:\n  url: https://g\n  webhook_url: https://bot/webhook\n  sync_hooks: true\n", "at least one entry"},
		{"blank groups", "gitlab:\n  url: https://g\n  groups: [\" / \"]\n  webhook_url: https://bot/webhook\n  sync_hooks: true\n", "at least one entry"},
		{"bad scheme", "gitlab:\n  url: https://g\n  groups: [a]\n  webhook_url: ftp://bot/webhook\n  sync_hooks: true\n", "http(s) URL"},
		{"no host", "gitlab:\n  url: https://g\n  groups: [a]\n  webhook_url: /webhook\n  sync_hooks: true\n", "http(s) URL"},
		{"bad url without sync", "gitlab:\n  url: https://g\n  webhook_url: not a url\n", "http(s) URL"},
		{"tiny interval", "gitlab:\n  url: https://g\n  groups: [a]\n  webhook_url: https://bot/webhook\n  sync_hooks: true\n  sync_interval: 5s\n", "sync_interval"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadService(writeConfig(t, tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}

	// Groups without sync_hooks are fine (filter only, hooks managed by hand).
	if _, err := LoadService(writeConfig(t, "gitlab:\n  url: https://g\n  groups: [a]\n")); err != nil {
		t.Fatalf("groups without sync: %v", err)
	}
	// Valid sync config.
	if _, err := LoadService(writeConfig(t, "gitlab:\n  url: https://g\n  groups: [a]\n  webhook_url: http://bot:8080/webhook\n  sync_hooks: true\n")); err != nil {
		t.Fatalf("valid sync config: %v", err)
	}
}

func TestNormalizeGroups(t *testing.T) {
	got := NormalizeGroups([]string{"  a/b/ ", "A/B", "//c//", "", "  "})
	if len(got) != 2 || got[0] != "a/b" || got[1] != "c" {
		t.Errorf("got %q", got)
	}
	if NormalizeGroups(nil) != nil {
		t.Error("nil in, nil out")
	}
}

func TestRepoJiraProjectKeys(t *testing.T) {
	svc := Defaults()
	svc.Jira.ProjectKeys = []string{"PROJ"}
	if e := Merge(&svc, nil); strings.Join(e.JiraProjectKeys, ",") != "PROJ" {
		t.Errorf("service keys: %v", e.JiraProjectKeys)
	}
	repo, err := ParseRepo([]byte("jira:\n  project_keys: [\" pay \", PAY, ops2]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if e := Merge(&svc, repo); strings.Join(e.JiraProjectKeys, ",") != "PAY,OPS2" {
		t.Errorf("repo override: %v", e.JiraProjectKeys)
	}
	empty, _ := ParseRepo([]byte("jira:\n  project_keys: []\n"))
	if e := Merge(&svc, empty); strings.Join(e.JiraProjectKeys, ",") != "PROJ" {
		t.Errorf("empty repo list must keep service keys: %v", e.JiraProjectKeys)
	}
	if _, err := ParseRepo([]byte("jira:\n  project_keys: [\"bad key\"]\n")); err == nil {
		t.Error("expected invalid key error")
	}
}

func TestSyncHooksRequiresHooksToken(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	t.Setenv("GITLAB_HOOKS_TOKEN", "")
	t.Setenv("GITLAB_SERVICE_TOKEN", "")
	_, err := LoadService(writeConfig(t, "gitlab:\n  url: https://g\n  groups: [a]\n  webhook_url: https://bot/webhook\n  sync_hooks: true\n"))
	if err == nil || !strings.Contains(err.Error(), "GITLAB_SERVICE_TOKEN") {
		t.Fatalf("want hooks token error, got %v", err)
	}
}

func TestCompatConfig(t *testing.T) {
	svc := Defaults()
	if !svc.Review.CompatCheck || svc.Review.CompatTimeout.D() != 3*time.Minute {
		t.Fatalf("defaults: %+v", svc.Review)
	}
	if e := Merge(&svc, nil); !e.Compat.Enabled {
		t.Error("compat must be on by default")
	}
	repo, err := ParseRepo([]byte(`
compat:
  openapi: ["contracts/*.yml"]
  proto: ["schemas/**/*.pb.txt"]
  graphql: ["api/*.gql"]
`))
	if err != nil {
		t.Fatal(err)
	}
	e := Merge(&svc, repo)
	if !e.Compat.Enabled || e.Compat.OpenAPI[0] != "contracts/*.yml" || e.Compat.Proto[0] != "schemas/**/*.pb.txt" ||
		e.Compat.GraphQL[0] != "api/*.gql" {
		t.Errorf("repo compat: %+v", e.Compat)
	}
	off, _ := ParseRepo([]byte("compat:\n  enabled: false\n"))
	if Merge(&svc, off).Compat.Enabled {
		t.Error("repo must be able to turn compat off")
	}
	on, _ := ParseRepo([]byte("compat:\n  enabled: true\n"))
	svc.Review.CompatCheck = false
	if Merge(&svc, on).Compat.Enabled {
		t.Error("the service switch must win over the repo")
	}
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	cfg, err := LoadService(writeConfig(t, "gitlab:\n  url: https://g\nreview:\n  compat_check: false\n  compat_timeout: 90s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Review.CompatCheck || cfg.Review.CompatTimeout.D() != 90*time.Second {
		t.Errorf("file: %+v", cfg.Review)
	}
}

// sync-hooks validates only what it needs: no bot token, no other sections.
func TestLoadSyncHooksValidation(t *testing.T) {
	for _, k := range []string{"GITLAB_URL", "GITLAB_TOKEN", "GITLAB_WEBHOOK_SECRET", "GITLAB_SERVICE_TOKEN", "GITLAB_HOOKS_TOKEN"} {
		t.Setenv(k, "")
	}
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("gitlab:\n  url: https://g.example\n  groups: [team]\n  webhook_url: https://bot/webhook\nsandbox:\n  mode: bogus\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSyncHooks(path); err == nil || !strings.Contains(err.Error(), "GITLAB_SERVICE_TOKEN") ||
		!strings.Contains(err.Error(), "GITLAB_WEBHOOK_SECRET") || strings.Contains(err.Error(), "GITLAB_TOKEN is required") ||
		strings.Contains(err.Error(), "sandbox.mode") {
		t.Fatalf("LoadSyncHooks without tokens: %v", err)
	}
	t.Setenv("GITLAB_SERVICE_TOKEN", "glpat-svc")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "s")
	cfg, err := LoadSyncHooks(path)
	if err != nil {
		t.Fatalf("LoadSyncHooks: %v", err)
	}
	if cfg.GitLab.ServiceToken != "glpat-svc" || cfg.GitLab.Token != "" {
		t.Fatalf("cfg = %+v", cfg.GitLab)
	}
	// serve still requires everything.
	if _, err := LoadService(path); err == nil || !strings.Contains(err.Error(), "GITLAB_TOKEN is required") {
		t.Fatalf("LoadService: %v", err)
	}
}

// A repo can tighten a limit the service leaves unlimited (0), never loosen
// one: per-hour limits take the lower positive value, the cooldown the longer.
func TestMergeRepoLimitsTightenUnlimited(t *testing.T) {
	cases := []struct {
		name                 string
		svcReruns, svcChat   int
		svcCooldown          time.Duration
		repoReruns, repoChat int
		repoCooldown         time.Duration
		wantReruns, wantChat int
		wantCooldown         time.Duration
	}{
		{"service unlimited, repo sets", 0, 0, 0, 3, 7, 15 * time.Minute, 3, 7, 15 * time.Minute},
		{"service unlimited, repo unset", 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{"repo stricter", 5, 20, 10 * time.Minute, 2, 10, 30 * time.Minute, 2, 10, 30 * time.Minute},
		{"repo looser ignored", 5, 20, 10 * time.Minute, 50, 30, time.Minute, 5, 20, 10 * time.Minute},
		{"repo unset keeps service", 5, 20, 10 * time.Minute, 0, 0, 0, 5, 20, 10 * time.Minute},
		{"negative repo ignored", 0, 4, 0, -1, -1, 0, 0, 4, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := Defaults()
			svc.Limits.UserRerunsPerHour = c.svcReruns
			svc.Limits.UserChatPerHour = c.svcChat
			svc.Limits.MRRerunCooldown = Duration(c.svcCooldown)
			repo := &Repo{Limits: &RepoLimits{UserRerunsPerHour: c.repoReruns, UserChatPerHour: c.repoChat,
				MRRerunCooldown: Duration(c.repoCooldown)}}
			ul := Merge(&svc, repo).UserLimits()
			if ul.RerunsPerHour != c.wantReruns || ul.ChatPerHour != c.wantChat || ul.MRRerunCooldown != c.wantCooldown {
				t.Errorf("got reruns=%d chat=%d cooldown=%v, want %d %d %v",
					ul.RerunsPerHour, ul.ChatPerHour, ul.MRRerunCooldown, c.wantReruns, c.wantChat, c.wantCooldown)
			}
		})
	}
}

func TestEventsConfig(t *testing.T) {
	for _, k := range []string{"GITLAB_WEBHOOK_SECRET", "GITLAB_SERVICE_TOKEN", "GITLAB_HOOKS_TOKEN"} {
		t.Setenv(k, "")
	}
	t.Setenv("GITLAB_TOKEN", "tok")

	d := Defaults().GitLab
	if d.Events != EventsWebhook || d.PollInterval.D() != 5*time.Second || !d.UsesWebhook() || d.UsesPolling() {
		t.Fatalf("defaults: %+v", d)
	}
	// Webhook mode (default) still needs the secret.
	if _, err := LoadService(writeConfig(t, "gitlab:\n  url: https://g\n")); err == nil || !strings.Contains(err.Error(), "GITLAB_WEBHOOK_SECRET") {
		t.Fatalf("webhook without secret: %v", err)
	}
	// Poll mode needs no secret, but needs groups.
	cfg, err := LoadService(writeConfig(t, "gitlab:\n  url: https://g\n  groups: [team]\n  events: \" Poll \"\n  poll_interval: 15s\n"))
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if cfg.GitLab.Events != EventsPoll || cfg.GitLab.UsesWebhook() || !cfg.GitLab.UsesPolling() || cfg.GitLab.PollInterval.D() != 15*time.Second {
		t.Errorf("poll cfg: %+v", cfg.GitLab)
	}
	cases := []struct{ name, yaml, want string }{
		{"no groups", "gitlab:\n  url: https://g\n  events: poll\n", "requires at least one entry in gitlab.groups"},
		{"bad mode", "gitlab:\n  url: https://g\n  groups: [a]\n  events: pull\n", `"webhook", "poll" or "both"`},
		{"tiny interval", "gitlab:\n  url: https://g\n  groups: [a]\n  events: poll\n  poll_interval: 2s\n", "poll_interval"},
		{"both needs secret", "gitlab:\n  url: https://g\n  groups: [a]\n  events: both\n", "GITLAB_WEBHOOK_SECRET"},
		{"sync hooks in poll mode", "gitlab:\n  url: https://g\n  groups: [a]\n  events: poll\n  sync_hooks: true\n  webhook_url: https://bot/webhook\n", "makes no sense"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadService(writeConfig(t, tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	if cfg, err := LoadService(writeConfig(t, "gitlab:\n  url: https://g\n  groups: [a]\n  events: both\n")); err != nil || !cfg.GitLab.UsesWebhook() || !cfg.GitLab.UsesPolling() {
		t.Fatalf("both: %v", err)
	}
}

func TestRepoContextRelated(t *testing.T) {
	r, err := ParseRepo([]byte(`context:
  related:
    - repo: " /frontend/web/ "
      note: "фронтенд,
        вызывает наш API"
    - repo: Frontend/Web
    - repo: mobile/app
`))
	if err != nil {
		t.Fatal(err)
	}
	e := Merge(&Service{}, r)
	rel := e.ContextRelated()
	if len(rel) != 2 || rel[0].Repo != "frontend/web" || rel[0].Note != "фронтенд, вызывает наш API" || rel[1].Repo != "mobile/app" {
		t.Fatalf("related %+v", rel)
	}
	if Merge(&Service{}, nil).ContextRelated() != nil {
		t.Error("no repo config -> nil")
	}
	for _, bad := range []string{
		"context:\n  related:\n    - repo: web\n",
		"context:\n  related:\n    - repo: a/../b\n",
		"context:\n  related:\n    - repo: a/b.git\n",
		"context:\n  related:\n    - note: x\n",
		"context:\n  related:\n    - repo: a/b\n      note: \"" + strings.Repeat("я", MaxRelatedNoteLen+1) + "\"\n",
	} {
		if _, err := ParseRepo([]byte(bad)); err == nil || !strings.Contains(err.Error(), "context.related") {
			t.Errorf("%q: want related error, got %v", bad, err)
		}
	}
	var many strings.Builder
	many.WriteString("context:\n  related:\n")
	for i := 0; i <= MaxRelatedRepos; i++ {
		fmt.Fprintf(&many, "    - repo: g/p%d\n", i)
	}
	if _, err := ParseRepo([]byte(many.String())); err == nil {
		t.Error("too many related must fail")
	}
}

func TestTBDConfig(t *testing.T) {
	d := Defaults().Review.TBD
	if !d.Enabled || d.MaxLines != 400 || d.MaxAge.D() != 72*time.Hour || d.MaxBehind != 30 || !d.Pipeline {
		t.Fatalf("defaults %+v", d)
	}
	svc := Defaults()
	r, err := ParseRepo([]byte("tbd:\n  max_lines: 1000\n  max_age: 24h\n  max_behind: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	e := Merge(&svc, r).TBD
	if !e.Enabled || e.MaxLines != 1000 || e.MaxAge.D() != 24*time.Hour || e.MaxBehind != 0 || !e.Pipeline {
		t.Errorf("merged %+v", e)
	}
	off, _ := ParseRepo([]byte("tbd:\n  enabled: false\n"))
	if Merge(&svc, off).TBD.Enabled {
		t.Error("repo must be able to turn it off")
	}
	svc.Review.TBD.Enabled = false
	on, _ := ParseRepo([]byte("tbd:\n  enabled: true\n"))
	if Merge(&svc, on).TBD.Enabled {
		t.Error("the service switch wins")
	}
	if _, err := ParseRepo([]byte("tbd:\n  max_lines: -1\n")); err == nil {
		t.Error("negative max_lines must fail")
	}
}

func TestCoverageConfig(t *testing.T) {
	svc := Defaults()
	if !svc.Review.Coverage.Enabled || svc.Review.Coverage.MaxReportBytes() != 50<<20 {
		t.Fatalf("defaults %+v", svc.Review.Coverage)
	}
	if Merge(&svc, nil).Coverage.Enabled {
		t.Error("no repo config: off")
	}
	r, err := ParseRepo([]byte("coverage:\n  job: \" tests \"\n  path: /build/coverage.xml\n  reports:\n    - job: integration\n      path: cov/clover.xml\n  min: 80\n"))
	if err != nil {
		t.Fatal(err)
	}
	e := Merge(&svc, r).Coverage
	if !e.Enabled || len(e.Reports) != 2 || e.Reports[0] != (CoverageReport{Job: "tests", Path: "build/coverage.xml"}) ||
		e.Reports[1].Job != "integration" || e.Min != 80 {
		t.Errorf("merged %+v", e)
	}
	svc.Review.Coverage.Enabled = false
	if Merge(&svc, r).Coverage.Enabled {
		t.Error("the service switch wins")
	}
	for _, bad := range []string{"coverage:\n  job: tests\n", "coverage:\n  job: t\n  path: ../x.xml\n", "coverage:\n  job: t\n  path: a.xml\n  min: 120\n"} {
		if _, err := ParseRepo([]byte(bad)); err == nil {
			t.Errorf("%q must fail", bad)
		}
	}
}

func TestProjectExclude(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	cfg, err := LoadService(writeConfig(t, "gitlab:\n  url: https://g\n  groups: [backend]\n  exclude: [\" /backend/legacy-*/ \", \"Frontend/sandbox/**\", \"\"]\nreview:\n  context:\n    exclude: [\"secret/**\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.GitLab
	if len(g.Exclude) != 2 || !g.Excluded("backend/legacy-billing") || !g.Excluded("frontend/SANDBOX/x/y") || g.Excluded("backend/billing") || g.Excluded("") {
		t.Errorf("exclude %q", g.Exclude)
	}
	if !cfg.Review.Context.Excluded("secret/keys") || cfg.Review.Context.Excluded("backend/api") {
		t.Error("context exclude")
	}
	if _, err := LoadService(writeConfig(t, "gitlab:\n  url: https://g\n  exclude: [\"a/[\"]\n")); err == nil || !strings.Contains(err.Error(), "gitlab.exclude") {
		t.Errorf("invalid glob: %v", err)
	}
}

func TestMainBranchConfig(t *testing.T) {
	g := GitLab{}
	if g.MainBranchFor("team/api", "master") != "master" {
		t.Error("no config: GitLab default branch")
	}
	g = GitLab{MainBranch: "main", MainBranches: map[string]string{"legacy/**": "master", "legacy/billing": "release", "TEAM/old-*": "develop"}}
	for path, want := range map[string]string{"team/api": "main", "legacy/x/y": "master", "legacy/billing": "release", "team/old-svc": "develop"} {
		if got := g.MainBranchFor(path, "trunk"); got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	if _, err := LoadService(writeConfig(t, "gitlab:\n  url: https://g\n  main_branch: \"bad branch\"\n")); err == nil || !strings.Contains(err.Error(), "gitlab.main_branch") {
		t.Errorf("bad main_branch: %v", err)
	}
	if _, err := LoadService(writeConfig(t, "gitlab:\n  url: https://g\n  main_branches:\n    \"a/**\": \"../x\"\n")); err == nil || !strings.Contains(err.Error(), "gitlab.main_branches") {
		t.Errorf("bad main_branches: %v", err)
	}
}

func TestLoadUnvalidated(t *testing.T) {
	for _, k := range []string{"GITLAB_URL", "GITLAB_TOKEN", "GITLAB_WEBHOOK_SECRET", "GITLAB_SERVICE_TOKEN", "GITLAB_HOOKS_TOKEN"} {
		t.Setenv(k, "")
	}
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("gitlab:\n  url: https://g.example/\n  groups: [team]\nsandbox:\n  mode: bogus\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITLAB_TOKEN", "glpat-x")
	cfg, err := LoadUnvalidated(path)
	if err != nil {
		t.Fatalf("LoadUnvalidated: %v", err)
	}
	if cfg.GitLab.URL != "https://g.example" || cfg.GitLab.Token != "glpat-x" || cfg.Sandbox.Mode != "bogus" || cfg.GitLab.Events != EventsWebhook {
		t.Fatalf("cfg = %+v %+v", cfg.GitLab, cfg.Sandbox)
	}
	if cfg.Validate() == nil {
		t.Fatal("the config is invalid (no webhook secret, sandbox.mode): Validate must say so")
	}
	if _, err := LoadUnvalidated(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("missing file: want an error")
	}
}
