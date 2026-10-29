package config

import (
	"strings"
	"testing"
)

func TestRoutingDefaults(t *testing.T) {
	d := Defaults()
	r := d.Review.Routing
	if !r.Enabled || r.LightModel != "" || r.MaxLines != 60 || len(r.LowRiskPaths) == 0 || len(r.SensitivePaths) == 0 {
		t.Errorf("defaults %+v", r)
	}
	if !d.Review.SkipTrivialIncrements {
		t.Error("skip_trivial_increments must default to true")
	}
	if errs := r.validate(); len(errs) > 0 {
		t.Errorf("defaults invalid: %v", errs)
	}
	e := Merge(&d, nil).Routing
	if !e.Enabled || e.LightModel != "haiku" || e.MaxLines != 60 {
		t.Errorf("effective %+v", e)
	}
	for _, p := range []string{"README.md", "docs/guide/setup.yaml", "pkg/x/x_test.go", "app/test_api.py", "web/src/a.test.ts", ".gitignore"} {
		if !MatchAnyPath(e.LowRiskPaths, p) {
			t.Errorf("%s should be low risk", p)
		}
	}
	for _, p := range []string{"main.go", "src/app.py", "config/app.yaml", "package.json",
		// Dependency manifests are not documentation; .gitattributes
		// changes what the diff shows.
		"requirements.txt", "requirements/base.txt", "constraints.txt", "CMakeLists.txt", "robots.txt", ".gitattributes"} {
		if MatchAnyPath(e.LowRiskPaths, p) {
			t.Errorf("%s should not be low risk", p)
		}
	}
	// Manifests under a low-risk directory are a risk signal.
	for _, p := range []string{"docs/requirements.txt", "requirements/prod.txt", "tests/package.json", "docs/package-lock.json",
		"tests/go.mod", "docs/go.sum", "docs/pyproject.toml", "docs/Pipfile.lock", "tests/yarn.lock", "docs/Gemfile.lock", "tests/Cargo.toml",
		"tests/pom.xml", "docs/build.gradle.kts", "tests/composer.json", "tests/constraints-dev.txt"} {
		if !MatchAnyPath(e.SensitivePaths, p) {
			t.Errorf("%s should be sensitive", p)
		}
	}
	if !MatchAnyPath(e.LowRiskPaths, "README.txt") {
		t.Error("README.txt should be low risk")
	}
	for _, p := range []string{"internal/auth/token.go", "docs/security/README.md", "pkg/crypto/aes_test.go", "Dockerfile",
		"build/Dockerfile.sandbox", ".gitlab-ci.yml", "db/migrations/001.sql", "services/payments/README.md", "requirements-dev.txt"} {
		if !MatchAnyPath(e.SensitivePaths, p) {
			t.Errorf("%s should be sensitive", p)
		}
	}
}

func TestRoutingValidate(t *testing.T) {
	r := defaultRouting()
	r.MaxLines = 0
	r.SensitivePaths = append(r.SensitivePaths, "a/[")
	errs := strings.Join(r.validate(), "; ")
	for _, want := range []string{"review.routing.max_lines", `review.routing.sensitive_paths: invalid glob "a/["`} {
		if !strings.Contains(errs, want) {
			t.Errorf("errors %q, want %q", errs, want)
		}
	}
}

// Models are the service's decision: a repository cannot change the
// routing (its old routing section is a removed key).
func TestRoutingServiceOnly(t *testing.T) {
	svc := Defaults()
	if e := Merge(&svc, &Repo{}).Routing; !e.Enabled || e.LightModel != "haiku" || len(e.SensitivePaths) != len(DefaultRoutingSensitivePaths) {
		t.Errorf("defaults: %+v", e)
	}
	// The service light model from review.routing wins over claude.light_model.
	svc2 := Defaults()
	svc2.Review.Routing.LightModel = "sonnet"
	if e := Merge(&svc2, nil).Routing; e.LightModel != "sonnet" {
		t.Errorf("service light model: %+v", e)
	}
	// No light model at all: routing off.
	svc3 := Defaults()
	svc3.Claude.LightModel = ""
	if e := Merge(&svc3, nil).Routing; e.Enabled {
		t.Errorf("no light model must turn routing off: %+v", e)
	}
	for _, y := range []string{
		"routing:\n  light_model: opus\n",
		"routing:\n  enabled: false\n",
		"routing:\n  sensitive_paths: [\"legal/**\"]\n",
	} {
		_, err := ParseRepo([]byte(y))
		if err == nil || !strings.Contains(err.Error(), "models are chosen only in the service config") {
			t.Errorf("%q: err %v", y, err)
		}
	}
}

func TestModelRank(t *testing.T) {
	for m, want := range map[string]int{"haiku": 1, "claude-haiku-4-5": 1, "Sonnet": 2, "claude-opus-4-1": 3, "": 0, "gpt": 0} {
		if got := ModelRank(m); got != want {
			t.Errorf("%q: %d, want %d", m, got, want)
		}
	}
}

// The heavy route: off by default (no heavy_model), 300 lines, no paths;
// it lives in the service config only.
func TestRoutingHeavy(t *testing.T) {
	d := Defaults()
	r := d.Review.Routing
	if r.HeavyModel != "" || r.HeavyMinLines != 300 || len(r.HeavyPaths) != 0 {
		t.Errorf("heavy defaults %+v", r)
	}
	if e := Merge(&d, nil).Routing; e.HeavyModel != "" || e.HeavyMinLines != 300 {
		t.Errorf("effective %+v", e)
	}
	s := mutSvc()
	s.Review.Routing.HeavyModel = "opus"
	s.Review.Routing.HeavyPaths = []string{" **/migrations/** ", "**/openapi*.yaml"}
	wantValid(t, "heavy on", s)
	// The heavy route does not depend on the light one.
	s.Review.Routing.Enabled = false
	s.Claude.LightModel = ""
	if e := Merge(&s, nil).Routing; e.Enabled || e.HeavyModel != "opus" || strings.Join(e.HeavyPaths, ",") != "**/migrations/**,**/openapi*.yaml" {
		t.Errorf("heavy without light: %+v", e)
	}
	for _, c := range []struct {
		mut  func(*Routing)
		want string
	}{
		{func(r *Routing) { r.HeavyModel = "opus 4" }, "review.routing.heavy_model"},
		{func(r *Routing) { r.HeavyMinLines = 0 }, "review.routing.heavy_min_lines"},
		{func(r *Routing) { r.HeavyPaths = []string{"a/["} }, "review.routing.heavy_paths"},
		{func(r *Routing) { r.HeavyModel, r.HeavyPaths = "", []string{"**/migrations/**"} }, "review.routing.heavy_paths needs review.routing.heavy_model"},
	} {
		s := mutSvc()
		s.Review.Routing.HeavyModel = "opus"
		c.mut(&s.Review.Routing)
		wantInvalid(t, c.want, s, c.want)
	}
	if _, err := ParseRepo([]byte("routing:\n  heavy_model: opus\n")); err == nil || !strings.Contains(err.Error(), "models are chosen only in the service config") {
		t.Errorf("repo heavy_model: %v", err)
	}
}
