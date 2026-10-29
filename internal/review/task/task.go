// Package task links a merge request to its Jira issues and checks its title.
//
// It finds issue keys in the MR title, branch, description and commit
// messages, fetches the issues from Jira and contributes to a review:
//   - the "Linked Jira issues" / "MR title format" block of the review prompt
//     and the task_alignment / suggested_title properties of the output schema
//     (task/prompt.go);
//   - the "📋 Соответствие задаче" section and the title hint block of the
//     summary note (task/section.go, task/title.go);
//   - the title refresh job, which re-renders only the title hint after the
//     MR title changed (task/title.go).
//
// Jira errors never fail a review: they are shown in the task section and
// reported as bot problems.
//
// The package depends on the shared review packages only: env.Env (GitLab,
// config, logger and the orchestrator's env.RepoConfig, of which the title
// refresh uses Auto), publish.Publisher (ScanMR, to find the summary note) and
// jobctx (bot problems). Other features use Feature.Jira and Feature.BrowseURL
// for their own Jira lookups.
package task

import (
	"context"
	"errors"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/jira"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/publish"
	"github.com/fixcik/gitlab-review-bot/internal/tasklink"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// Source fetches Jira issues (implemented by *jira.Client). review.Deps.Tasks
// is nil when Jira is not configured.
type Source interface {
	GetIssue(ctx context.Context, key string) (*jira.Issue, error)
}

// Feature links the MR to its Jira issues: the task block of the review
// prompt, the task section and title hint of the summary, the title refresh
// job (task/title.go).
type Feature struct {
	env *env.Env
	pub *publish.Publisher
	// Jira fetches Jira issues; nil when Jira is not configured.
	Jira Source
}

// New builds the feature; src is nil when Jira is not configured.
func New(e *env.Env, pub *publish.Publisher, src Source) *Feature {
	return &Feature{env: e, pub: pub, Jira: src}
}

const (
	// maxCommitMessages bounds how many MR commits are searched for keys.
	maxCommitMessages = 200
	// maxPromptCommentRunes caps one Jira comment in the prompt.
	maxPromptCommentRunes = 3000
	// maxAlignmentItems caps each task_alignment list.
	maxAlignmentItems = 15
)

// failure is a linked issue that could not be fetched.
type failure struct {
	Key    string
	Reason string // Russian
}

// Context is what the review knows about the MR's Jira issues and title.
type Context struct {
	titleCheck bool
	jiraOn     bool
	found      []tasklink.Found
	// projectKeys are the configured key prefixes for this repo.
	projectKeys []string
	keys        []string // issues to check (at most tasklink.MaxIssues)
	Issues      []*jira.Issue
	failures    []failure
	// AskTitle: ask Claude for suggested_title (title not in Conventional
	// Commits format). A valid title without the key is reused as is.
	AskTitle bool
	// Title: the format and language of the suggested title
	// (jira.title_prompt / title_language, repository over service).
	Title TitleFormat
}

// TitleFormat is the effective jira.title_prompt and jira.title_language.
type TitleFormat struct {
	Prompt, Language string
}

// AskTask reports whether Claude is asked for task_alignment (some linked
// issue was fetched).
func (tc *Context) AskTask() bool { return len(tc.Issues) > 0 }

// TitleTypes are the allowed Conventional Commits types of the MR title.
func (t *Feature) TitleTypes() []string {
	if tt := t.env.Cfg.Jira.TitleTypes; len(tt) > 0 {
		return tt
	}
	return config.DefaultTitleTypes
}

// findKeys searches the MR title, branch, description and commit messages.
func (t *Feature) findKeys(ctx context.Context, job core.Job, mr *core.MergeRequest, projectKeys []string) []tasklink.Found {
	src := tasklink.Sources{Title: tasklink.StripDraft(mr.Title), Branch: mr.SourceBranch, Description: mr.Description}
	commits, err := t.env.GitLab.ListMRCommits(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		t.env.Log.Warn("review: list mr commits for task keys", "mr", job.MRKey(), "err", err)
	}
	if len(commits) > maxCommitMessages {
		commits = commits[:maxCommitMessages]
	}
	// GitLab lists commits newest first; search oldest first so the first
	// mentioned key wins.
	for i := len(commits) - 1; i >= 0; i-- {
		msg := commits[i].Message
		if msg == "" {
			msg = commits[i].Title
		}
		src.Commits = append(src.Commits, msg)
	}
	return tasklink.Find(src, projectKeys)
}

// BuildContext finds issue keys, fetches the issues from Jira and
// decides what to ask Claude. Jira errors never fail the review.
func (t *Feature) BuildContext(ctx context.Context, job core.Job, mr *core.MergeRequest, projectKeys []string) *Context {
	ctx, tsp := tracing.Start(ctx, "review.task")
	defer tsp.End()
	tc := &Context{titleCheck: t.env.Cfg.Jira.TitleCheck, jiraOn: t.Jira != nil, projectKeys: projectKeys}
	if !tc.titleCheck && !tc.jiraOn {
		return tc
	}
	tc.found = t.findKeys(ctx, job, mr, projectKeys)
	if tc.titleCheck {
		tc.AskTitle = tasklink.NeedsSuggestion(mr.Title, t.TitleTypes())
	}
	if !tc.jiraOn {
		return tc
	}
	tc.keys = tasklink.Keys(tc.found, tasklink.MaxIssues)
	for _, key := range tc.keys {
		is, err := t.Jira.GetIssue(ctx, key)
		if err != nil {
			t.env.Log.Warn("review: jira issue", "mr", job.MRKey(), "key", key, "err", err)
			tc.failures = append(tc.failures, failure{Key: key, Reason: jiraReason(err)})
			continue
		}
		tc.Issues = append(tc.Issues, is)
	}
	return tc
}

// IssueMissing reports whether an error of Source.GetIssue means the issue
// does not exist or is not visible to the bot (jira.ErrNotFound,
// jira.ErrForbidden), so features need not import the Jira client.
func IssueMissing(err error) bool {
	return errors.Is(err, jira.ErrNotFound) || errors.Is(err, jira.ErrForbidden)
}

func jiraReason(err error) string {
	switch {
	case errors.Is(err, jira.ErrNotFound):
		return "задача не найдена"
	case errors.Is(err, jira.ErrForbidden):
		return "нет доступа (проверьте токен Jira и права на проект)"
	default:
		return "ошибка запроса к Jira"
	}
}

// BrowseURL is the issue's Jira page: is.URL when known, else built from
// jira.url and the key.
func (t *Feature) BrowseURL(key string, is *jira.Issue) string {
	if is != nil && is.URL != "" {
		return is.URL
	}
	return strings.TrimRight(t.env.Cfg.Jira.URL, "/") + "/browse/" + key
}

// NoteProblems reports the Jira issues that could not be fetched as bot
// problems.
func NoteProblems(ctx context.Context, tc *Context) {
	if tc == nil {
		return
	}
	for _, f := range tc.failures {
		jobctx.BotProblem(ctx, problems.JiraFailed, f.Key+": "+f.Reason, "")
	}
}
