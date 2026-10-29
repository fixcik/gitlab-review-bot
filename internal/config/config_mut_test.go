package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// mutSvc is a service config that passes Validate.
func mutSvc() Service {
	s := Defaults()
	s.GitLab.URL = "https://g.example"
	s.GitLab.Token = "tok"
	s.GitLab.WebhookSecret = "sec"
	return s
}

func wantValid(t *testing.T, what string, s Service) {
	t.Helper()
	if err := s.Validate(); err != nil {
		t.Errorf("%s: must be valid, got %v", what, err)
	}
}

func wantInvalid(t *testing.T, what string, s Service, substr string) {
	t.Helper()
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), substr) {
		t.Errorf("%s: want error containing %q, got %v", what, substr, err)
	}
}

func mustRepo(t *testing.T, y string) *Repo {
	t.Helper()
	r, err := ParseRepo([]byte(y))
	if err != nil {
		t.Fatalf("ParseRepo(%q): %v", y, err)
	}
	return r
}

func TestMutSvcIsValid(t *testing.T) { wantValid(t, "defaults", mutSvc()) }

// ---------------------------------------------------------------------------
// Merge
// ---------------------------------------------------------------------------

func TestMergeDefaultsWithoutRepo(t *testing.T) {
	svc := mutSvc()
	for name, repo := range map[string]*Repo{"nil": nil, "empty": mustRepo(t, ""), "unrelated": mustRepo(t, "instructions: x\n")} {
		e := Merge(&svc, repo)
		if !e.Enabled || !e.AutoReview {
			t.Errorf("%s: the bot must be on by default: enabled=%v auto_review=%v", name, e.Enabled, e.AutoReview)
		}
		if e.MaxFiles != 200 || e.Language != svc.Review.DefaultLang {
			t.Errorf("%s: max_files=%d language=%q", name, e.MaxFiles, e.Language)
		}
	}
	off := mustRepo(t, "enabled: false\nauto_review: false\n")
	if e := Merge(&svc, off); e.Enabled || e.AutoReview {
		t.Errorf("repo switches ignored: %+v", e)
	}
}

func TestMergeRepoValues(t *testing.T) {
	svc := mutSvc()
	svc.Jira.ProjectKeys = []string{"SVC"}
	r := mustRepo(t, "max_files: 1\njira:\n  project_keys: [ctx]\nsysmap:\n  exclude: [\"mocks/**\"]\n")
	e := Merge(&svc, r)
	if e.MaxFiles != 1 {
		t.Errorf("max_files: 1 must apply, got %d", e.MaxFiles)
	}
	if strings.Join(e.JiraProjectKeys, ",") != "CTX" {
		t.Errorf("a single repo jira key replaces the service keys, got %v", e.JiraProjectKeys)
	}
	if strings.Join(e.SysmapExclude, ",") != "mocks/**" {
		t.Errorf("sysmap.exclude not copied: %v", e.SysmapExclude)
	}
	// Absent values keep the service ones.
	e = Merge(&svc, mustRepo(t, "jira:\n  project_keys: []\n"))
	if e.Language != svc.Review.DefaultLang || strings.Join(e.JiraProjectKeys, ",") != "SVC" || e.MaxFiles != 200 {
		t.Errorf("service values lost: lang=%q keys=%v max_files=%d", e.Language, e.JiraProjectKeys, e.MaxFiles)
	}
}

func TestMergeSkills(t *testing.T) {
	svc := mutSvc()
	if Merge(&svc, nil).SkillsEnabled {
		t.Error("no review.skills.dir: skills must be off whatever the repo says")
	}
	svc.Review.Skills.Dir = t.TempDir()
	if !Merge(&svc, nil).SkillsEnabled {
		t.Error("skills dir set, no repo section: on")
	}
	if Merge(&svc, mustRepo(t, "skills:\n  enabled: false\n")).SkillsEnabled {
		t.Error("repo skills.enabled: false ignored")
	}
	e := Merge(&svc, mustRepo(t, "skills:\n  disable: [\" Go-Style \", \"\", sql]\n"))
	if !e.SkillsEnabled || strings.Join(e.SkillsDisable, ",") != "go-style,sql" {
		t.Errorf("disable list: enabled=%v %q", e.SkillsEnabled, e.SkillsDisable)
	}
}

