package migrations

// The usage check: does the deployed code (the merge base) still use what a
// contract operation drops or renames? And does the merge request's own
// code (the head)? Read-only: a Searcher looks words up in the tree of a
// commit.

import (
	"context"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Hit is one line containing a searched word.
type Hit struct {
	Path string
	Line int
	Text string
}

// Searcher finds lines with any of the words (whole words, case
// sensitive) in the source files of commit rev. partial reports that a
// limit cut the search short.
type Searcher interface {
	Search(ctx context.Context, rev string, words []string) (hits []Hit, partial bool, err error)
}

// sourceExts are the files the usage check reads.
var sourceExts = map[string]bool{}

// SourcePathspecs are the git pathspecs of the files the usage check reads.
var SourcePathspecs []string

func init() {
	for _, e := range strings.Fields(`php py go java kt kts scala rb js mjs cjs jsx ts tsx vue svelte cs rs ex exs
		twig html htm jinja j2 tpl graphql gql sql xml yml yaml`) {
		sourceExts["."+e] = true
		SourcePathspecs = append(SourcePathspecs, "*."+e)
	}
}

var (
	testPathRe   = regexp.MustCompile(`(?i)(^|/)(tests?|spec|specs|__tests__|testdata|fixtures)/|_test\.(go|py)$|(^|/)test_[^/]*\.py$|Test\.php$|\.(spec|test)\.[jt]sx?$`)
	vendorPathRe = regexp.MustCompile(`(^|/)(vendor|node_modules|site-packages|\.git|dist|build|var/cache)/`)
	ormMapRe     = regexp.MustCompile(`\.(orm|dcm)\.(xml|ya?ml)$`)
	dumpRe       = regexp.MustCompile(`(?i)(^|/)(schema|structure|dump)[^/]*\.sql$`)
)

// IsSourceFile reports a file of application code the usage check reads:
// code, templates, ORM mappings and SQL (not tests, vendored code, schema
// dumps).
func IsSourceFile(p string) bool {
	ext := strings.ToLower(path.Ext(p))
	if !sourceExts[ext] || testPathRe.MatchString(p) || vendorPathRe.MatchString(p) {
		return false
	}
	switch ext {
	case ".xml", ".yml", ".yaml":
		return ormMapRe.MatchString(p)
	case ".sql":
		return !dumpRe.MatchString(p)
	}
	return true
}

// GrepSearcher searches with a core.WordGrepper (`git grep`).
type GrepSearcher struct {
	G core.WordGrepper
	// MaxBytes caps the grep output of one search.
	MaxBytes int
}

// Search implements Searcher.
func (g GrepSearcher) Search(ctx context.Context, rev string, words []string) ([]Hit, bool, error) {
	ms, partial, err := g.G.GrepWords(ctx, rev, words, SourcePathspecs, g.MaxBytes)
	if err != nil {
		return nil, false, err
	}
	hits := make([]Hit, 0, len(ms))
	for _, m := range ms {
		hits = append(hits, Hit{Path: m.Path, Line: m.Line, Text: m.Text})
	}
	return hits, partial, nil
}

// Tree is what TreeSearcher reads (core.Checkout has it).
type Tree interface {
	ListFiles(ctx context.Context, rev string) ([]string, error)
	ShowFile(ctx context.Context, rev, path string) ([]byte, error)
}

// TreeSearcher searches by reading files one by one: the fallback when the
// checkout cannot grep. Limits: MaxFiles source files, MaxBytes in total,
// files above MaxFileBytes skipped.
type TreeSearcher struct {
	Tree         Tree
	Keep         func(path string) bool
	MaxFiles     int
	MaxBytes     int
	MaxFileBytes int
}

// Search implements Searcher.
func (t TreeSearcher) Search(ctx context.Context, rev string, words []string) ([]Hit, bool, error) {
	re := wordsRe(words)
	if re == nil {
		return nil, false, nil
	}
	files, err := t.Tree.ListFiles(ctx, rev)
	if err != nil {
		return nil, false, err
	}
	var hits []Hit
	partial := false
	n, total := 0, 0
	for _, f := range files {
		if !IsSourceFile(f) || (t.Keep != nil && !t.Keep(f)) {
			continue
		}
		if ctx.Err() != nil || (t.MaxFiles > 0 && n >= t.MaxFiles) || (t.MaxBytes > 0 && total >= t.MaxBytes) {
			partial = true
			break
		}
		n++
		b, err := t.Tree.ShowFile(ctx, rev, f)
		if err != nil {
			continue
		}
		if t.MaxFileBytes > 0 && len(b) > t.MaxFileBytes {
			partial = true
			continue
		}
		total += len(b)
		for i, l := range strings.Split(string(b), "\n") {
			if re.MatchString(l) {
				hits = append(hits, Hit{Path: f, Line: i + 1, Text: l})
			}
		}
	}
	return hits, partial, nil
}

// wordsRe matches any of the words as a whole word.
func wordsRe(words []string) *regexp.Regexp {
	var qs []string
	for _, w := range words {
		if identRe.MatchString(w) {
			qs = append(qs, regexp.QuoteMeta(w))
		}
	}
	if len(qs) == 0 {
		return nil
	}
	sort.Strings(qs)
	return regexp.MustCompile(`\b(?:` + strings.Join(qs, "|") + `)\b`)
}

// UsageOptions configures CheckUsages.
type UsageOptions struct {
	Base, Head string
	// MaxUsages caps the locations kept per object (default 5).
	MaxUsages int
	// Keep filters the files whose lines count (e.g. not a migration).
	Keep func(path string) bool
	// MaxWords caps the words of one search (default 120).
	MaxWords int
}

type usageTarget struct {
	f   *Finding
	obj *regexp.Regexp
	tbl *regexp.Regexp // ambiguous names: the file must mention the table
}

// CheckUsages fills the usage check of every contract finding and
// recomputes the levels. A nil searcher or no base revision marks them
// unchecked.
func CheckUsages(ctx context.Context, s Searcher, res *Result, o UsageOptions) {
	if res == nil {
		return
	}
	defer res.setLevels()
	maxUsages := o.MaxUsages
	if maxUsages <= 0 {
		maxUsages = 5
	}
	maxWords := o.MaxWords
	if maxWords <= 0 {
		maxWords = 120
	}
	targets, words := usageTargets(res, maxWords)
	if len(targets) == 0 {
		return
	}
	if s == nil || o.Base == "" {
		msg := "поиск недоступен"
		if s != nil {
			msg = "нет merge base: задеплоенный код не проверен"
		}
		for _, t := range targets {
			t.f.Usage.Err = msg
		}
		return
	}
	keep := func(p string) bool { return IsSourceFile(p) && (o.Keep == nil || o.Keep(p)) }
	for _, rev := range []string{o.Base, o.Head} {
		if rev == "" {
			continue
		}
		hits, partial, err := s.Search(ctx, rev, words)
		paths, byPath := groupHits(hits, keep)
		for _, t := range targets {
			u := t.f.Usage
			if err != nil {
				u.Err = "поиск не удался: " + oneLineShort(err.Error(), 120)
				continue
			}
			u.Partial = u.Partial || partial
			list, total := t.usages(paths, byPath, maxUsages)
			if rev == o.Base {
				u.Base, u.BaseTotal = list, total
			} else {
				u.Head, u.HeadTotal = list, total
			}
		}
	}
	for _, t := range targets {
		t.f.Usage.Checked = t.f.Usage.Err == ""
	}
}

// usageTargets starts the usage check of every contract finding and
// returns the searchable ones with every word to search for (at most
// maxWords; findings beyond that, or with a name that is not an
// identifier, get an error instead).
func usageTargets(res *Result, maxWords int) (targets []usageTarget, words []string) {
	for i := range res.Findings {
		f := &res.Findings[i]
		r := f.Rule()
		if r == nil || !r.Contract {
			continue
		}
		u := &UsageCheck{}
		f.Usage = u
		var obj, tbl []string
		if f.Column != "" {
			obj = columnVariants(f.Column)
			if generic(f.Column) {
				u.Ambiguous = true
				tbl = tableVariants(f.Table)
			}
		} else {
			obj = tableVariants(f.Table)
			u.Ambiguous = generic(f.Table)
		}
		if len(obj) == 0 {
			u.Err = "имя нельзя искать как идентификатор"
			continue
		}
		if len(words)+len(obj)+len(tbl) > maxWords {
			u.Err = "слишком много объектов для поиска"
			continue
		}
		u.Words = obj
		words = addUnique(words, obj...)
		words = addUnique(words, tbl...)
		targets = append(targets, usageTarget{f: f, obj: wordsRe(obj), tbl: wordsRe(tbl)})
	}
	return targets, words
}

// groupHits groups the hits in files keep accepts by path; paths are sorted.
func groupHits(hits []Hit, keep func(string) bool) (paths []string, byPath map[string][]Hit) {
	byPath = map[string][]Hit{}
	for _, h := range hits {
		if !keep(h.Path) {
			continue
		}
		if _, ok := byPath[h.Path]; !ok {
			paths = append(paths, h.Path)
		}
		byPath[h.Path] = append(byPath[h.Path], h)
	}
	sort.Strings(paths)
	return paths, byPath
}

// usages counts the code lines using the target's object and keeps the
// first maxUsages of them.
func (t usageTarget) usages(paths []string, byPath map[string][]Hit, maxUsages int) (list []Usage, total int) {
	for _, p := range paths {
		hs := byPath[p]
		if t.tbl != nil && !anyMatch(hs, t.tbl) {
			continue
		}
		for _, h := range hs {
			if commentLine(h.Text) || !t.obj.MatchString(h.Text) {
				continue
			}
			total++
			if len(list) < maxUsages {
				list = append(list, Usage{Path: h.Path, Line: h.Line, Snippet: oneLineShort(h.Text, 160)})
			}
		}
	}
	return list, total
}

// commentLine reports a line that is only a comment (//, #, --, /* and
// docblock lines; PHP attributes #[...] are code).
func commentLine(s string) bool {
	t := strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(t, "#["):
		return false
	case strings.HasPrefix(t, "//"), strings.HasPrefix(t, "#"), strings.HasPrefix(t, "--"),
		strings.HasPrefix(t, "/*"), strings.HasPrefix(t, "*"), strings.HasPrefix(t, "{#"):
		return true
	}
	return false
}

func anyMatch(hs []Hit, re *regexp.Regexp) bool {
	for _, h := range hs {
		if re.MatchString(h.Text) {
			return true
		}
	}
	return false
}

// oneLineShort collapses whitespace and cuts to n runes.
func oneLineShort(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
