// Package deploy holds deployment assets (compose config, skills, Grafana
// dashboard, Prometheus rules). This test keeps the observability assets in
// sync with the code: every metric, label and label value referenced by
// deploy/grafana/*.json and deploy/prometheus/*.yml must exist in the
// registry built by internal/telemetry.
package deploy_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"text/template"
	"time"
	"unicode"

	"github.com/goccy/go-yaml"

	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/metrics"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/telemetry"
)

// targetLabels are attached by Prometheus to every scraped series.
var targetLabels = map[string]bool{"job": true, "instance": true}

// series is one queryable series name: a counter/gauge family, or the
// _bucket/_sum/_count series of a histogram.
type series struct {
	family string
	labels map[string]bool
	help   string
}

// registry builds telemetry.New's registry, creates one child of every
// labelled family (arity found by trial: With panics on a wrong count) and
// reads names, types and label names back from the text exposition.
func registry(t *testing.T) map[string]*series {
	t.Helper()
	m := telemetry.New("test")
	v := reflect.ValueOf(m).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if !f.CanInterface() {
			continue
		}
		var with func(vals ...string)
		switch x := f.Interface().(type) {
		case *metrics.CounterVec:
			with = func(vals ...string) { x.With(vals...) }
		case *metrics.GaugeVec:
			with = func(vals ...string) { x.With(vals...) }
		case *metrics.HistogramVec:
			with = func(vals ...string) { x.With(vals...) }
		default:
			continue
		}
		for n := 0; n <= 10; n++ {
			if tryWith(with, n) {
				break
			}
		}
	}
	var buf bytes.Buffer
	if err := m.Registry.WriteText(&buf); err != nil {
		t.Fatal(err)
	}

	types, helps := map[string]string{}, map[string]string{}
	sampleLabels := map[string]map[string]bool{}
	sampleRe := regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{(.*)\})? `)
	labelRe := regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="(?:[^"\\]|\\.)*"`)
	for _, line := range strings.Split(buf.String(), "\n") {
		switch {
		case strings.HasPrefix(line, "# HELP "):
			name, help, _ := strings.Cut(strings.TrimPrefix(line, "# HELP "), " ")
			helps[name] = help
		case strings.HasPrefix(line, "# TYPE "):
			name, typ, _ := strings.Cut(strings.TrimPrefix(line, "# TYPE "), " ")
			types[name] = typ
		case line == "" || strings.HasPrefix(line, "#"):
		default:
			sm := sampleRe.FindStringSubmatch(line)
			if sm == nil {
				t.Fatalf("unparsable exposition line %q", line)
			}
			ls := map[string]bool{}
			for _, lm := range labelRe.FindAllStringSubmatch(sm[2], -1) {
				ls[lm[1]] = true
			}
			sampleLabels[sm[1]] = ls
		}
	}

	out := map[string]*series{}
	for fam, typ := range types {
		names := []string{fam}
		if typ == "histogram" {
			names = []string{fam + "_bucket", fam + "_sum", fam + "_count"}
		}
		for _, n := range names {
			ls, ok := sampleLabels[n]
			if !ok {
				t.Fatalf("family %s (%s) has no sample after creating a child: extend the discovery in registry()", fam, typ)
			}
			out[n] = &series{family: fam, labels: ls, help: helps[fam]}
		}
	}
	if len(out) < 30 {
		t.Fatalf("only %d series discovered; telemetry.New changed shape?", len(out))
	}
	return out
}

func tryWith(with func(...string), n int) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	with(make([]string, n)...)
	return true
}

