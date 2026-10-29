package telemetry

// reviewbot_skipped_total reasons set by this package's callers.
const (
	// SkipTooBig: a review over review.max_mr_lines
	// (internal/review/bigmr.go).
	SkipTooBig = "too_big"
	// SkipTrivialIncrement: an automatic incremental review skipped as
	// trivial (internal/review/trivial.go).
	SkipTrivialIncrement = "trivial_increment"
)

// ObserveSkip counts one review that deliberately ran no Claude (see
// review.Deps.OnSkip). Nil-safe.
func (m *Metrics) ObserveSkip(reason string) {
	if m == nil {
		return
	}
	m.Skipped.With(reason).Inc()
}

// ObserveGitLabSaved counts one GitLab read answered from a job's memo (see
// review.Deps.OnGitLabSaved). Nil-safe.
func (m *Metrics) ObserveGitLabSaved(op string) {
	if m == nil {
		return
	}
	m.GitLabSaved.With(op).Inc()
}

// ObserveRoute counts one review by model route (light, normal, heavy; see
// review.Deps.OnRoute). Nil-safe.
func (m *Metrics) ObserveRoute(route string) {
	if m == nil {
		return
	}
	m.ReviewRoutes.With(route).Inc()
}
