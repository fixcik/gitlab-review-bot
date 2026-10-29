package migrations

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// revSearcher returns, per revision, the hits whose text contains one of
// the searched words (as a real searcher would) plus a partial flag or an
// error.
type revSearcher struct {
	hits    map[string][]Hit
	partial map[string]bool
	err     map[string]error
	calls   [][]string
}

func (s *revSearcher) Search(_ context.Context, rev string, words []string) ([]Hit, bool, error) {
	s.calls = append(s.calls, append([]string{rev}, words...))
	if err := s.err[rev]; err != nil {
		return nil, false, err
	}
	re := wordsRe(words)
	var out []Hit
	for _, h := range s.hits[rev] {
		if re != nil && re.MatchString(h.Text) {
			out = append(out, h)
		}
	}
	return out, s.partial[rev], nil
}

func analyzeSQLFile(t *testing.T, sql string) *Result {
	t.Helper()
	return Analyze([]Input{{Path: "db/migrations/1.up.sql", New: true, Content: []byte(sql)}}, Config{})
}

func findingByObject(t *testing.T, res *Result, obj string) Finding {
	t.Helper()
	for _, f := range res.Findings {
		if f.Object() == obj {
			return f
		}
	}
	t.Fatalf("no finding for %s in %s", obj, findingsStr(res.Findings))
	return Finding{}
}

// TestCheckUsagesLevels: the level of a contract finding follows the usage
// check — deployed use of a specific name is critical, of a generic name
// major, use left only in the MR's code major, no use info.
func TestCheckUsagesLevels(t *testing.T) {
	res := analyzeSQLFile(t, "ALTER TABLE orders DROP COLUMN created_by, DROP COLUMN status, DROP COLUMN legacy_flag, DROP COLUMN unused_col;\n")
	s := &revSearcher{hits: map[string][]Hit{
		"base": {
			{Path: "src/Order.php", Line: 3, Text: "$o->createdBy = 1; // Order"},
			{Path: "src/Order.php", Line: 4, Text: "$o->status = 'x';"},
		},
		"head": {
			{Path: "src/Job.php", Line: 7, Text: "$row['legacy_flag'] = 1;"},
		},
	}}
	CheckUsages(context.Background(), s, res, UsageOptions{Base: "base", Head: "head"})
	for obj, want := range map[string]Level{
		"orders.created_by": LevelCritical, "orders.status": LevelMajor,
		"orders.legacy_flag": LevelMajor, "orders.unused_col": LevelInfo,
	} {
		f := findingByObject(t, res, obj)
		if f.Level != want || !f.Usage.Checked {
			t.Errorf("%s: level %s checked %v, want %s (%+v)", obj, f.Level, f.Usage.Checked, want, f.Usage)
		}
	}
	lf := findingByObject(t, res, "orders.legacy_flag")
	if lf.Usage.BaseTotal != 0 || lf.Usage.HeadTotal != 1 || len(lf.Usage.Head) != 1 || lf.Usage.Head[0].Path != "src/Job.php" || lf.BreaksOldCode() {
		t.Errorf("legacy_flag usage %+v", lf.Usage)
	}
	if st := findingByObject(t, res, "orders.status"); !st.Usage.Ambiguous || st.Usage.BaseTotal != 1 {
		t.Errorf("status usage %+v", st.Usage)
	}
	if cb := findingByObject(t, res, "orders.created_by"); !reflect.DeepEqual(cb.Usage.Words, []string{"created_by", "createdBy", "CreatedBy"}) {
		t.Errorf("words %v", cb.Usage.Words)
	}
	// One search per revision, with every object's words (and the table
	// words of the generic name) deduplicated.
	if len(s.calls) != 2 || s.calls[0][0] != "base" || s.calls[1][0] != "head" {
		t.Fatalf("calls %v", s.calls)
	}
	words := strings.Join(s.calls[0][1:], " ")
	for _, w := range []string{"created_by", "status", "orders", "Order", "legacy_flag", "unused_col"} {
		if !strings.Contains(" "+words+" ", " "+w+" ") {
			t.Errorf("word %q not searched: %s", w, words)
		}
	}
	if strings.Count(" "+words+" ", " status ") != 1 {
		t.Errorf("duplicate words: %s", words)
	}
}

