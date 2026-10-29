package problems_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
)

func TestImpacts(t *testing.T) {
	if got := problems.Impacts(); len(got) != 3 || got[0] != problems.ImpactBreaks || got[2] != problems.ImpactNone {
		t.Fatalf("impacts %v", got)
	}
	for i, im := range problems.Impacts() {
		if im.Rank() != i || !im.Valid() || im.Label() == "" {
			t.Errorf("%s: rank %d label %q", im, im.Rank(), im.Label())
		}
	}
	if bad := problems.Impact("bogus"); bad.Valid() || bad.Label() != "" || bad.Rank() != 3 {
		t.Error("unknown impact")
	}
	// The classes of the owner's decision: what breaks main or the review.
	for k, want := range map[problems.Kind]problems.Impact{
		problems.MainPipelineFailed: problems.ImpactBreaks, problems.RepoConfigInvalid: problems.ImpactBreaks,
		problems.ClaudeAuthFailed: problems.ImpactBreaks, problems.InvalidOutput: problems.ImpactBreaks,
		problems.DiffTruncated: problems.ImpactPartial, problems.ContextUnavailable: problems.ImpactPartial,
		problems.LabelsFailed: problems.ImpactNone, problems.ReactionsFailed: problems.ImpactNone,
		problems.AdvisorUnavailable: problems.ImpactNone, problems.SysmapFailed: problems.ImpactNone,
		problems.DeepReviewUnavailable: problems.ImpactNone, problems.DescriptionSummaryFailed: problems.ImpactNone,
		problems.SandboxImageMissing: problems.ImpactBreaks,
	} {
		if got := problems.InfoOf(k).Impact; got != want {
			t.Errorf("%s: impact %s, want %s", k, got, want)
		}
	}
}

// Every snippet that names a config file must be valid for it: a
// .reviewbot.yaml fragment passes ValidateRepo (unknown keys are errors
// there), a config.yaml fragment decodes into config.Service without
// unknown fields. Values that must be adapted are marked as an example.
func TestStepSnippetsAreValidConfig(t *testing.T) {
	var repo, service int
	for _, k := range problems.Kinds() {
		for i, st := range k.Steps {
			if strings.TrimSpace(st.Text) == "" || strings.Count(st.Text, "`")%2 != 0 {
				t.Errorf("%s step %d: empty text or unbalanced backticks: %q", k.Kind, i, st.Text)
			}
			if i == 0 && st.Or {
				t.Errorf("%s: the first step cannot be an alternative", k.Kind)
			}
			sn := st.Snippet
			if sn == nil {
				continue
			}
			if sn.File == "" || sn.Where == "" || strings.TrimSpace(sn.Code) == "" {
				t.Errorf("%s step %d: incomplete snippet %+v", k.Kind, i, sn)
			}
			if sn.Placeholder != "" && (!sn.Example || !strings.Contains(sn.Code, sn.Placeholder)) {
				t.Errorf("%s step %d: placeholder %q must be in the example code", k.Kind, i, sn.Placeholder)
			}
			// The check must bite at the snippet's deepest key too: the same
			// snippet with that key misspelled is rejected.
			typo := typoDeepest(sn.Code)
			switch sn.File {
			case problems.FileRepoConfig:
				repo++
				if _, errs := config.ValidateRepo([]byte(sn.Code)); len(errs) != 0 {
					t.Errorf("%s step %d: invalid .reviewbot.yaml snippet: %v\n%s", k.Kind, i, errs, sn.Code)
				}
				if _, errs := config.ValidateRepo([]byte(typo)); len(errs) == 0 {
					t.Errorf("%s step %d: a typo in the deepest key is not caught:\n%s", k.Kind, i, typo)
				}
			case problems.FileServiceConfig:
				service++
				if err := strictService(sn.Code); err != nil {
					t.Errorf("%s step %d: invalid config.yaml snippet: %v\n%s", k.Kind, i, err, sn.Code)
				}
				if strictService(typo) == nil {
					t.Errorf("%s step %d: a typo in the deepest key is not caught:\n%s", k.Kind, i, typo)
				}
			}
		}
	}
	if repo < 8 || service < 8 {
		t.Errorf("snippets checked: repo %d, service %d", repo, service)
	}
}

func strictService(code string) error {
	s := config.Defaults()
	return yaml.UnmarshalWithOptions([]byte(code), &s, yaml.DisallowUnknownField())
}

var keyLineRe = regexp.MustCompile(`(?m)^(\s*(?:- )?)([a-z_]+):`)

// typoDeepest appends "x" to the key of the last "key:" line.
func typoDeepest(code string) string {
	locs := keyLineRe.FindAllStringSubmatchIndex(code, -1)
	if len(locs) == 0 {
		return code
	}
	end := locs[len(locs)-1][5]
	return code[:end] + "x" + code[end:]
}

func TestComparePriority(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	rec := func(fp string, k problems.Kind, count int64, ago time.Duration) problems.Record {
		return problems.Record{Fingerprint: fp, Kind: k, Count: count, LastSeen: at.Add(-ago)}
	}
	acked := rec("acked", problems.MainPipelineFailed, 9, 0)
	acked.AckedAt = at
	fixed := rec("fixed", problems.RepoConfigInvalid, 9, 0)
	fixed.ResolvedAt = at
	rows := []problems.Record{
		fixed,
		rec("labels", problems.LabelsFailed, 50, 0),           // no impact
		rec("cov", problems.CoverageMissing, 1, 0),            // partial, info
		rec("diff-old", problems.DiffTruncated, 2, time.Hour), // partial, warn, older
		acked,
		rec("diff-new", problems.DiffTruncated, 2, 0),
		rec("diff-many", problems.DiffTruncated, 7, 5*time.Hour),
		rec("main", problems.MainPipelineFailed, 1, 9*time.Hour), // breaks
		rec("b", problems.DiffTruncated, 2, 0),
	}
	slices.SortFunc(rows, problems.ComparePriority)
	var got []string
	for _, r := range rows {
		got = append(got, r.Fingerprint)
	}
	want := "main,diff-many,b,diff-new,diff-old,cov,labels,acked,fixed"
	if strings.Join(got, ",") != want {
		t.Errorf("order %v, want %s", got, want)
	}
}