// knownValues lists label values that the HELP text does not spell out.
func knownValues(fam, label string) []string {
	switch {
	case label == "kind" && (fam == "reviewbot_problems_total" || fam == "reviewbot_problems_new_total"):
		var out []string
		for _, k := range problems.Kinds() {
			out = append(out, string(k.Kind))
		}
		return out
	case label == "kind":
		return []string{meter.KindReview, meter.KindChat, meter.KindVerification, meter.KindLearning,
			meter.KindOther, meter.KindCI, meter.KindMemory, meter.KindMap}
	case fam == "reviewbot_readiness_check_ok" && label == "check":
		return []string{"gitlab", "store", "docker"}
	case fam == "reviewbot_tool_runs_total" && label == "tool":
		return []string{"oasdiff", "buf", "graphql-inspector", "custom", "gitleaks"}
	case fam == "reviewbot_webhook_events_total" && label == "event":
		return []string{telemetry.EventMergeRequest, telemetry.EventNote, telemetry.EventPipeline, telemetry.EventOther}
	case fam == "reviewbot_webhook_events_total" && label == "result":
		return []string{telemetry.WebhookAccepted, telemetry.WebhookIgnored, telemetry.WebhookUnauthorized,
			telemetry.WebhookBadRequest, telemetry.WebhookMethodNotAllowed}
	}
	return nil
}

var simpleValueRe = regexp.MustCompile(`^[a-z0-9_]+(\|[a-z0-9_]+)*$`)

