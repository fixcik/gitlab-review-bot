package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
)

// review.checkout_cache: checked only when enabled; CacheTTL is 0 when off.
func TestCheckoutCacheSettings(t *testing.T) {
	if d := Defaults().Review.CheckoutCache; d != (CheckoutCache{Enabled: true, TTL: Duration(2 * time.Hour), MaxGB: 10}) {
		t.Errorf("defaults %+v", d)
	}
	off := CheckoutCache{Enabled: false, TTL: 0, MaxGB: -1}
	if errs := off.validate(); errs != nil {
		t.Errorf("disabled cache validated: %v", errs)
	}
	if off := (CheckoutCache{TTL: Duration(time.Hour)}); off.CacheTTL() != 0 {
		t.Errorf("disabled cache TTL %v", off.CacheTTL())
	}
	for _, tc := range []struct {
		c    CheckoutCache
		want []string
	}{
		{CheckoutCache{Enabled: true, TTL: Duration(time.Minute), MaxGB: 0}, nil},
		{CheckoutCache{Enabled: true, TTL: 0, MaxGB: 1}, []string{"review.checkout_cache.ttl must be positive (or set review.checkout_cache.enabled: false)"}},
		{CheckoutCache{Enabled: true, TTL: Duration(-time.Second), MaxGB: 1}, []string{"review.checkout_cache.ttl must be positive (or set review.checkout_cache.enabled: false)"}},
		{CheckoutCache{Enabled: true, TTL: Duration(time.Nanosecond), MaxGB: -0.5}, []string{"review.checkout_cache.max_gb must not be negative"}},
		{CheckoutCache{Enabled: true, TTL: 0, MaxGB: -1}, []string{
			"review.checkout_cache.ttl must be positive (or set review.checkout_cache.enabled: false)",
			"review.checkout_cache.max_gb must not be negative"}},
	} {
		if got := tc.c.validate(); strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%+v: %q, want %q", tc.c, got, tc.want)
		}
	}
	// Wired into Validate.
	s := mutSvc()
	s.Review.CheckoutCache = CheckoutCache{Enabled: true, MaxGB: -1, TTL: Duration(time.Hour)}
	wantInvalid(t, "negative max_gb", s, "review.checkout_cache.max_gb must not be negative")

	on := CheckoutCache{Enabled: true, TTL: Duration(90 * time.Minute)}
	if on.CacheTTL() != 90*time.Minute {
		t.Errorf("TTL %v", on.CacheTTL())
	}
	for gb, want := range map[float64]int64{0: 0, 1: 1 << 30, 1.5: 3 << 29, 10: 10 << 30} {
		if got := (CheckoutCache{MaxGB: gb}).MaxBytes(); got != want {
			t.Errorf("MaxBytes(%v) = %d, want %d", gb, got, want)
		}
	}
}

func TestCIExplainModelBlank(t *testing.T) {
	s := Defaults()
	s.Review.CI.Model, s.Claude.LightModel, s.Claude.Model = " ", "", "\t"
	if m := s.CIExplainModel(); m != "" {
		t.Errorf("all blank: %q", m)
	}
	s.Claude.Model = " sonnet "
	if m := s.CIExplainModel(); m != "sonnet" {
		t.Errorf("trimmed: %q", m)
	}
}