// TestCheckUsagesFilters: comment-only lines, files the caller does not
// keep and non-source files (tests, vendor) never count; the location list
// is capped and sorted by path while the total counts every use.
func TestCheckUsagesFilters(t *testing.T) {
	res := analyzeSQLFile(t, "ALTER TABLE orders DROP COLUMN created_by;\n")
	s := &revSearcher{hits: map[string][]Hit{"base": {
		{Path: "src/z.php", Line: 1, Text: "$a->created_by;"},
		{Path: "src/b.php", Line: 2, Text: "    // created_by legacy"},
		{Path: "src/b.php", Line: 3, Text: " * @var createdBy"},
		{Path: "src/b.php", Line: 4, Text: "#[Column(name: 'created_by')]"},
		{Path: "src/b.php", Line: 5, Text: "# created_by"},
		{Path: "src/b.py", Line: 1, Text: "-- created_by"},
		{Path: "templates/a.twig", Line: 1, Text: "{# createdBy #}"},
		{Path: "src/a.php", Line: 9, Text: "return $this->createdBy;"},
		{Path: "src/a.php", Line: 10, Text: "/* createdBy */"},
		{Path: "src/skip.php", Line: 1, Text: "$x->createdBy;"},
		{Path: "tests/a.php", Line: 1, Text: "$x->createdBy;"},
		{Path: "README.md", Line: 1, Text: "createdBy"},
		{Path: "src/c.php", Line: 1, Text: "$c->CreatedBy();"},
	}}}
	CheckUsages(context.Background(), s, res, UsageOptions{Base: "base", MaxUsages: 2,
		Keep: func(p string) bool { return p != "src/skip.php" }})
	u := res.Findings[0].Usage
	if u.BaseTotal != 4 || !u.Checked || u.Partial {
		t.Fatalf("usage %+v", u)
	}
	want := []Usage{{Path: "src/a.php", Line: 9, Snippet: "return $this->createdBy;"}, {Path: "src/b.php", Line: 4, Snippet: "#[Column(name: 'created_by')]"}}
	if !reflect.DeepEqual(u.Base, want) {
		t.Errorf("base %+v\nwant %+v", u.Base, want)
	}
	if u.Head != nil || u.HeadTotal != 0 {
		t.Errorf("head searched without a head revision: %+v", u)
	}
	if len(s.calls) != 1 {
		t.Errorf("calls %v", s.calls)
	}
}

// TestCheckUsagesDefaultCap: without MaxUsages five locations are kept.
func TestCheckUsagesDefaultCap(t *testing.T) {
	res := analyzeSQLFile(t, "DROP TABLE invoices;\n")
	var hits []Hit
	for i := 0; i < 7; i++ {
		hits = append(hits, Hit{Path: fmt.Sprintf("src/f%d.php", i), Line: 1, Text: "new Invoice();"})
	}
	CheckUsages(context.Background(), &revSearcher{hits: map[string][]Hit{"base": hits}}, res, UsageOptions{Base: "base"})
	u := res.Findings[0].Usage
	if len(u.Base) != 5 || u.BaseTotal != 7 || res.Findings[0].Level != LevelCritical {
		t.Errorf("usage %+v", u)
	}
}

// TestCheckUsagesPartialAndErrors: a partial search is flagged; a failing
// search (on either revision) leaves the finding unchecked, hence major.
func TestCheckUsagesPartialAndErrors(t *testing.T) {
	res := analyzeSQLFile(t, "DROP TABLE invoices;\n")
	CheckUsages(context.Background(), &revSearcher{partial: map[string]bool{"head": true}}, res, UsageOptions{Base: "base", Head: "head"})
	if u := res.Findings[0].Usage; !u.Partial || !u.Checked || res.Findings[0].Level != LevelInfo {
		t.Errorf("partial: %+v level %s", u, res.Findings[0].Level)
	}

	res = analyzeSQLFile(t, "DROP TABLE invoices;\n")
	CheckUsages(context.Background(), &revSearcher{
		hits: map[string][]Hit{"base": {{Path: "src/a.php", Line: 1, Text: "new Invoice();"}}},
		err:  map[string]error{"head": errors.New("grep\n  failed")},
	}, res, UsageOptions{Base: "base", Head: "head"})
	f := res.Findings[0]
	if f.Usage.Checked || f.Level != LevelMajor || f.BreaksOldCode() || f.Usage.Err != "поиск не удался: grep failed" {
		t.Errorf("head error: %+v level %s", f.Usage, f.Level)
	}
}