// valueOK: the value is a known constant or a whole word of the family's
// HELP text (which documents the enumerations). Regexps that are not a
// plain alternation of words, and free-form labels, are not checked.
func valueOK(s *series, label, value string) bool {
	if targetLabels[label] || label == "model" || label == "method" || label == "le" ||
		label == "version" || label == "go_version" || strings.Contains(value, "$") || !simpleValueRe.MatchString(value) {
		return true
	}
	known := knownValues(s.family, label)
	words := strings.FieldsFunc(s.help, func(r rune) bool { return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') })
	for _, v := range strings.Split(value, "|") {
		if !slices.Contains(known, v) && !slices.Contains(words, v) {
			return false
		}
	}
	return true
}

// --- a small PromQL scanner -------------------------------------------------

type token struct {
	kind string // ident, str, num, punct, var
	text string
}

func tokenize(expr string) ([]token, error) {
	var out []token
	rs := []rune(expr)
	for i := 0; i < len(rs); {
		c := rs[i]
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '#':
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
		case c == '"' || c == '\'' || c == '`':
			j := i + 1
			for j < len(rs) && rs[j] != c {
				if rs[j] == '\\' && c != '`' {
					j++
				}
				j++
			}
			if j >= len(rs) {
				return nil, fmt.Errorf("unterminated string at %d", i)
			}
			out = append(out, token{"str", string(rs[i+1 : j])})
			i = j + 1
		case c == '[':
			j := i
			for j < len(rs) && rs[j] != ']' {
				j++
			}
			if j >= len(rs) {
				return nil, fmt.Errorf("unterminated [ at %d", i)
			}
			i = j + 1 // range / subquery: nothing to check inside
		case c == '$':
			j := i + 1
			for j < len(rs) && (rs[j] == '_' || unicode.IsLetter(rs[j]) || unicode.IsDigit(rs[j])) {
				j++
			}
			out = append(out, token{"var", string(rs[i:j])})
			i = j
		case c == '_' || c == ':' || unicode.IsLetter(c):
			j := i
			for j < len(rs) && (rs[j] == '_' || rs[j] == ':' || unicode.IsLetter(rs[j]) || unicode.IsDigit(rs[j])) {
				j++
			}
			out = append(out, token{"ident", string(rs[i:j])})
			i = j
		case unicode.IsDigit(c) || c == '.':
			j := i
			for j < len(rs) && (unicode.IsDigit(rs[j]) || rs[j] == '.' || rs[j] == 'e' ||
				((rs[j] == '+' || rs[j] == '-') && j > i && rs[j-1] == 'e')) {
				j++
			}
			out = append(out, token{"num", string(rs[i:j])})
			i = j
		default:
			if i+1 < len(rs) {
				two := string(rs[i : i+2])
				if two == "=~" || two == "!=" || two == "!~" || two == "==" || two == ">=" || two == "<=" {
					out = append(out, token{"punct", two})
					i += 2
					continue
				}
			}
			out = append(out, token{"punct", string(c)})
			i++
		}
	}
	return out, nil
}

var keywords = map[string]bool{
	"by": true, "without": true, "on": true, "ignoring": true, "group_left": true, "group_right": true,
	"and": true, "or": true, "unless": true, "bool": true, "offset": true, "inf": true, "nan": true,
}

var aggregators = map[string]bool{
	"sum": true, "min": true, "max": true, "avg": true, "count": true, "stddev": true, "stdvar": true, "group": true,
	"topk": true, "bottomk": true, "quantile": true, "count_values": true, "limitk": true, "limit_ratio": true,
}

type ref struct {
	name string
	pos  int
}

type clause struct {
	kw     string
	labels []string
	end    int // index of the token after ")"
}

// checkExpr validates one PromQL expression against reg.
func checkExpr(reg map[string]*series, expr string) []string {
	var errs []string
	fail := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	// Grafana's label_values(selector, label).
	if m := regexp.MustCompile(`^\s*label_values\((.*),\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\)\s*$`).FindStringSubmatch(expr); m != nil {
		errs = append(errs, checkExpr(reg, m[1])...)
		toks, _ := tokenize(m[1])
		for _, tk := range toks {
			if s, ok := reg[tk.text]; tk.kind == "ident" && ok && !s.labels[m[2]] && !targetLabels[m[2]] {
				fail("label_values: %s has no label %q", tk.text, m[2])
			}
		}
		return errs
	}

	toks, err := tokenize(expr)
	if err != nil {
		return []string{err.Error()}
	}
	lookup := func(name string) (*series, bool) {
		if name == "up" {
			return &series{family: "up", labels: map[string]bool{}}, true
		}
		s, ok := reg[name]
		return s, ok
	}
	matchingParen := func(i int) int { // toks[i] is "("
		depth := 0
		for j := i; j < len(toks); j++ {
			switch toks[j].text {
			case "(":
				depth++
			case ")":
				depth--
				if depth == 0 {
					return j
				}
			}
		}
		return -1
	}

	var refs []ref
	var clauses []clause
	for i := 0; i < len(toks); i++ {
		tk := toks[i]
		if tk.kind == "punct" && tk.text == "{" {
			fail("selector without a metric name at token %d", i)
			continue
		}
		if tk.kind != "ident" {
			continue
		}
		low := strings.ToLower(tk.text)
		if keywords[low] {
			if (low == "by" || low == "without" || low == "on" || low == "ignoring" || low == "group_left" || low == "group_right") &&
				i+1 < len(toks) && toks[i+1].text == "(" {
				end := matchingParen(i + 1)
				if end < 0 {
					fail("unbalanced %s(", low)
					return errs
				}
				var ls []string
				for _, x := range toks[i+2 : end] {
					if x.kind == "ident" {
						ls = append(ls, x.text)
					}
				}
				clauses = append(clauses, clause{kw: low, labels: ls, end: end + 1})
				i = end
			}
			continue
		}
		if aggregators[low] || (i+1 < len(toks) && toks[i+1].text == "(") {
			continue // aggregation or function
		}
		s, ok := lookup(tk.text)
		if !ok {
			fail("unknown metric %q", tk.text)
		}
		refs = append(refs, ref{tk.text, i})
		if i+1 < len(toks) && toks[i+1].text == "{" {
			j := i + 2
			for j < len(toks) && toks[j].text != "}" {
				if toks[j].text == "," {
					j++
					continue
				}
				if j+2 >= len(toks) || toks[j].kind != "ident" || toks[j+2].kind != "str" {
					fail("%s: cannot parse matcher at token %d", tk.text, j)
					break
				}
				label, op, val := toks[j].text, toks[j+1].text, toks[j+2].text
				switch {
				case !ok:
				case targetLabels[label]:
				case !s.labels[label]:
					fail("%s has no label %q", tk.text, label)
				case !valueOK(s, label, val):
					fail("%s{%s%s%q}: value not among the known values of %s", tk.text, label, op, val, label)
				}
				j += 3
			}
			i = j
		}
	}

	for _, c := range clauses {
		var scope []ref
		switch c.kw {
		case "by", "without":
			if c.end >= len(toks) || toks[c.end].text != "(" {
				fail("%s (%s): write the aggregation in prefix form `sum %s (...) (expr)`", c.kw, strings.Join(c.labels, ", "), c.kw)
				continue
			}
			end := matchingParen(c.end)
			for _, r := range refs {
				if r.pos > c.end && r.pos < end {
					scope = append(scope, r)
				}
			}
		default: // on / ignoring / group_*: every metric of the expression
			scope = refs
		}
		for _, l := range c.labels {
			if targetLabels[l] {
				continue
			}
			for _, r := range scope {
				s, ok := lookup(r.name)
				if !ok {
					continue
				}
				if !s.labels[l] {
					fail("%s (%s): %s has no label %q", c.kw, strings.Join(c.labels, ", "), r.name, l)
				}
			}
		}
	}
	return errs
}

// --- assets -----------------------------------------------------------------

// collectJSON gathers PromQL from a Grafana dashboard: every "expr", and the
// "query"/"definition" of query variables.
func collectJSON(v any, key string, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			collectJSON(vv, k, out)
		}
	case []any:
		for _, vv := range x {
			collectJSON(vv, key, out)
		}
	case string:
		if key == "expr" || key == "definition" || (key == "query" && strings.Contains(x, "(")) {
			*out = append(*out, x)
		}
	}
}

type rulesFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Alert       string            `yaml:"alert"`
			Record      string            `yaml:"record"`
			Expr        string            `yaml:"expr"`
			For         string            `yaml:"for"`
			Labels      map[string]string `yaml:"labels"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDashboardMetricsExist(t *testing.T) {
	reg := registry(t)
	files, _ := filepath.Glob("grafana/*.json")
	if len(files) == 0 {
		t.Fatal("no dashboards in deploy/grafana")
	}
	for _, f := range files {
		var d map[string]any
		if err := json.Unmarshal(readFile(t, f), &d); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		var exprs []string
		collectJSON(d, "", &exprs)
		if len(exprs) < 20 {
			t.Fatalf("%s: only %d queries found", f, len(exprs))
		}
		for _, e := range exprs {
			for _, msg := range checkExpr(reg, e) {
				t.Errorf("%s: %s\n  in: %s", f, msg, e)
			}
		}
	}
}

func TestDashboardShape(t *testing.T) {
	var d struct {
		Title         string           `json:"title"`
		UID           string           `json:"uid"`
		SchemaVersion int              `json:"schemaVersion"`
		Inputs        []map[string]any `json:"__inputs"`
		Panels        []map[string]any `json:"panels"`
		Templating    struct {
			List []map[string]any `json:"list"`
		} `json:"templating"`
	}
	if err := json.Unmarshal(readFile(t, "grafana/reviewbot-dashboard.json"), &d); err != nil {
		t.Fatal(err)
	}
	if d.Title != "GitLab Review Bot" || d.UID == "" || d.SchemaVersion < 36 {
		t.Errorf("title/uid/schemaVersion: %q %q %d", d.Title, d.UID, d.SchemaVersion)
	}
	if len(d.Inputs) != 1 || d.Inputs[0]["name"] != "DS_PROMETHEUS" {
		t.Errorf("__inputs must declare DS_PROMETHEUS: %v", d.Inputs)
	}
	var vars []string
	for _, v := range d.Templating.List {
		vars = append(vars, fmt.Sprint(v["name"]))
	}
	if !slices.Equal(vars, []string{"datasource", "job", "instance"}) {
		t.Errorf("template variables = %v", vars)
	}
	ids := map[float64]bool{}
	cyr := regexp.MustCompile(`\p{Cyrillic}`)
	var walk func(ps []map[string]any)
	walk = func(ps []map[string]any) {
		for _, p := range ps {
			id, _ := p["id"].(float64)
			if ids[id] {
				t.Errorf("duplicate panel id %v", id)
			}
			ids[id] = true
			gp, _ := p["gridPos"].(map[string]any)
			if x, w := gp["x"].(float64), gp["w"].(float64); x+w > 24 {
				t.Errorf("panel %v %q overflows the grid", id, p["title"])
			}
			if p["type"] == "row" {
				if sub, ok := p["panels"].([]any); ok {
					var m []map[string]any
					for _, s := range sub {
						m = append(m, s.(map[string]any))
					}
					walk(m)
				}
				continue
			}
			if desc, _ := p["description"].(string); !cyr.MatchString(desc) {
				t.Errorf("panel %q: description must be in Russian", p["title"])
			}
			ds, _ := p["datasource"].(map[string]any)
			if ds["uid"] != "${datasource}" {
				t.Errorf("panel %q: datasource must be ${datasource}, got %v", p["title"], ds)
			}
			for _, tg := range p["targets"].([]any) {
				e, _ := tg.(map[string]any)["expr"].(string)
				if strings.Contains(e, "reviewbot_") && !strings.Contains(e, `instance=~"$instance"`) {
					t.Errorf("panel %q: query ignores $job/$instance: %s", p["title"], e)
				}
			}
		}
	}
	walk(d.Panels)
}

func TestAlertRules(t *testing.T) {
	reg := registry(t)
	files, _ := filepath.Glob("prometheus/*.yml")
	rules := 0
	for _, f := range files {
		var rf rulesFile
		if err := yaml.Unmarshal(readFile(t, f), &rf); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		names := map[string]bool{}
		for _, g := range rf.Groups {
			if g.Name == "" {
				t.Errorf("%s: group without a name", f)
			}
			for _, r := range g.Rules {
				rules++
				if r.Alert == "" || r.Expr == "" {
					t.Errorf("%s/%s: rule without alert or expr: %+v", f, g.Name, r)
					continue
				}
				if names[r.Alert] {
					t.Errorf("%s: duplicate alert %s", f, r.Alert)
				}
				names[r.Alert] = true
				if r.For != "" {
					if _, err := time.ParseDuration(r.For); err != nil {
						t.Errorf("%s: for: %v", r.Alert, err)
					}
				}
				if sev := r.Labels["severity"]; sev != "critical" && sev != "warning" && sev != "info" {
					t.Errorf("%s: severity %q", r.Alert, sev)
				}
				for _, k := range []string{"summary", "description"} {
					a := r.Annotations[k]
					if !regexp.MustCompile(`\p{Cyrillic}`).MatchString(a) {
						t.Errorf("%s: annotation %s must be set, in Russian", r.Alert, k)
					}
					// Prometheus defines $labels/$value and these functions.
					_, err := template.New(r.Alert).Funcs(template.FuncMap{
						"humanize": fmt.Sprint, "humanizePercentage": fmt.Sprint, "humanizeDuration": fmt.Sprint,
					}).Parse(`{{$labels := .Labels}}{{$value := .Value}}` + a)
					if err != nil {
						t.Errorf("%s: annotation %s: %v", r.Alert, k, err)
					}
					for _, m := range regexp.MustCompile(`\$labels\.([a-zA-Z_]+)`).FindAllStringSubmatch(a, -1) {
						if !regexp.MustCompile(`\b`+m[1]+`\b`).MatchString(r.Expr) && !targetLabels[m[1]] {
							t.Errorf("%s: annotation uses $labels.%s, which the expr does not keep", r.Alert, m[1])
						}
					}
				}
				for _, msg := range checkExpr(reg, r.Expr) {
					t.Errorf("%s %s: %s\n  in: %s", f, r.Alert, msg, r.Expr)
				}
			}
		}
	}
	if rules < 10 {
		t.Fatalf("only %d alert rules found in deploy/prometheus/*.yml", rules)
	}
}

func TestScrapeConfig(t *testing.T) {
	var sc struct {
		ScrapeConfigs []struct {
			JobName       string `yaml:"job_name"`
			MetricsPath   string `yaml:"metrics_path"`
			Authorization struct {
				Type string `yaml:"type"`
			} `yaml:"authorization"`
		} `yaml:"scrape_configs"`
	}
	if err := yaml.Unmarshal(readFile(t, "prometheus/scrape.yml"), &sc); err != nil {
		t.Fatal(err)
	}
	if len(sc.ScrapeConfigs) != 1 {
		t.Fatalf("scrape_configs: %+v", sc.ScrapeConfigs)
	}
	c := sc.ScrapeConfigs[0]
	if c.JobName != "reviewbot" || c.MetricsPath != telemetry.MetricsPath || c.Authorization.Type != "Bearer" {
		t.Errorf("scrape config: %+v", c)
	}
}

// TestNoUnknownReviewbotNames is a blunt sweep over the raw files: any
// reviewbot_* token anywhere (comments, legends, descriptions) must be a
// series of the registry.
func TestNoUnknownReviewbotNames(t *testing.T) {
	reg := registry(t)
	files, _ := filepath.Glob("grafana/*.json")
	more, _ := filepath.Glob("prometheus/*.yml")
	files = append(files, more...)
	re := regexp.MustCompile(`\breviewbot_[a-z0-9_]+`)
	used := map[string]bool{}
	for _, f := range files {
		for _, n := range re.FindAllString(string(readFile(t, f)), -1) {
			used[n] = true
			if _, ok := reg[n]; !ok {
				t.Errorf("%s: %s is not a registered series", f, n)
			}
		}
	}
	var fams []string
	for n, s := range reg {
		if strings.HasPrefix(n, "reviewbot_") && !used[n] && !used[s.family+"_bucket"] && !used[s.family+"_count"] && !used[s.family+"_sum"] {
			fams = append(fams, n)
		}
	}
	sort.Strings(fams)
	if len(fams) > 0 {
		t.Logf("series not used by any asset: %v", fams)
	}
}

// TestChecker guards the scanner itself against silently accepting typos.
func TestChecker(t *testing.T) {
	reg := registry(t)
	bad := []string{
		`reviewbot_jobs_totl`,
		`reviewbot_job_duration_seconds{kind="review"}`,
		`sum by (project) (rate(reviewbot_jobs_total[5m]))`,
		`sum(rate(reviewbot_jobs_total[5m])) by (kind)`,
		`reviewbot_jobs_total{statuss="ok"}`,
		`reviewbot_jobs_total{status="fail"}`,
		`reviewbot_problems_total{kind="no_such_kind"}`,
		`histogram_quantile(0.9, sum by (le) (rate(reviewbot_jobs_total[5m])))`,
		`label_values(reviewbot_build_info, project)`,
		`process_resident_memory_bytes`,
	}
	for _, e := range bad {
		if len(checkExpr(reg, e)) == 0 {
			t.Errorf("checker accepted %s", e)
		}
	}
	good := []string{
		`histogram_quantile(0.95, sum by (le, kind) (rate(reviewbot_job_duration_seconds_bucket{kind="review",job=~"$job"}[$__rate_interval])))`,
		`label_values(reviewbot_build_info{job=~"$job"}, instance)`,
		`up{job="reviewbot"} == 0`,
		`sum by (instance) (increase(reviewbot_claude_rate_limited_total[1h])) >= 3`,
		`sum by (kind) (increase(reviewbot_problems_new_total{kind=~"invalid_output|lint_failed"}[15m])) > 0`,
	}
	for _, e := range good {
		if errs := checkExpr(reg, e); len(errs) > 0 {
			t.Errorf("checker rejected %s: %v", e, errs)
		}
	}
}
