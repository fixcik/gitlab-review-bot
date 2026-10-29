package telemetry

import "github.com/fixcik/gitlab-review-bot/internal/sysmap"

// SysmapIndex is implemented by *sysmap.Map.
type SysmapIndex interface{ Index() *sysmap.Index }

// ObserveSysmapScan counts one repository update of the system map
// (sysmap.Options.OnScan). Nil-safe.
func (m *Metrics) ObserveSysmapScan(_ string, status string) {
	if m == nil {
		return
	}
	m.SysmapScans.With(status).Inc()
}

// ObserveSysmap samples the map's index at every scrape. Nil-safe.
func (m *Metrics) ObserveSysmap(x SysmapIndex) {
	if m == nil || x == nil {
		return
	}
	m.Registry.OnCollect(func() {
		idx := x.Index()
		repos := map[string]int{sysmap.StatusOK: 0, sysmap.StatusPartial: 0, sysmap.StatusError: 0}
		sides := map[sysmap.Side]int{sysmap.Provides: 0, sysmap.Consumes: 0}
		unresolved := 0
		if idx != nil {
			for _, r := range idx.Repos {
				repos[r.Status]++
			}
			for _, e := range idx.Entries {
				sides[e.Side]++
				if e.Status == sysmap.Unresolved {
					unresolved++
				}
			}
		}
		for st, n := range repos {
			m.SysmapRepos.With(st).Set(float64(n))
		}
		for side, n := range sides {
			m.SysmapEntries.With(string(side)).Set(float64(n))
		}
		m.SysmapUnresolved.Set(float64(unresolved))
	})
}
