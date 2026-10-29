package crossrepo

import (
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
)

// Aliases for the package's external tests (package crossrepo_test).

type OpenMR = openMR

const DepsMaxRepos = depsMaxRepos

var (
	ContainsFold         = containsFold
	PreviousDiagram      = previousDiagram
	RenderDepsPrompt     = renderDepsPrompt
	RenderDiagramSection = renderDiagramSection
	RenderSysmapBlock    = renderSysmapBlock
	RenderSysmapPrompt   = renderSysmapPrompt
	SanitizeDiagram      = sanitizeDiagram
)

// NewOpenMR is an open merge request mr of the counterpart repository repo.
func NewOpenMR(repo string, mr core.MergeRequest) *OpenMR { return &openMR{repo: repo, mr: mr} }

// AddMissing records c as a contract missing from its provider; checked:
// the provider's open merge requests were looked at.
func (d *DepsRun) AddMissing(c sysmap.DepContract, checked bool) {
	d.missing = append(d.missing, &depMissing{check: &depCheck{contract: c}, checked: checked})
}

func (xr *Feature) RenderDepsBlock(d *DepsRun, self string) string {
	return xr.renderDepsBlock(d, self)
}

func (xr *Feature) MRRef(m *OpenMR) string { return xr.mrRef(m) }
