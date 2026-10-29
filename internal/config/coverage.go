package config

import (
	"fmt"
	"strings"
)

// Coverage configures diff coverage from CI artifacts (review.coverage):
// after the MR pipeline succeeds, the bot downloads the coverage report(s)
// named in the repo's .reviewbot.yaml and shows the coverage of the changed
// lines in the summary; Claude gets the report and the diffcov tool.
type Coverage struct {
	// Enabled turns the feature on for the service (default true); repos
	// still have to name their report in .reviewbot.yaml.
	Enabled bool `yaml:"enabled"`
	// MaxReportMB caps one downloaded report.
	MaxReportMB int `yaml:"max_report_mb"`
}

func defaultCoverage() Coverage { return Coverage{Enabled: true, MaxReportMB: 50} }

func (c Coverage) validate() []string {
	if c.Enabled && (c.MaxReportMB < 1 || c.MaxReportMB > 100) {
		return []string{"review.coverage.max_report_mb must be between 1 and 100"}
	}
	return nil
}

// MaxReportBytes is MaxReportMB in bytes.
func (c Coverage) MaxReportBytes() int64 { return int64(c.MaxReportMB) << 20 }

// CoverageReport names one report artifact: the CI job and the file path
// inside its artifacts.
type CoverageReport struct {
	Job  string `yaml:"job"`
	Path string `yaml:"path"`
}

// RepoCoverage is the coverage section of .reviewbot.yaml. Either Job+Path
// (one report) or Reports (several jobs, merged: a line is covered when any
// report covers it). A job name also matches its parallel instances
// ("tests 1/3").
type RepoCoverage struct {
	Job     string           `yaml:"job"`
	Path    string           `yaml:"path"`
	Reports []CoverageReport `yaml:"reports"`
	// Min is the warning threshold in percent of covered changed lines
	// (0 = no warning). Never blocks the merge.
	Min float64 `yaml:"min"`
}

// MaxCoverageReports caps coverage.reports.
const MaxCoverageReports = 10

func (r *RepoCoverage) normalize() error {
	if r == nil {
		return nil
	}
	if r.Job != "" || r.Path != "" {
		r.Reports = append([]CoverageReport{{Job: r.Job, Path: r.Path}}, r.Reports...)
		r.Job, r.Path = "", ""
	}
	if len(r.Reports) > MaxCoverageReports {
		return fmt.Errorf("coverage.reports: at most %d entries", MaxCoverageReports)
	}
	for i := range r.Reports {
		rep := &r.Reports[i]
		rep.Job = strings.TrimSpace(rep.Job)
		rep.Path = strings.Trim(strings.TrimSpace(rep.Path), "/")
		if rep.Job == "" || rep.Path == "" {
			return fmt.Errorf("coverage: every report needs job and path")
		}
		if strings.Contains(rep.Path, "..") || strings.ContainsAny(rep.Path, "\\\x00") {
			return fmt.Errorf("coverage: bad artifact path %q", rep.Path)
		}
	}
	if r.Min < 0 || r.Min > 100 {
		return fmt.Errorf("coverage.min must be between 0 and 100")
	}
	return nil
}

// EffCoverage is the effective coverage configuration of a repo.
type EffCoverage struct {
	Enabled bool // service on AND the repo named at least one report
	Reports []CoverageReport
	Min     float64
}

func mergeCoverage(svc Coverage, repo *Repo) EffCoverage {
	if !svc.Enabled || repo == nil || repo.Coverage == nil || len(repo.Coverage.Reports) == 0 {
		return EffCoverage{}
	}
	return EffCoverage{Enabled: true, Reports: append([]CoverageReport(nil), repo.Coverage.Reports...), Min: repo.Coverage.Min}
}