func TestCIListCaps(t *testing.T) {
	globs := make([]string, MaxCIIgnoreJobs)
	for i := range globs {
		globs[i] = "job-*"
	}
	if errs := validJobGlobs("k", globs); len(errs) > 0 {
		t.Errorf("%d globs: %v", len(globs), errs)
	}
	if errs := validJobGlobs("k", append(globs, "x")); len(errs) != 1 || errs[0] != "k: at most 50 globs" {
		t.Errorf("%d globs: %v", len(globs)+1, errs)
	}
	if errs := validJobGlobs("k", []string{" ", "[", "ok-*"}); strings.Join(errs, "|") != `k: empty glob|k: invalid glob "["` {
		t.Errorf("every glob is checked: %v", errs)
	}
	for _, tc := range []struct {
		logs, tests int
		want        string
	}{
		{5, 1, ""}, {MaxCIMaxLogLines, MaxCIMaxTests, ""},
		{4, 1, "review.ci.max_log_lines must be between 5 and 200"},
		{MaxCIMaxLogLines + 1, 1, "review.ci.max_log_lines must be between 5 and 200"},
		{5, 0, "review.ci.max_tests must be between 1 and 50"},
		{5, MaxCIMaxTests + 1, "review.ci.max_tests must be between 1 and 50"},
	} {
		s := mutSvc()
		s.Review.CI.MaxLogLines, s.Review.CI.MaxTests = tc.logs, tc.tests
		if tc.want == "" {
			wantValid(t, fmt.Sprintf("ci %d/%d", tc.logs, tc.tests), s)
		} else {
			wantInvalid(t, fmt.Sprintf("ci %d/%d", tc.logs, tc.tests), s, tc.want)
		}
	}

	s := mutSvc()
	s.Review.CI.NotifyMain = nil
	for i := 0; i < MaxCINotifyMain; i++ {
		s.Review.CI.NotifyMain = append(s.Review.CI.NotifyMain, "user")
	}
	wantValid(t, "20 notify_main users", s)
	s.Review.CI.NotifyMain = append(s.Review.CI.NotifyMain, "@one")
	wantInvalid(t, "21 notify_main users", s, "review.ci.notify_main: at most 20 usernames")

	y := "ci:\n  ignore_jobs: [" + strings.TrimSuffix(strings.Repeat("a, ", MaxCIIgnoreJobs+1), ", ") + "]\n"
	if _, err := ParseRepo([]byte(y)); err == nil || !strings.Contains(err.Error(), "ci.ignore_jobs: at most 50 globs") {
		t.Errorf("repo ignore_jobs cap: %v", err)
	}
}

func TestLabelRuleEdgeCases(t *testing.T) {
	// when_fact must be a name or a list of names.
	var fl FactList
	if err := fl.UnmarshalYAML([]byte("{a: 1}")); err == nil || !strings.HasPrefix(err.Error(), "when_fact must be a fact name or a list of names: ") || fl != nil {
		t.Errorf("mapping when_fact: %v %q", err, fl)
	}
	if err := fl.UnmarshalYAML([]byte("migrations")); err != nil || len(fl) != 1 || fl[0] != "migrations" {
		t.Errorf("one name: %v %q", err, fl)
	}
	if _, err := ParseRepo([]byte("labels:\n  rules:\n    - {name: x, when_fact: {a: 1}}\n")); err == nil || !strings.Contains(err.Error(), "line 1: labels: ") {
		t.Errorf("mapping when_fact in the repo config: %v", err)
	}
	var err error
	// An empty name is no fact at all.
	for _, v := range []string{`""`, `"  "`} {
		var f struct {
			W FactList `yaml:"w"`
		}
		f.W = FactList{"stale"}
		if err := yaml.Unmarshal([]byte("w: "+v+"\n"), &f); err != nil || f.W != nil {
			t.Errorf("when_fact %s: %v %q", v, err, f.W)
		}
	}
	// Paths are capped per rule.
	paths := strings.TrimSuffix(strings.Repeat(`"a/**", `, MaxLabelPaths+1), ", ")
	_, err = ParseRepo([]byte("labels:\n  rules:\n    - {name: big, paths: [" + paths + "]}\n"))
	if err == nil || !strings.Contains(err.Error(), "labels.rules[0] (big): at most 50 paths") {
		t.Errorf("paths cap: %v", err)
	}
	paths = strings.TrimSuffix(strings.Repeat(`"a/**", `, MaxLabelPaths), ", ")
	if _, err := ParseRepo([]byte("labels:\n  rules:\n    - {name: big, paths: [" + paths + "]}\n")); err != nil {
		t.Errorf("50 paths: %v", err)
	}
}

