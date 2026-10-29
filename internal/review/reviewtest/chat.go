package reviewtest

import (
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

func ChatJob(q string) core.Job {
	j := ManualJob(core.JobChat)
	j.DiscussionID = "d-find"
	j.NoteID = 30
	j.Question = q
	return j
}

func FindingThread(resolved bool) core.Discussion {
	id := finding.ID("cache.go", "Race on map")
	pos := &core.Position{NewPath: "cache.go", OldPath: "cache.go", NewLine: 2}
	return core.Discussion{ID: "d-find", Resolvable: true, Resolved: resolved, Notes: []core.Note{
		BotNote(10, finding.NoteBody(id, output.Finding{Severity: "major", Title: "Race on map", Body: "No lock."}, false, 0), pos),
		{ID: 20, AuthorID: UserID, AuthorUsername: "bob", Body: "Я добавил мьютекс в Get."},
		{ID: 30, AuthorID: UserID, AuthorUsername: "bob", Body: "@reviewbot теперь ок?"},
	}}
}
