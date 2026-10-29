package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
)

// ReviewSysmap configures the system map (review.sysmap): who provides and
// who consumes which HTTP/webhook/WebSocket/SSE contract across the
// repositories of gitlab.groups, extracted in the background from the
// context repository snapshots.
type ReviewSysmap struct {
	// Enabled turns the map on (default true). Effective only when context
	// repositories are on (Service.SysmapEnabled).
	Enabled bool `yaml:"enabled"`
	// Model extracts contracts from code (default "sonnet"); "" =
	// claude.light_model.
	Model string `yaml:"model"`
	// MaxRunsPerHour caps extraction runs (one run = one batch of files).
	// Not counted against limits.global_runs_per_hour. 0 = code is never
	// sent to the model (only OpenAPI specs are mapped).
	MaxRunsPerHour int `yaml:"max_runs_per_hour"`
	// MaxFilesPerRepo caps the code files of one repository sent to the
	// model; MaxFileKB skips larger files.
	MaxFilesPerRepo int `yaml:"max_files_per_repo"`
	MaxFileKB       int `yaml:"max_file_kb"`
	// RefreshInterval: how often repositories are checked for a new
	// default-branch commit.
	RefreshInterval Duration `yaml:"refresh_interval"`
	// Exclude are doublestar globs of project paths never scanned.
	Exclude []string `yaml:"exclude"`
	// Hints map a base expression or env var name found in a call
	// ("ORDERS_URL", "orders.internal") to the repository that serves it
	// ("backend/orders"). A hint wins over path matching.
	Hints map[string]string `yaml:"hints"`
	// Topology declares which repository calls which over which protocols
	// ({from: group/front, to: group/back, via: [http, ws]}). Applied as
	// the last matching step: a call left unresolved by paths, events and
	// hints is linked "by the topology rule" when exactly one rule target
	// exists for its repository and protocol. Empty = off.
	Topology []SysmapTopologyRule `yaml:"topology"`
	// Externals: calls from these files of a repository go to an external
	// system whose address the code does not show ({name: Bitrix24, repo:
	// group/backend, paths: [components/bitrix/**]}). Applied before hints
	// and the topology; an exact code provider still wins.
	Externals []SysmapExternal `yaml:"externals"`
	// Deps turns on phase B in reviews (default true): what the merge
	// request starts or stops consuming in other repositories, whether the
	// provider's default branch has it, and related open merge requests
	// there. Summary block and prompt context only; never blocks.
	Deps bool `yaml:"deps"`
	// DepsMaxFiles caps the changed code files of one merge request
	// analysed for phase B (cached by content hash; model runs count toward
	// MaxRunsPerHour).
	DepsMaxFiles int `yaml:"deps_max_files"`
	// DepsMaxMRs caps the open merge requests looked at per other
	// repository; DepsMaxAPICalls caps the GitLab requests of phase B in
	// one review (open MR lists and their diffs).
	DepsMaxMRs      int `yaml:"deps_max_mrs"`
	DepsMaxAPICalls int `yaml:"deps_max_api_calls"`
	// DepsMaxRuns caps the model runs of phase B in one review (each run
	// is one batch of up to 25 files; also bounded by MaxRunsPerHour).
	DepsMaxRuns int `yaml:"deps_max_runs"`
	// DepsMaxRunsPerHour is phase B's own hourly budget of model runs,
	// separate from MaxRunsPerHour (building the map), so neither starves
	// the other. 0 = phase B never sends code to the model.
	DepsMaxRunsPerHour int `yaml:"deps_max_runs_per_hour"`
	// Token (env MAP_TOKEN preferred) protects /map like usage.token
	// protects /usage. Empty = the usage token is used; when that is empty
	// too, the page is open.
	Token string `yaml:"token"`
}

// Limits of review.sysmap.hints and review.sysmap.topology.
const (
	MaxSysmapHints      = 200
	MaxSysmapHintKeyLen = 200
	MaxSysmapTopology   = 200
)

// SysmapTopologyRule is one review.sysmap.topology rule: From calls To
// over the Via protocols (http, webhook, ws, sse).
type SysmapTopologyRule struct {
	From string   `yaml:"from"`
	To   string   `yaml:"to"`
	Via  []string `yaml:"via"`
}

// SysmapExternal is one review.sysmap.externals entry: calls from the
// files of Repo matching Paths (doublestar globs) go to the external
// system Name (Host, optional, is shown next to it).
type SysmapExternal struct {
	Name  string   `yaml:"name"`
	Repo  string   `yaml:"repo"`
	Paths []string `yaml:"paths"`
	Host  string   `yaml:"host"`
}

// Limits of review.sysmap.externals.
const (
	MaxSysmapExternals       = 100
	maxSysmapExternalNameLen = 80
)