// Invalid service rules (Validate refuses them at startup) are dropped as a
// whole by Merge; the repository rules still apply.
func TestMergeLabelsInvalidServiceRules(t *testing.T) {
	svc := Defaults()
	svc.Review.Labels.Rules = []LabelRule{{Name: "ok", Paths: []string{"a/**"}}, {Name: "a,b", Paths: []string{"x"}}}
	repo := mustRepo(t, "labels:\n  rules:\n    - {name: frontend, paths: [\"web/**\"]}\n")
	e := Merge(&svc, repo).Labels
	if len(e.Rules) != 1 || e.Rules[0].Name != "frontend" {
		t.Fatalf("rules %+v", e.Rules)
	}
	if e := Merge(&svc, nil).Labels; len(e.Rules) != 0 {
		t.Fatalf("service-only rules %+v", e.Rules)
	}
}

func TestRemovedKeysUnparsable(t *testing.T) {
	if got := removedKeys([]byte("a: [\n"), removedServiceLintKeys, "review", "lint"); got != nil {
		t.Errorf("unparsable: %v", got)
	}
	if got := removedKeys([]byte("review:\n  lint:\n    allow_custom: true\n    semgrep_rules: []\n"), removedServiceLintKeys, "review", "lint"); strings.Join(got, ",") != "allow_custom,semgrep_rules" {
		t.Errorf("found: %v", got)
	}
}

// A CI lint tool name derived from a long job name is cut to 32
// characters, without a separator at the end.
func TestCIToolNameLongJob(t *testing.T) {
	for job, want := range map[string]string{
		strings.Repeat("a", 40):                       strings.Repeat("a", 32),
		strings.Repeat("a", 31) + "-tail-of-the-name": strings.Repeat("a", 31),
		strings.Repeat("b", 32):                       strings.Repeat("b", 32),
		strings.Repeat("c", 33):                       strings.Repeat("c", 32),
		"Lint: PHPStan [*]":                           "lint-phpstan",
	} {
		if got := ciToolName(job); got != want {
			t.Errorf("ciToolName(%q) = %q, want %q", job, got, want)
		}
	}
	r := mustRepo(t, "lint:\n  ci:\n    - {job: \""+strings.Repeat("x", 31)+".long-suffix\", report: codequality}\n")
	if got := r.Lint.CI[0].Tool; got != strings.Repeat("x", 31) {
		t.Errorf("derived tool %q", got)
	}
}

func TestServiceValidateMoreSections(t *testing.T) {
	s := mutSvc()
	s.Review.BlockMinSeverity = "huge"
	wantInvalid(t, "block_min_severity", s, "review.block_min_severity must be critical, major, minor or nit")

	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(dir, "missing"), file} {
		s := mutSvc()
		s.Review.Skills.Dir = d
		wantInvalid(t, "skills dir "+d, s, "review.skills.dir must be an existing directory: "+d)
	}
	s = mutSvc()
	s.Review.Skills.Dir = dir
	wantValid(t, "skills dir", s)

	s = mutSvc()
	s.Review.Todo.Keywords = nil
	for i := 0; i <= maxTodoKeywords; i++ {
		s.Review.Todo.Keywords = append(s.Review.Todo.Keywords, "TODO")
	}
	wantInvalid(t, "todo keywords cap", s, "review.todo.keywords: at most 20 keywords")
	s.Review.Todo.Keywords = s.Review.Todo.Keywords[:maxTodoKeywords]
	wantValid(t, "20 todo keywords", s)

	s.Review.Todo.Ignore = nil
	for i := 0; i <= maxTodoIgnore; i++ {
		s.Review.Todo.Ignore = append(s.Review.Todo.Ignore, "gen/**")
	}
	wantInvalid(t, "todo ignore cap", s, "review.todo.ignore: at most 100 globs")
	s.Review.Todo.Ignore = s.Review.Todo.Ignore[:maxTodoIgnore]
	wantValid(t, "100 todo ignore globs", s)
}

func TestVerifyMax(t *testing.T) {
	for n, want := range map[int]int{0: DefaultVerifyMaxFindings, -3: DefaultVerifyMaxFindings, 1: 1, 25: 25} {
		if got := (Verify{MaxFindings: n}).Max(); got != want {
			t.Errorf("Max(%d) = %d, want %d", n, got, want)
		}
	}
}

