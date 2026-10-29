package core

// Severity is how serious a review finding is. It lives in core (not with
// the review output types in internal/review/output) because config
// (severity thresholds) and lint (linter findings) use it too.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityMajor    Severity = "major"
	SeverityMinor    Severity = "minor"
	SeverityNit      Severity = "nit"
)