// TestCheckUsagesUnsearchable: a name that is not an identifier, or more
// words than MaxWords, is reported as an error (unchecked, major) without
// affecting the other findings.
func TestCheckUsagesUnsearchable(t *testing.T) {
	res := analyzeSQLFile(t, "DROP TABLE \"2024_orders\";\nDROP TABLE invoices;\nDROP TABLE payments;\n")
	CheckUsages(context.Background(), &revSearcher{}, res, UsageOptions{Base: "base", MaxWords: 3})
	bad := findingByObject(t, res, "2024_orders")
	if bad.Usage.Checked || bad.Usage.Err == "" || bad.Level != LevelMajor {
		t.Errorf("bad name: %+v %s", bad.Usage, bad.Level)
	}
	inv := findingByObject(t, res, "invoices")
	if !inv.Usage.Checked || inv.Level != LevelInfo {
		t.Errorf("invoices (2 words, fits): %+v %s", inv.Usage, inv.Level)
	}
	pay := findingByObject(t, res, "payments")
	if pay.Usage.Checked || pay.Usage.Err == "" || pay.Level != LevelMajor {
		t.Errorf("payments (over max words): %+v %s", pay.Usage, pay.Level)
	}

	// Default MaxWords (120) fits a handful of objects.
	res = analyzeSQLFile(t, "DROP TABLE a1x, b2x, c3x, d4x, e5x;\n")
	CheckUsages(context.Background(), &revSearcher{}, res, UsageOptions{Base: "base"})
	for _, f := range res.Findings {
		if !f.Usage.Checked {
			t.Errorf("%s unchecked: %+v", f.Object(), f.Usage)
		}
	}
}

// TestCheckUsagesNonContract: only contract findings get a usage check.
func TestCheckUsagesNonContract(t *testing.T) {
	res := analyzeSQLFile(t, "CREATE INDEX i ON t (a);\n")
	CheckUsages(context.Background(), nil, res, UsageOptions{Base: "base"})
	if res.Findings[0].Usage != nil || res.Findings[0].Level != LevelMajor {
		t.Errorf("%+v", res.Findings[0])
	}
	CheckUsages(context.Background(), nil, nil, UsageOptions{}) // nil result: no panic
}

// TestCheckUsagesGenericTable: a generic table name is flagged ambiguous
// (major, not critical) even when the deployed code uses it.
func TestCheckUsagesGenericTable(t *testing.T) {
	res := analyzeSQLFile(t, "DROP TABLE settings;\n")
	CheckUsages(context.Background(), &revSearcher{hits: map[string][]Hit{"base": {{Path: "src/a.py", Line: 1, Text: "settings = 1"}}}},
		res, UsageOptions{Base: "base"})
	f := res.Findings[0]
	if !f.Usage.Ambiguous || f.Usage.BaseTotal != 1 || f.Level != LevelMajor {
		t.Errorf("%+v %s", f.Usage, f.Level)
	}
}

type memTree struct {
	files map[string]string
	order []string
	errs  map[string]bool
}

func (m memTree) ListFiles(context.Context, string) ([]string, error) { return m.order, nil }
func (m memTree) ShowFile(_ context.Context, _, p string) ([]byte, error) {
	if m.errs[p] {
		return nil, errors.New("unreadable")
	}
	return []byte(m.files[p]), nil
}

