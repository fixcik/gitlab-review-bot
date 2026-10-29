package webhook

import (
	"net/http"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func TestNoteHookMRCommands(t *testing.T) {
	tests := []struct {
		note     string
		wantKind core.JobKind
		wantQ    string
	}{
		{"@reviewbot pause", core.JobPause, "pause"},
		{"@ReviewBot Пауза", core.JobPause, "Пауза"},
		{"@reviewbot продолжить", core.JobResume, "продолжить"},
		{"@reviewbot resume", core.JobResume, "resume"},
		{"@reviewbot ignore", core.JobIgnore, "ignore"},
		{"@reviewbot игнорировать", core.JobIgnore, "игнорировать"},
		{"@reviewbot закрой треды", core.JobResolveAll, "закрой треды"},
		{"@reviewbot resolve", core.JobResolveAll, "resolve"},
		{"@reviewbot конфиг", core.JobConfig, "конфиг"},
		{"@reviewbot config", core.JobConfig, "config"},
		{"@reviewbot check", core.JobCheck, "check"},
		{"@reviewbot перепроверь", core.JobCheck, "перепроверь"},
		{"@ReviewBot, проверь.", core.JobCheck, "проверь."},
		{"@reviewbot recheck", core.JobCheck, "recheck"},
		{"@reviewbot проверь этот цикл", core.JobChat, "проверь этот цикл"},
		// Existing commands and chat are unaffected.
		{"@reviewbot review", core.JobReview, ""},
		{"@reviewbot help", core.JobHelp, ""},
		{"@reviewbot правила", core.JobRules, "правила"},
		{"@reviewbot pause the consumer here?", core.JobChat, "pause the consumer here?"},
	}
	for _, tt := range tests {
		t.Run(tt.note, func(t *testing.T) {
			rec := &recorder{}
			h := newTestHandler(rec, "")
			w := do(h, http.MethodPost, "Note Hook", secret, noteHook("bob", tt.note, false, "MergeRequest"))
			if w.Code != http.StatusOK || len(rec.jobs) != 1 {
				t.Fatalf("status %d jobs %d", w.Code, len(rec.jobs))
			}
			j := rec.jobs[0]
			if j.Kind != tt.wantKind || j.Question != tt.wantQ || j.UserID != 11 || j.DiscussionID != "d1scuss10n" || j.NoteID != 555 {
				t.Fatalf("job %+v", j)
			}
		})
	}
}