func TestValidateSyncHooksWebhookURL(t *testing.T) {
	base := Defaults()
	base.GitLab.URL = "https://g.example"
	base.GitLab.ServiceToken = "svc"
	base.GitLab.WebhookSecret = "sec"
	base.GitLab.Groups = []string{"team"}
	for u, want := range map[string]string{
		"":                        "gitlab.webhook_url is required",
		"ftp://bot/webhook":       "gitlab.webhook_url must be an http(s) URL",
		"http://%zz/webhook":      "gitlab.webhook_url must be an http(s) URL",
		"https://bot/webhook":     "",
		"http://bot:8080/webhook": "",
	} {
		s := base
		s.GitLab.WebhookURL = u
		err := s.ValidateSyncHooks()
		if want == "" {
			if err != nil {
				t.Errorf("%q: %v", u, err)
			}
			continue
		}
		if err == nil || err.Error() != "invalid config: "+want {
			t.Errorf("%q: %v, want %q", u, err, want)
		}
	}
}

func TestDurationNotAString(t *testing.T) {
	var v struct {
		D Duration `yaml:"d"`
	}
	v.D = Duration(time.Minute)
	if err := yaml.Unmarshal([]byte("d: {a: 1}\n"), &v); err == nil {
		t.Fatal("a mapping is not a duration")
	}
	if v.D != Duration(time.Minute) {
		t.Errorf("value changed on error: %v", v.D.D())
	}
	if _, err := ParseRepo([]byte("limits: {mr_rerun_cooldown: [1]}\n")); err == nil || !strings.Contains(err.Error(), "limits") {
		t.Errorf("repo: %v", err)
	}
}

func TestLoadDiagErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	if _, _, err := LoadDiag(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
	p := writeConfig(t, "diag: [\n")
	if _, _, err := LoadDiag(p); err == nil || !strings.HasPrefix(err.Error(), "parse "+p+": ") {
		t.Errorf("bad yaml: %v", err)
	}
	dir, d, err := LoadDiag("")
	if err != nil || dir != Defaults().DataDir || d.MaxRunMB != Defaults().Diag.MaxRunMB {
		t.Errorf("defaults: %q %+v %v", dir, d, err)
	}
}

func clearOTelEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_SERVICE_NAME", "OTEL_SDK_DISABLED"} {
		t.Setenv(k, "")
	}
}