// TestTreeSearcherLimits: the fallback searcher reads only kept source
// files and reports partial results when a limit is hit.
func TestTreeSearcherLimits(t *testing.T) {
	tree := memTree{
		files: map[string]string{
			"a.php": "x\n$o->createdBy;\n", "b.php": "created_by\n", "big.php": strings.Repeat("created_by ", 20),
			"c.php": "created_by\n", "tests/d.php": "created_by\n", "skip.php": "created_by\n", "bad.php": "",
		},
		order: []string{"tests/d.php", "skip.php", "bad.php", "a.php", "big.php", "b.php", "c.php"},
		errs:  map[string]bool{"bad.php": true},
	}
	words := []string{"created_by", "createdBy"}
	keep := func(p string) bool { return p != "skip.php" }
	paths := func(hs []Hit) string {
		var out []string
		for _, h := range hs {
			out = append(out, fmt.Sprintf("%s:%d", h.Path, h.Line))
		}
		return strings.Join(out, ",")
	}

	hits, partial, err := TreeSearcher{Tree: tree, Keep: keep}.Search(context.Background(), "r", words)
	if err != nil || partial || paths(hits) != "a.php:2,big.php:1,b.php:1,c.php:1" {
		t.Errorf("no limits: %s %v %v", paths(hits), partial, err)
	}
	hits, partial, _ = TreeSearcher{Tree: tree, Keep: keep, MaxFileBytes: 100}.Search(context.Background(), "r", words)
	if !partial || paths(hits) != "a.php:2,b.php:1,c.php:1" {
		t.Errorf("max file bytes: %s %v", paths(hits), partial)
	}
	// bad.php counts as a read file; the fourth is not read.
	hits, partial, _ = TreeSearcher{Tree: tree, Keep: keep, MaxFiles: 3}.Search(context.Background(), "r", words)
	if !partial || paths(hits) != "a.php:2,big.php:1" {
		t.Errorf("max files: %s %v", paths(hits), partial)
	}
	hits, partial, _ = TreeSearcher{Tree: tree, Keep: keep, MaxBytes: 18}.Search(context.Background(), "r", words)
	if !partial || paths(hits) != "a.php:2,big.php:1" {
		t.Errorf("max bytes: %s %v", paths(hits), partial)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if hits, partial, _ = (TreeSearcher{Tree: tree}).Search(ctx, "r", words); !partial || len(hits) != 0 {
		t.Errorf("cancelled: %s %v", paths(hits), partial)
	}
	if hits, partial, err = (TreeSearcher{Tree: tree}).Search(context.Background(), "r", []string{"not an ident!"}); hits != nil || partial || err != nil {
		t.Errorf("no searchable words: %v %v %v", hits, partial, err)
	}
}

func TestCommentLineAndShort(t *testing.T) {
	for line, want := range map[string]bool{
		"  // x": true, "# x": true, "-- x": true, "/* x": true, " * x": true, "{# x #}": true,
		"#[ORM\\Column]": false, "$x = 1; // y": false, "x # y": false, "": false, "a - b": false,
	} {
		if commentLine(line) != want {
			t.Errorf("commentLine(%q) = %v", line, !want)
		}
	}
	if got := oneLineShort("a\n\t b   c", 10); got != "a b c" {
		t.Errorf("collapse: %q", got)
	}
	if got := oneLineShort("абвгдеж", 5); got != "абвг…" {
		t.Errorf("cut: %q", got)
	}
	if got := oneLineShort("абвгд", 5); got != "абвгд" {
		t.Errorf("exact length: %q", got)
	}
}

// TestCheckUsagesMixedFindings: a non-contract finding sorted before a
// contract one does not stop the usage check of the rest.
func TestCheckUsagesMixedFindings(t *testing.T) {
	res := analyzeSQLFile(t, "CREATE INDEX i ON t (a);\nDROP TABLE invoices;\nTRUNCATE x;\nALTER TABLE t DROP COLUMN legacy_col;\n")
	CheckUsages(context.Background(), &revSearcher{}, res, UsageOptions{Base: "base"})
	for _, obj := range []string{"invoices", "t.legacy_col"} {
		if f := findingByObject(t, res, obj); f.Usage == nil || !f.Usage.Checked {
			t.Errorf("%s not checked: %+v", obj, f.Usage)
		}
	}
}

// TestCheckUsagesNoBase: without the merge base the deployed code cannot be
// checked: unchecked (major), never "no use found" (info).
func TestCheckUsagesNoBase(t *testing.T) {
	res := analyzeSQLFile(t, "DROP TABLE invoices;\n")
	s := &revSearcher{}
	CheckUsages(context.Background(), s, res, UsageOptions{Head: "head"})
	f := res.Findings[0]
	if f.Usage.Checked || f.Usage.Err == "" || f.Level != LevelMajor || len(s.calls) != 0 {
		t.Errorf("%+v %s calls %v", f.Usage, f.Level, s.calls)
	}
}
