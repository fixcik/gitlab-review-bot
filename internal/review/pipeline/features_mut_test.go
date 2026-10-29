package pipeline

import (
	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/cijob"
	"strings"
	"testing"
	"time"
)

// TBD thresholds warn only strictly above the limit; 0 means no limit.
func TestTBDThresholdsMut(t *testing.T) {
	lim := config.TBD{MaxLines: 10, MaxAge: config.Duration(72 * time.Hour), MaxBehind: 5}
	at := &TBDStatus{SizeKnown: true, Added: 6, Removed: 4, Files: 1, AgeKnown: true, Age: 72 * time.Hour, Behind: 5, Target: "main"}
	if blk := RenderTBD(at, lim); strings.Contains(blk, "⚠️") {
		t.Errorf("at the limits nothing is flagged:\n%s", blk)
	}
	over := &TBDStatus{SizeKnown: true, Added: 7, Removed: 4, Files: 1, AgeKnown: true, Age: 73 * time.Hour, Behind: 6, Target: "main"}
	blk := RenderTBD(over, lim)
	for _, want := range []string{"⚠️ больше 10: такой MR", "⚠️ дольше 3 дня", "⚠️ больше 5: обновите ветку"} {
		if !strings.Contains(blk, want) {
			t.Errorf("over the limits: lacks %q:\n%s", want, blk)
		}
	}
	if blk := RenderTBD(over, config.TBD{}); strings.Contains(blk, "⚠️") {
		t.Errorf("no limits configured:\n%s", blk)
	}
	unknown := &TBDStatus{Behind: -1}
	blk = RenderTBD(unknown, lim)
	if !strings.Contains(blk, "размер MR узнать не удалось") || strings.Contains(blk, "🕒") || strings.Contains(blk, "↩️") || strings.Contains(blk, "пайплайн") {
		t.Errorf("unknown data:\n%s", blk)
	}
	if RenderTBD(nil, lim) != "" {
		t.Error("nil status renders nothing")
	}
}

// needsRefresh keeps the refresh chain alive only for a running pipeline
// that the block shows, or for coverage waiting on the head pipeline.
func TestNeedsRefreshMut(t *testing.T) {
	running := &core.Pipeline{Status: "running"}
	done := &core.Pipeline{Status: "success"}
	for _, tc := range []struct {
		name string
		s    *TBDStatus
		cov  *CoverageRun
		want bool
	}{
		{"nothing", nil, nil, false},
		{"running shown", &TBDStatus{ShowPipe: true, Pipeline: running}, nil, true},
		{"running hidden", &TBDStatus{ShowPipe: false, Pipeline: running}, nil, false},
		{"finished", &TBDStatus{ShowPipe: true, Pipeline: done}, nil, false},
		{"no pipeline", &TBDStatus{ShowPipe: true}, nil, false},
		{"coverage waits", nil, &CoverageRun{State: covWaiting}, true},
		{"coverage done", &TBDStatus{ShowPipe: true, Pipeline: done}, &CoverageRun{State: CoverageOK}, false},
		{"coverage error", nil, &CoverageRun{State: covError}, false},
	} {
		if got := needsRefresh(tc.s, tc.cov); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	for st, want := range map[string]bool{"created": true, "waiting_for_resource": true, "preparing": true, "pending": true,
		"running": true, "scheduled": true, "success": false, "failed": false, "canceled": false, "manual": false, "skipped": false} {
		if got := cijob.Running(&core.Pipeline{Status: st}); got != want {
			t.Errorf("pipelineRunning(%s) = %v", st, got)
		}
	}
}
