package review

// Repository configuration: opt-in and validation (review.require_repo_config).
//
// A repository takes part in reviews when its .reviewbot.yaml exists on the
// MR's TARGET branch (an empty file means the defaults); with
// review.require_repo_config: false every project of gitlab.groups does, as
// before. Not opted in: automatic events are skipped silently (metric
// reviewbot_skipped_total{reason="no_repo_config"}), commands addressed to
// the bot get one reply saying how to connect the repository.
//
// The target branch's file is validated strictly (config.ValidateRepo:
// syntax, unknown keys, types, values; every problem with its line). An
// invalid file stops the review: the bot keeps ONE note in the MR (marker
// <!-- reviewbot:config-error -->, updated in place) with the errors, records
// the problem repo_config_invalid (one per project) and, for commands,
// replies in the thread as well. When the file is valid again, the next
// review edits that note to "✅ Конфиг исправлен" (the note is kept, not
// deleted, so the history stays readable).
//
// An MR that adds or changes .reviewbot.yaml gets its HEAD version validated
// too, in the same note: errors always; "valid" only when the repository is
// not opted in yet (the onboarding MR: no review, the config starts working
// after the merge) or the note showed errors before. The note never blocks
// the merge; an invalid HEAD config of an opted-in repository is also
// mentioned at the top of the review summary.
//
// The target branch's file (and files at commit SHAs) are cached per
// (project, ref) for repoConfigTTL, so events do not cost a GitLab request
// each; only "found" and "not found" are cached, other errors never.

import (
	"fmt"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
)

// DefaultRepoConfigTTL is how long a .reviewbot.yaml read (or its absence)
// is reused: adding, fixing or removing the file on the target branch takes
// effect within this time.
const DefaultRepoConfigTTL = 5 * time.Minute

// Skip reasons (Deps.OnSkip, reviewbot_skipped_total).
const (
	SkipNoRepoConfig      = "no_repo_config"
	SkipRepoConfigInvalid = "repo_config_invalid"
)

// Documentation links in the bot's notes.
const (
	docsBaseURL       = "https://github.com/fixcik/gitlab-review-bot/blob/main/"
	docsExampleConfig = docsBaseURL + ".reviewbot.example.yaml"
	docsConfiguration = docsBaseURL + "docs/configuration.md"
	docsOnboarding    = docsBaseURL + "docs/setup-gitlab.md#подключение-репозитория"
)

const (
	msgNotOptedIn     = "Репозиторий не подключён к ревью: добавьте `.reviewbot.yaml` в ветку %s (пустой файл — настройки по умолчанию). Пример — [.reviewbot.example.yaml](" + docsExampleConfig + "), как подключить — [docs/setup-gitlab.md](" + docsOnboarding + ")."
	msgBadRepoConfigN = "Не удалось разобрать `.reviewbot.yaml` в целевой ветке %s: %s. Ревью не выполняется; все ошибки и как исправить — в заметке бота в этом MR."
	msgHelpNotOptedIn = "\n⚠️ " + msgNotOptedIn + "\n"
)

// repoConfigError means .reviewbot.yaml of the target branch is invalid, or
// (notOptedIn) missing while review.require_repo_config is on.
type repoConfigError struct {
	err        error
	notOptedIn bool
	branch     string
}

func (e *repoConfigError) Error() string { return e.err.Error() }
func (e *repoConfigError) Unwrap() error { return e.err }

// repoConfigMessage is the reply to a command refused because of the
// repository config.
func repoConfigMessage(rc *repoConfigError) string {
	if rc.notOptedIn {
		return fmt.Sprintf(msgNotOptedIn, cfgCode(rc.branch))
	}
	issues := config.RepoIssuesOf(rc.err)
	what := ""
	if len(issues) > 0 {
		what = cfgText(issues[0].String(), 200)
	}
	if len(issues) > 1 {
		what += fmt.Sprintf(" (и ещё %d)", len(issues)-1)
	}
	return fmt.Sprintf(msgBadRepoConfigN, cfgCode(rc.branch), what)
}