// stricter decides whether a repository may change a per-hour limit: only
// downwards, or from "unlimited" (0) to a limit.
func TestStricterNeverLoosens(t *testing.T) {
	for _, tc := range []struct{ svc, repo, want int }{
		{5, 0, 5}, {5, 1, 1}, {5, 4, 4}, {5, 5, 5}, {5, 50, 5},
		{1, 5, 1}, {1, 1, 1},
		{0, 3, 3}, {0, 1, 1}, {0, 0, 0}, {-1, 2, 2},
		{5, -1, 5},
	} {
		if got := stricter(tc.svc, tc.repo); got != tc.want {
			t.Errorf("stricter(svc=%d, repo=%d) = %d, want %d", tc.svc, tc.repo, got, tc.want)
		}
	}
}

func TestMergeCooldownOnlyLonger(t *testing.T) {
	svc := mutSvc()
	svc.Limits.MRRerunCooldown = 0
	e := Merge(&svc, mustRepo(t, "limits:\n  mr_rerun_cooldown: 1m\n"))
	if e.Limits.MRRerunCooldown.D() != time.Minute {
		t.Errorf("no service cooldown: any repo cooldown applies, got %v", e.Limits.MRRerunCooldown.D())
	}
	svc.Limits.MRRerunCooldown = Duration(10 * time.Minute)
	for _, y := range []string{"limits:\n  mr_rerun_cooldown: -20m\n", "limits:\n  mr_rerun_cooldown: 9m\n", "limits: {}\n"} {
		if got := Merge(&svc, mustRepo(t, y)).Limits.MRRerunCooldown.D(); got != 10*time.Minute {
			t.Errorf("%q: cooldown %v, want the service 10m", y, got)
		}
	}
	if got := Merge(&svc, mustRepo(t, "limits:\n  mr_rerun_cooldown: 1h\n")).Limits.MRRerunCooldown.D(); got != time.Hour {
		t.Errorf("longer repo cooldown: %v", got)
	}
}

// ---------------------------------------------------------------------------
// Main branch, globs
// ---------------------------------------------------------------------------

// Equally specific globs must resolve the same way on every call (the map
// iteration order is random): the lexicographically smaller glob wins.
func TestMainBranchForTieIsDeterministic(t *testing.T) {
	g := GitLab{MainBranch: "main", MainBranches: map[string]string{"a/*": "aaa-from-a-star", "*/b": "zzz-from-star-b"}}
	for i := 0; i < 200; i++ {
		if got := g.MainBranchFor("a/b", "trunk"); got != "zzz-from-star-b" {
			t.Fatalf("call %d: %q, want the branch of the smaller glob \"*/b\"", i, got)
		}
	}
	// A longer glob still wins over a shorter one.
	g.MainBranches["a/b*"] = "longest"
	if got := g.MainBranchFor("a/b", "trunk"); got != "longest" {
		t.Errorf("longest glob: %q", got)
	}
}

func TestValidBranch(t *testing.T) {
	for b, want := range map[string]bool{
		"main": true, "release/1.2": true, "feature/x-y_z+1": true, "v1.0": true,
		"": false, "-main": false, "a..b": false, "a/": false, "x.lock": false, "a//b": false,
		"a b": false, "a~1": false, "a^": false, "a:b": false, "a\\b": false,
	} {
		if got := validBranch(b); got != want {
			t.Errorf("validBranch(%q) = %v, want %v", b, got, want)
		}
	}
}