// LoadSandbox adds the CLI tracing collector to egress_allow once: an
// entry that is already there (any case, spaces) is not repeated. An empty
// file is the defaults.
func TestLoadSandboxTracingHostOnce(t *testing.T) {
	clearOTelEnv(t)
	tracing := "tracing:\n  enabled: true\n  claude_cli: true\n  endpoint: https://otel.example.com\n"
	sb, err := LoadSandbox(writeConfig(t, tracing+"sandbox:\n  egress_allow: [\" OTEL.example.com \", api.example.com]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sb.EgressAllow, ",") != " OTEL.example.com ,api.example.com" {
		t.Errorf("egress_allow %q", sb.EgressAllow)
	}
	sb, err = LoadSandbox(writeConfig(t, tracing+"sandbox:\n  egress_allow: [api.example.com]\n"))
	if err != nil || strings.Join(sb.EgressAllow, ",") != "api.example.com,otel.example.com" {
		t.Errorf("egress_allow %q %v", sb.EgressAllow, err)
	}
	// The collector may come from the OTel environment only.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", " https://env-otel.example.com ")
	sb, err = LoadSandbox(writeConfig(t, "tracing:\n  enabled: true\n  claude_cli: true\nsandbox:\n  egress_allow: [api.example.com]\n"))
	if err != nil || strings.Join(sb.EgressAllow, ",") != "api.example.com,env-otel.example.com" {
		t.Errorf("env endpoint: %q %v", sb.EgressAllow, err)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	sb, err = LoadSandbox(writeConfig(t, ""))
	if err != nil || sb.Mode != Defaults().Sandbox.Mode {
		t.Errorf("empty file: %+v %v", sb, err)
	}
}

func TestMCPClientURLUnknown(t *testing.T) {
	c := Defaults()
	c.Review.MCP.Listen = "nope"
	if got := c.MCPClientURL(); got != "" {
		t.Errorf("bad listen: %q", got)
	}
	for _, proxy := range []string{"", "http://%zz", "egress:3128"} {
		c := Defaults()
		c.Sandbox.Mode = "docker"
		c.Sandbox.EgressProxy = proxy
		if got := c.MCPClientURL(); got != "" {
			t.Errorf("proxy %q: %q", proxy, got)
		}
	}
}

func TestMatchAnyPathBlankAndBadGlobs(t *testing.T) {
	if !MatchAnyPath([]string{"", "  ", " src/** "}, "src/a.go") {
		t.Error("trimmed glob after blanks must match")
	}
	if MatchAnyPath([]string{"", "  "}, "") {
		t.Error("blank globs match nothing, not even an empty path")
	}
	if MatchAnyPath([]string{"[", "docs/**"}, "src/a.go") {
		t.Error("invalid or non-matching globs")
	}
}

func TestTracingCLIDirectHostAndServiceName(t *testing.T) {
	for ep, want := range map[string]string{
		"http://%zz":              "",
		"http://Otel:4318":        "otel",
		"https://otel.local:4318": "otel.local",
		"https://otel.local":      "", // through the egress proxy
	} {
		tr := Tracing{Enabled: true, ClaudeCLI: true, Endpoint: ep}
		if got := tr.CLIDirectHost(); got != want {
			t.Errorf("CLIDirectHost(%q) = %q, want %q", ep, got, want)
		}
	}
	clearOTelEnv(t)
	tr := Tracing{ServiceName: "  "}
	applyTracingEnv(&tr)
	if tr.ServiceName != DefaultTracingService {
		t.Errorf("blank service name: %q", tr.ServiceName)
	}
	tr = Tracing{ServiceName: " svc "}
	applyTracingEnv(&tr)
	if tr.ServiceName != "svc" {
		t.Errorf("service name: %q", tr.ServiceName)
	}
}

// A service config file without content (empty, only comments, a bare
// "---" or null) keeps every default: go-yaml decoding such a document
// over the defaults reset them to zero values (no sandbox mode, limits
// off, hook_ssl_verify false), so an env-only setup failed validation and
// `reviewbot sync-hooks` would have registered hooks without SSL
// verification.
func TestEmptyServiceFileKeepsDefaults(t *testing.T) {
	t.Setenv("GITLAB_URL", "https://g.example")
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	t.Setenv("GITLAB_SERVICE_TOKEN", "svc")
	clearOTelEnv(t)
	def := Defaults()
	for _, body := range []string{"", "# configured by env\n", "---\n", "~\n"} {
		p := writeConfig(t, body)
		cfg, err := LoadService(p)
		if err != nil {
			t.Errorf("%q: LoadService: %v", body, err)
			continue
		}
		if cfg.Sandbox.Mode != def.Sandbox.Mode || cfg.Limits.MaxConcurrent != def.Limits.MaxConcurrent ||
			cfg.Limits.GlobalRunsPerHour != def.Limits.GlobalRunsPerHour || !cfg.GitLab.HookSSLVerify {
			t.Errorf("%q: defaults lost: %+v %+v", body, cfg.Sandbox, cfg.Limits)
		}
		sh, err := LoadSyncHooks(p)
		if sh == nil || !sh.GitLab.HookSSLVerify || sh.GitLab.SyncInterval != def.GitLab.SyncInterval {
			t.Errorf("%q: LoadSyncHooks: %+v %v", body, sh, err)
		}
		_, d, err := LoadDiag(p)
		if err != nil || d.MaxRunMB != def.Diag.MaxRunMB || d.Retention != def.Diag.Retention {
			t.Errorf("%q: LoadDiag: %+v %v", body, d, err)
		}
	}
	// A real document is still decoded over the defaults.
	cfg, err := LoadService(writeConfig(t, "# c\nlimits:\n  max_concurrent: 3\n"))
	if err != nil || cfg.Limits.MaxConcurrent != 3 || cfg.Limits.GlobalRunsPerHour != def.Limits.GlobalRunsPerHour {
		t.Fatalf("document: %+v %v", cfg, err)
	}
}
