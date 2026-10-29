package reviewtest

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/learning"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/store"
)

const MrURL = "https://gitlab.example.com/group/api/-/merge_requests/7"

type LearnHarness struct {
	*Harness
	Db *store.SQLite
}

// NewLearnHarness is New with a real SQLite rule store.
func NewLearnHarness(t *testing.T, st core.Store) *LearnHarness {
	t.Helper()
	h := New(t)
	db, err := store.OpenSQLite(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h.GL.MR.WebURL = MrURL
	if st == nil {
		st = h.St
	} else {
		h.Lim.Usage = st // the learn cap counts its records
	}
	h.Deps = review.Deps{
		GitLab: h.GL, Repos: h.Repos, Runner: h.Runner, Store: st, Limiter: h.Lim, Config: h.Cfg,
		Learnings: db, Outcomes: db, ScratchDir: t.TempDir(),
		BuildIndex:    func([]core.FileDiff) env.DiffIndex { return h.Idx },
		RepoConfigTTL: -1, // tests change the fake .reviewbot.yaml between runs
		BotUserID:     BotID,
		BotUsername:   BotName,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:           func() time.Time { return TestNow },
	}
	h.Reset()
	return &LearnHarness{Harness: h, Db: db}
}

func ReplyJob(noteID int64) core.Job {
	return core.Job{Kind: core.JobThreadReply, ProjectID: PID, ProjectPath: "group/api", MRIID: IID,
		UserID: UserID, Username: "bob", DiscussionID: "d-find", NoteID: noteID}
}

func RulesJob(q string) core.Job {
	j := ManualJob(core.JobRules)
	j.Question = q
	return j
}

// DisagreementThread: a bot finding and a human reply explaining why it is
// not a problem.
func DisagreementThread() core.Discussion {
	id := finding.ID("client.go", "Нет ретраев")
	pos := &core.Position{NewPath: "client.go", OldPath: "client.go", NewLine: 12}
	return core.Discussion{ID: "d-find", Resolvable: true, Notes: []core.Note{
		BotNote(10, finding.NoteBody(id, output.Finding{Severity: "major", Title: "Нет ретраев", Body: "Вызов без повторов при ошибке."}, false, 0), pos),
		{ID: 20, AuthorID: UserID, AuthorUsername: "bob", Body: "это не баг, ретраи делает API-шлюз"},
	}}
}

func LearnJSON(t *testing.T, learnIt bool, rule, scope string) string {
	return MustJSON(t, learning.ExtractOutput{Learn: learnIt, Rule: rule, Scope: scope, Reason: "причина"})
}