func TestValidateMainBranches(t *testing.T) {
	s := mutSvc()
	s.GitLab.MainBranches = map[string]string{"a/[": "main"}
	wantInvalid(t, "bad glob", s, `gitlab.main_branches: invalid glob "a/["`)
	s.GitLab.MainBranches = map[string]string{"a/**": "x.lock"}
	wantInvalid(t, "bad branch", s, `gitlab.main_branches["a/**"]: invalid branch name`)
	s.GitLab.MainBranches = map[string]string{" /legacy/** ": " master "}
	wantValid(t, "trimmed entries", s)
}

func TestExcludedEmptyPath(t *testing.T) {
	g := GitLab{Exclude: []string{"**"}}
	if g.Excluded("") || g.Excluded(" / ") {
		t.Error("an empty project path is never excluded")
	}
	if !g.Excluded("a/b") {
		t.Error("** excludes everything else")
	}
}

// ---------------------------------------------------------------------------
// Service loading and validation
// ---------------------------------------------------------------------------

func TestValidateSyncHooksRequirements(t *testing.T) {
	ok := GitLab{URL: "https://g", ServiceToken: "st", WebhookSecret: "ws", WebhookURL: "https://bot/webhook", Groups: []string{"g"}}
	if err := (&Service{GitLab: ok}).ValidateSyncHooks(); err != nil {
		t.Fatalf("valid: %v", err)
	}
	noURL := ok
	noURL.URL = ""
	noGroups := ok
	noGroups.Groups = nil
	for name, tc := range map[string]struct {
		g    GitLab
		want string
	}{"url": {noURL, "gitlab.url"}, "groups": {noGroups, "gitlab.groups"}} {
		err := (&Service{GitLab: tc.g}).ValidateSyncHooks()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestLoadServiceInvalidYAML(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	if _, err := LoadService(writeConfig(t, "gitlab: [\n")); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("broken yaml must fail: %v", err)
	}
}

func TestLoadSandbox(t *testing.T) {
	sb, err := LoadSandbox(writeConfig(t, "sandbox:\n  egress_allow: [api.anthropic.com]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sb.EgressAllow, ",") != "api.anthropic.com" || sb.Mode != "docker" {
		t.Errorf("sandbox %+v", sb)
	}
	if _, err := LoadSandbox(writeConfig(t, "sandbox: [\n")); err == nil {
		t.Error("broken yaml must fail")
	}
	if _, err := LoadSandbox(writeConfig(t, "") + ".missing"); err == nil {
		t.Error("missing file must fail")
	}
}

// Secrets from env win; the yaml values are the fallback when env is unset.
func TestSecretsYAMLFallback(t *testing.T) {
	for _, k := range []string{"GITLAB_URL", "GITLAB_TOKEN", "GITLAB_WEBHOOK_SECRET", "GITLAB_HOOKS_TOKEN", "GITLAB_SERVICE_TOKEN"} {
		t.Setenv(k, "")
	}
	cfg, err := LoadService(writeConfig(t, "gitlab:\n  url: https://g\n  token: ytok\n  webhook_secret: ysec\n  service_token: yst\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitLab.Token != "ytok" || cfg.GitLab.WebhookSecret != "ysec" || cfg.GitLab.ServiceToken != "yst" {
		t.Errorf("yaml fallback lost: %+v", cfg.GitLab)
	}
}

func TestEventsEmptyMeansWebhook(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	cfg, err := LoadService(writeConfig(t, "gitlab:\n  url: https://g\n  events: \"  \"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitLab.Events != EventsWebhook {
		t.Errorf("events %q", cfg.GitLab.Events)
	}
}

func TestValidateBounds(t *testing.T) {
	s := mutSvc()
	s.Limits.MaxConcurrent = 0
	wantInvalid(t, "max_concurrent 0", s, "limits.max_concurrent")
	s.Limits.MaxConcurrent = 1
	wantValid(t, "max_concurrent 1", s)

	s = mutSvc()
	s.GitLab.SyncHooks, s.GitLab.ServiceToken, s.GitLab.WebhookURL, s.GitLab.Groups = true, "st", "https://bot/webhook", []string{"g"}
	s.GitLab.SyncInterval = Duration(time.Minute)
	wantValid(t, "sync_interval 1m", s)
	s.GitLab.SyncInterval = Duration(59 * time.Second)
	wantInvalid(t, "sync_interval 59s", s, "gitlab.sync_interval")

	s = mutSvc()
	s.Review.Context.RefreshInterval = Duration(time.Minute)
	wantValid(t, "context refresh 1m", s)
}

// ---------------------------------------------------------------------------
// review.lint / lint
// ---------------------------------------------------------------------------

func TestLintTimeoutValidation(t *testing.T) {
	s := mutSvc()
	s.Review.Lint.Timeout = Duration(9 * time.Second)
	wantInvalid(t, "9s", s, "review.lint.timeout")
	s.Review.Lint.Timeout = Duration(10 * time.Second)
	wantValid(t, "10s", s)
	s.Review.Lint.Enabled, s.Review.Lint.Timeout = false, 0
	wantValid(t, "disabled", s)
}

func TestMergeLintSecretsKept(t *testing.T) {
	svc := mutSvc()
	off := mustRepo(t, "lint:\n  disable: [gitleaks]\n")
	l := Merge(&svc, off).Lint
	if !l.SecretsKept || !contains(l.Tools, LintGitleaks) {
		t.Errorf("gitleaks must stay on and the attempt be noted: %+v", l)
	}
	svc.Review.Lint.Tools = nil
	l = Merge(&svc, off).Lint
	if l.SecretsKept || contains(l.Tools, LintGitleaks) {
		t.Errorf("the service does not run gitleaks: nothing was kept: %+v", l)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// context, coverage, migrations, path_instructions, tbd
// ---------------------------------------------------------------------------

func TestRepoContextSkipsBlankExcludes(t *testing.T) {
	r := mustRepo(t, "context:\n  exclude: [\"\", \" / \", \"Legacy/**\"]\n")
	if strings.Join(r.Context.Exclude, ",") != "Legacy/**" {
		t.Fatalf("exclude %q", r.Context.Exclude)
	}
	if !Merge(&Service{}, r).ContextExcluded("legacy/x") {
		t.Error("the glob after blank entries must apply")
	}
}

func TestRelatedNoteLimit(t *testing.T) {
	note := strings.Repeat("я", MaxRelatedNoteLen)
	if _, err := ParseRepo([]byte("context:\n  related:\n    - repo: a/b\n      note: " + note + "\n")); err != nil {
		t.Errorf("%d characters are allowed: %v", MaxRelatedNoteLen, err)
	}
	if _, err := ParseRepo([]byte("context:\n  related:\n    - repo: a/b\n      note: " + note + "я\n")); err == nil {
		t.Error("a longer note must fail")
	}
}

func TestCoverageBounds(t *testing.T) {
	s := mutSvc()
	for mb, ok := range map[int]bool{0: false, 1: true, 100: true, 101: false} {
		s.Review.Coverage.MaxReportMB = mb
		if ok {
			wantValid(t, fmt.Sprintf("max_report_mb %d", mb), s)
		} else {
			wantInvalid(t, fmt.Sprintf("max_report_mb %d", mb), s, "review.coverage.max_report_mb")
		}
	}
	s.Review.Coverage.Enabled, s.Review.Coverage.MaxReportMB = false, 0
	wantValid(t, "coverage off", s)

	r := mustRepo(t, "coverage:\n  reports:\n    - job: unit\n      path: cov.xml\n  min: 100\n")
	if len(r.Coverage.Reports) != 1 || r.Coverage.Reports[0].Job != "unit" || r.Coverage.Min != 100 {
		t.Errorf("reports-only config: %+v", r.Coverage)
	}
	var b strings.Builder
	b.WriteString("coverage:\n  reports:\n")
	for i := 0; i < MaxCoverageReports; i++ {
		fmt.Fprintf(&b, "    - job: j%d\n      path: c%d.xml\n", i, i)
	}
	if _, err := ParseRepo([]byte(b.String())); err != nil {
		t.Errorf("%d reports allowed: %v", MaxCoverageReports, err)
	}
	if _, err := ParseRepo([]byte(b.String() + "    - job: x\n      path: x.xml\n")); err == nil {
		t.Error("too many reports must fail")
	}
	if _, err := ParseRepo([]byte("coverage:\n  job: j\n  path: c.xml\n  min: 100.5\n")); err == nil {
		t.Error("min above 100 must fail")
	}
}

func TestMigrationsBounds(t *testing.T) {
	s := mutSvc()
	for n, ok := range map[int]bool{0: false, 1: true, 50: true, 51: false} {
		s.Review.Migrations.MaxUsages = n
		if ok {
			wantValid(t, fmt.Sprintf("max_usages %d", n), s)
		} else {
			wantInvalid(t, fmt.Sprintf("max_usages %d", n), s, "review.migrations.max_usages")
		}
	}
}

func TestPathInstructionsCap(t *testing.T) {
	list := make([]PathInstruction, MaxPathInstructions)
	for i := range list {
		list[i] = PathInstruction{Path: fmt.Sprintf("d%d/**", i), Instructions: "x"}
	}
	if _, err := normalizePathInstructions(list, "p"); err != nil {
		t.Errorf("%d entries allowed: %v", MaxPathInstructions, err)
	}
	if _, err := normalizePathInstructions(append(list, list[0]), "p"); err == nil {
		t.Error("one more must fail")
	}
}

func TestTBDBounds(t *testing.T) {
	s := mutSvc()
	s.Review.TBD = TBD{MaxLines: 0, MaxAge: 0, MaxBehind: 0, FlagsHint: strings.Repeat("я", 1000)}
	wantValid(t, "zeros = no limit", s)
	for name, tc := range map[string]struct {
		t    TBD
		want string
	}{
		"max_lines":  {TBD{MaxLines: -1}, "review.tbd.max_lines"},
		"max_age":    {TBD{MaxAge: Duration(-time.Hour)}, "review.tbd.max_age"},
		"max_behind": {TBD{MaxBehind: -1}, "review.tbd.max_behind"},
		"flags_hint": {TBD{FlagsHint: strings.Repeat("я", 1001)}, "review.tbd.flags_hint"},
	} {
		s.Review.TBD = tc.t
		wantInvalid(t, name, s, tc.want)
	}

	if _, err := ParseRepo([]byte("tbd:\n  max_age: 0s\n  max_behind: 0\n  max_lines: 0\n")); err != nil {
		t.Errorf("repo zeros: %v", err)
	}
	for _, bad := range []string{"tbd:\n  max_age: -1h\n", "tbd:\n  max_behind: -1\n", "tbd:\n  flags_hint: " + strings.Repeat("я", 1001) + "\n"} {
		if _, err := ParseRepo([]byte(bad)); err == nil {
			t.Errorf("%.40q must fail", bad)
		}
	}
	svc := mutSvc()
	svc.Review.TBD.FlagsHint = "flags.Enabled"
	if got := Merge(&svc, mustRepo(t, "tbd:\n  max_lines: 10\n")).TBD.FlagsHint; got != "flags.Enabled" {
		t.Errorf("no repo flags_hint keeps the service one, got %q", got)
	}
	if got := Merge(&svc, mustRepo(t, "tbd:\n  flags_hint: own\n")).TBD.FlagsHint; got != "own" {
		t.Errorf("repo flags_hint: %q", got)
	}
}

// ---------------------------------------------------------------------------
// sysmap, jira, memory
// ---------------------------------------------------------------------------

func TestSysmapBounds(t *testing.T) {
	type fld struct {
		name     string
		set      func(*ReviewSysmap, int)
		min, max int // valid range; max < 0 = unbounded
		key      string
	}
	for _, f := range []fld{
		{"max_files_per_repo", func(c *ReviewSysmap, v int) { c.MaxFilesPerRepo = v }, 1, -1, "max_files_per_repo"},
		{"max_file_kb", func(c *ReviewSysmap, v int) { c.MaxFileKB = v }, 1, 1024, "max_file_kb"},
		{"deps_max_files", func(c *ReviewSysmap, v int) { c.DepsMaxFiles = v }, 1, 5000, "deps_max_files"},
		{"deps_max_mrs", func(c *ReviewSysmap, v int) { c.DepsMaxMRs = v }, 1, 100, "deps_max_mrs"},
		{"deps_max_api_calls", func(c *ReviewSysmap, v int) { c.DepsMaxAPICalls = v }, 1, 500, "deps_max_api_calls"},
		{"deps_max_runs", func(c *ReviewSysmap, v int) { c.DepsMaxRuns = v }, 1, 100, "deps_max_runs"},
		{"deps_max_runs_per_hour", func(c *ReviewSysmap, v int) { c.DepsMaxRunsPerHour = v }, 0, 1000, "deps_max_runs_per_hour"},
		{"max_runs_per_hour", func(c *ReviewSysmap, v int) { c.MaxRunsPerHour = v }, 0, -1, "max_runs_per_hour"},
	} {
		check := func(v int, ok bool) {
			s := mutSvc()
			f.set(&s.Review.Sysmap, v)
			if ok {
				wantValid(t, fmt.Sprintf("%s=%d", f.name, v), s)
			} else {
				wantInvalid(t, fmt.Sprintf("%s=%d", f.name, v), s, "review.sysmap."+f.key)
			}
		}
		check(f.min, true)
		check(f.min-1, false)
		if f.max >= 0 {
			check(f.max, true)
			check(f.max+1, false)
		}
	}
	// Phase B budgets are not checked when phase B is off.
	s := mutSvc()
	s.Review.Sysmap.Deps, s.Review.Sysmap.DepsMaxMRs = false, 0
	wantValid(t, "deps off", s)

	s = mutSvc()
	s.Review.Sysmap.Hints = map[string]string{}
	for i := 0; i < MaxSysmapHints; i++ {
		s.Review.Sysmap.Hints[fmt.Sprintf("K%d", i)] = "a/b"
	}
	wantValid(t, "max hints", s)
	s.Review.Sysmap.Hints["extra"] = "a/b"
	wantInvalid(t, "too many hints", s, "review.sysmap.hints: at most")
}

func TestRepoSysmapSkipsBlankExcludes(t *testing.T) {
	r := mustRepo(t, "sysmap:\n  exclude: [\"\", \" \", \"/mocks/**/\"]\n")
	if strings.Join(r.Sysmap.Exclude, ",") != "mocks/**" {
		t.Errorf("exclude %q", r.Sysmap.Exclude)
	}
}

func TestJiraNormalizeAndValidate(t *testing.T) {
	j := Jira{ProjectKeys: []string{" ctx ", "", "  "}, TitleTypes: []string{" ", ""}}
	j.normalize()
	if strings.Join(j.ProjectKeys, ",") != "CTX" {
		t.Errorf("keys %q", j.ProjectKeys)
	}
	if strings.Join(j.TitleTypes, ",") != strings.Join(DefaultTitleTypes, ",") {
		t.Errorf("blank title types fall back to the defaults: %q", j.TitleTypes)
	}
	for _, u := range []string{"http://", "ftp://jira.example", "jira.example"} {
		bad := Jira{URL: u, Token: "t"}
		if errs := bad.validate(); len(errs) == 0 {
			t.Errorf("jira.url %q must be rejected", u)
		}
	}
	good := Jira{URL: "https://jira.example", Token: "t"}
	if errs := good.validate(); len(errs) != 0 {
		t.Errorf("valid jira: %v", errs)
	}
}

func TestMemoryTotalCapSmall(t *testing.T) {
	if got := (Memory{MaxChars: 1}).TotalCap(); got != 1 {
		t.Errorf("max_chars 1: %d", got)
	}
	if got := (Memory{MaxChars: -5}).TotalCap(); got != DefaultMemoryMaxChars {
		t.Errorf("negative max_chars: %d", got)
	}
}
