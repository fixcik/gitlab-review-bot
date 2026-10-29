package telemetry

import (
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
)

type stubIndex struct{ x *sysmap.Index }

func (s stubIndex) Index() *sysmap.Index { return s.x }

func TestObserveSysmap(t *testing.T) {
	m := New("test")
	x := sysmap.Build([]sysmap.RepoState{{Path: "g/a", Status: sysmap.StatusOK}, {Path: "g/b", Status: sysmap.StatusPartial}},
		map[string][]sysmap.FileRecord{
			"g/a": {{Path: "a.py", Entries: []sysmap.Entry{{Side: sysmap.Provides, Protocol: sysmap.HTTP, Method: "GET", Path: "/x", File: "a.py", Line: 1}}}},
			"g/b": {{Path: "b.ts", Entries: []sysmap.Entry{
				{Side: sysmap.Consumes, Protocol: sysmap.HTTP, Method: "GET", Path: "/x", File: "b.ts", Line: 1},
				{Side: sysmap.Consumes, Protocol: sysmap.HTTP, Method: "GET", Path: "/nowhere", File: "b.ts", Line: 2},
			}}},
		}, nil, time.Now())
	m.ObserveSysmap(stubIndex{x})
	m.ObserveSysmapScan("g/a", sysmap.StatusOK)
	m.ObserveSysmapScan("g/b", sysmap.StatusPartial)
	m.ObserveSysmapScan("g/b", sysmap.StatusOK)
	out := scrape(t, m)
	has(t, out,
		`reviewbot_sysmap_scans_total{status="ok"} 2`,
		`reviewbot_sysmap_scans_total{status="partial"} 1`,
		`reviewbot_sysmap_repos{status="ok"} 1`,
		`reviewbot_sysmap_repos{status="partial"} 1`,
		`reviewbot_sysmap_repos{status="error"} 0`,
		`reviewbot_sysmap_entries{side="provides"} 1`,
		`reviewbot_sysmap_entries{side="consumes"} 2`,
		`reviewbot_sysmap_unresolved 1`,
	)
	var nilM *Metrics
	nilM.ObserveSysmap(stubIndex{})
	nilM.ObserveSysmapScan("g", "ok")
}
