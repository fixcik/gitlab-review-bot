package review

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Every manual job kind started by a note addressed to the bot gets the
// 👀 → ✅/❌ lifecycle; automatic and internal jobs do not.
func TestReactionLifecycleCoversManualKinds(t *testing.T) {
	for _, k := range []core.JobKind{core.JobReview, core.JobFullReview, core.JobForceReview, core.JobCheck, core.JobChat} {
		if !lifecycleKind(k) {
			t.Errorf("%s has no reaction lifecycle", k)
		}
	}
	for _, k := range []core.JobKind{core.JobAutoReview, core.JobReact} {
		if lifecycleKind(k) {
			t.Errorf("%s must not get the lifecycle", k)
		}
	}
}