// sysmapHostRe: a host name for review.sysmap.externals[].host.
var sysmapHostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$`)

// Normalized trims the fields and drops empty globs.
func (e SysmapExternal) Normalized() SysmapExternal {
	out := SysmapExternal{Name: strings.TrimSpace(e.Name), Repo: strings.Trim(strings.TrimSpace(e.Repo), "/"), Host: strings.ToLower(strings.TrimSpace(e.Host))}
	for _, p := range e.Paths {
		if p = strings.TrimPrefix(strings.TrimSpace(p), "./"); p != "" {
			out.Paths = append(out.Paths, p)
		}
	}
	return out
}

func (e SysmapExternal) validate(i int, groups []string) []string {
	n := e.Normalized()
	var errs []string
	bad := func(f string, a ...any) {
		errs = append(errs, fmt.Sprintf("review.sysmap.externals[%d]: ", i)+fmt.Sprintf(f, a...))
	}
	switch {
	case n.Name == "":
		bad("name is empty")
	case len([]rune(n.Name)) > maxSysmapExternalNameLen:
		bad("name: at most %d characters", maxSysmapExternalNameLen)
	}
	switch {
	case !relatedPathRe.MatchString(n.Repo) || strings.Contains(n.Repo, ".."):
		bad("%q is not a full project path like group/project", n.Repo)
	case len(groups) > 0 && !inGroups(n.Repo, groups):
		bad("%q is not in gitlab.groups", n.Repo)
	}
	if len(n.Paths) == 0 {
		bad("paths: at least one glob of files")
	}
	for _, p := range n.Paths {
		if strings.HasPrefix(p, "/") || strings.Contains(p, "..") || !doublestar.ValidatePattern(p) {
			bad("paths: invalid glob %q", p)
		}
	}
	if n.Host != "" && (len(n.Host) > 253 || !sysmapHostRe.MatchString(n.Host)) {
		bad("host %q is not a host name", e.Host)
	}
	return errs
}

// inGroups: project p lies in one of the groups (or their subgroups).
func inGroups(p string, groups []string) bool {
	for _, g := range groups {
		if strings.HasPrefix(strings.ToLower(p), strings.ToLower(strings.Trim(g, "/"))+"/") {
			return true
		}
	}
	return false
}

// sysmapProtocols are the protocols a topology rule may name.
var sysmapProtocols = map[string]bool{"http": true, "webhook": true, "ws": true, "sse": true}

// Normalized returns the rule with trimmed project paths (no leading or
// trailing "/") and lower-case, deduplicated protocols in input order.
func (r SysmapTopologyRule) Normalized() SysmapTopologyRule {
	out := SysmapTopologyRule{From: strings.Trim(strings.TrimSpace(r.From), "/"), To: strings.Trim(strings.TrimSpace(r.To), "/")}
	seen := map[string]bool{}
	for _, v := range r.Via {
		v = strings.ToLower(strings.TrimSpace(v))
		if v != "" && !seen[v] {
			seen[v] = true
			out.Via = append(out.Via, v)
		}
	}
	return out
}

func (r SysmapTopologyRule) validate(i int) []string {
	n := r.Normalized()
	var errs []string
	bad := func(f string, a ...any) {
		errs = append(errs, fmt.Sprintf("review.sysmap.topology[%d]: ", i)+fmt.Sprintf(f, a...))
	}
	for _, p := range []string{n.From, n.To} {
		if !relatedPathRe.MatchString(p) || strings.Contains(p, "..") {
			bad("%q is not a full project path like group/project", p)
		}
	}
	if n.From != "" && strings.EqualFold(n.From, n.To) {
		bad("from and to are the same project")
	}
	if len(n.Via) == 0 {
		bad("via: at least one protocol (http, webhook, ws, sse)")
	}
	for _, v := range n.Via {
		if !sysmapProtocols[v] {
			bad("via: unknown protocol %q (http, webhook, ws, sse)", v)
		}
	}
	return errs
}

func defaultReviewSysmap() ReviewSysmap {
	return ReviewSysmap{
		Enabled: true,
		// Sonnet: the map is built once and then updated only for changed
		// files, so extraction quality matters more than its cost.
		Model:              "sonnet",
		MaxRunsPerHour:     20,
		MaxFilesPerRepo:    400,
		MaxFileKB:          64,
		RefreshInterval:    Duration(10 * time.Minute),
		Deps:               true,
		DepsMaxFiles:       1000,
		DepsMaxMRs:         10,
		DepsMaxAPICalls:    100,
		DepsMaxRuns:        30,
		DepsMaxRunsPerHour: 60,
	}
}

func (c ReviewSysmap) validate() []string {
	if !c.Enabled {
		return nil
	}
	var errs []string
	if c.MaxRunsPerHour < 0 {
		errs = append(errs, "review.sysmap.max_runs_per_hour must be >= 0")
	}
	if c.MaxFilesPerRepo < 1 {
		errs = append(errs, "review.sysmap.max_files_per_repo must be >= 1")
	}
	if c.MaxFileKB < 1 || c.MaxFileKB > 1024 {
		errs = append(errs, "review.sysmap.max_file_kb must be between 1 and 1024")
	}
	if c.RefreshInterval.D() < time.Minute {
		errs = append(errs, "review.sysmap.refresh_interval must be >= 1m")
	}
	for _, g := range c.Exclude {
		if g = strings.Trim(strings.TrimSpace(g), "/"); g == "" || !doublestar.ValidatePattern(g) {
			errs = append(errs, fmt.Sprintf("review.sysmap.exclude: invalid glob %q", g))
		}
	}
	if c.Deps {
		if c.DepsMaxFiles < 1 || c.DepsMaxFiles > 5000 {
			errs = append(errs, "review.sysmap.deps_max_files must be between 1 and 5000")
		}
		if c.DepsMaxMRs < 1 || c.DepsMaxMRs > 100 {
			errs = append(errs, "review.sysmap.deps_max_mrs must be between 1 and 100")
		}
		if c.DepsMaxAPICalls < 1 || c.DepsMaxAPICalls > 500 {
			errs = append(errs, "review.sysmap.deps_max_api_calls must be between 1 and 500")
		}
		if c.DepsMaxRuns < 1 || c.DepsMaxRuns > 100 {
			errs = append(errs, "review.sysmap.deps_max_runs must be between 1 and 100")
		}
		if c.DepsMaxRunsPerHour < 0 || c.DepsMaxRunsPerHour > 1000 {
			errs = append(errs, "review.sysmap.deps_max_runs_per_hour must be between 0 and 1000")
		}
	}
	if len(c.Hints) > MaxSysmapHints {
		errs = append(errs, fmt.Sprintf("review.sysmap.hints: at most %d entries", MaxSysmapHints))
	}
	if len(c.Topology) > MaxSysmapTopology {
		errs = append(errs, fmt.Sprintf("review.sysmap.topology: at most %d rules", MaxSysmapTopology))
	}
	for i, r := range c.Topology {
		errs = append(errs, r.validate(i)...)
	}
	for k, v := range c.Hints {
		if strings.TrimSpace(k) == "" || len(k) > MaxSysmapHintKeyLen {
			errs = append(errs, fmt.Sprintf("review.sysmap.hints: bad key %q (1..%d characters)", k, MaxSysmapHintKeyLen))
		}
		r := strings.Trim(strings.TrimSpace(v), "/")
		if !relatedPathRe.MatchString(r) || strings.Contains(r, "..") {
			errs = append(errs, fmt.Sprintf("review.sysmap.hints: %q -> %q is not a full project path like group/project", k, v))
		}
	}
	return errs
}

// SysmapEnabled reports whether the system map is on: review.sysmap.enabled
// and context repositories on (the map is built from their snapshots).
func (c *Service) SysmapEnabled() bool {
	return c.Review.Sysmap.Enabled && c.ContextEnabled()
}

// SysmapModel is the extraction model: review.sysmap.model, else
// claude.light_model.
func (c *Service) SysmapModel() string {
	if m := strings.TrimSpace(c.Review.Sysmap.Model); m != "" {
		return m
	}
	return c.Claude.LightModel
}

// MaxFileBytes is max_file_kb in bytes.
func (c ReviewSysmap) MaxFileBytes() int64 { return int64(c.MaxFileKB) << 10 }

// Excluded reports whether a project path matches review.sysmap.exclude
// (case-insensitive).
func (c ReviewSysmap) Excluded(projectPath string) bool {
	p := strings.ToLower(strings.Trim(projectPath, "/"))
	for _, g := range c.Exclude {
		g = strings.ToLower(strings.Trim(strings.TrimSpace(g), "/"))
		if ok, err := doublestar.Match(g, p); err == nil && ok {
			return true
		}
	}
	return false
}

// AccessToken is the token protecting /map: MAP_TOKEN, else
// review.sysmap.token, else the usage page token. Empty = open.
func (c ReviewSysmap) AccessToken(usageToken string) string {
	return pageAccessToken("MAP_TOKEN", c.Token, usageToken)
}

// RepoSysmap is the sysmap section of .reviewbot.yaml (read by the map
// scanner from the repository's default branch).
type RepoSysmap struct {
	// Exclude are doublestar globs of files in this repository that are
	// never scanned ("legacy/**", "src/mocks/**").
	Exclude []string `yaml:"exclude"`
}

func (s *RepoSysmap) normalize() error {
	if s == nil {
		return nil
	}
	ex, err := cleanGlobList(s.Exclude)
	if err != nil {
		return fmt.Errorf("sysmap.exclude: %w", err)
	}
	s.Exclude = ex
	return nil
}
