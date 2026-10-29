package review

import (
	"context"
	"errors"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
)

// ---------------------------------------------------------------------------
// User-facing strings (Russian)
// ---------------------------------------------------------------------------

const (
	msgNotOpened       = "MR закрыт или уже слит — ревью не выполняется."
	msgDraft           = "MR находится в статусе **Draft** — ревью не выполняется. Когда MR будет готов (Mark as ready), бот автоматически проведёт полное ревью."
	msgDisabled        = "Бот отключён для этого репозитория (`enabled: false` в `.reviewbot.yaml`)."
	msgNoNewCommits    = "Новых коммитов с последнего ревью (`%s`) нет. Для полного ревью используйте `%s full review`."
	msgNothingToReview = "В диапазоне `%s..%s` нет файлов для ревью (все изменения попадают под правила ignore)."
	msgPostponed       = "⏳ Ревью отложено до %s: %s"
	msgReviewFailed    = "❌ Ревью не удалось: %s"
	msgReviewDone      = "✅ Ревью выполнено (`%s..%s`): новых замечаний — %d, закрыто — %d. Сводка обновлена."
	msgChatPostponed   = "⏳ Достигнут лимит использования Claude, отвечу после %s."
	msgChatFailed      = "❌ Не удалось получить ответ: %s"
	reasonClaudeLimit  = "достигнут лимит использования Claude"
	reasonInternal     = "внутренняя ошибка (подробности в логах бота)"
	reasonClaudeAuth   = "Claude отклонил токен бота (недействителен или истёк): администратору нужно заменить токен"
	reasonInvalidJSON  = "Claude вернул ответ в неверном формате"
)

// failReason is what a command author is told about a failed Claude run: a
// rejected token is named (and shown on /problems), anything else is an
// internal error.
func failReason(ctx context.Context, err error) string {
	var ae *core.ErrClaudeAuth
	if errors.As(err, &ae) {
		jobctx.BotProblem(ctx, problems.ClaudeAuthFailed, "токен Claude отклонён", ae.Error())
		return reasonClaudeAuth
	}
	return reasonInternal
}
