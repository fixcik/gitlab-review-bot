package diag

import "github.com/fixcik/gitlab-review-bot/internal/diag/diagindex"

// IndexRow and IndexFilter are the index rows and their filter; they live in
// the leaf internal/diag/diagindex so the store does not import diag.
type (
	IndexRow    = diagindex.Row
	IndexFilter = diagindex.Filter
)
