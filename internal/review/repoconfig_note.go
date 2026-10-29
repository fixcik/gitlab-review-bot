package review

// The config-error note in the MR.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
)

func (r *Reviewer) syncConfigNote(ctx context.Context, job core.Job, mr *core.MergeRequest, target cfgFile, head *cfgFile, optedIn bool) {
	log := r.env.Log.With("mr", job.MRKey())
	ds, err := r.env.GitLab.ListDiscussions(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		log.Warn("review: list discussions for the config note", "err", err)
		return
	}
	var noteID int64
	var existing string
	for _, d := range ds {
		if len(d.Notes) == 0 {
			continue
		}
		n := d.Notes[0]
		if n.System || !r.env.IsBotNote(n) || !marker.ConfigNoteRe.MatchString(marker.Header(n.Body)) {
			continue
		}
		if noteID == 0 || n.ID < noteID { // the oldest one, if a race made two
			noteID, existing = n.ID, n.Body
		}
	}
	prevErr := false
	if m := marker.ConfigNoteRe.FindStringSubmatch(marker.Header(existing)); m != nil {
		prevErr = m[1] != "ok"
	}
	body, write := configNoteBody(mr.TargetBranch, target, head, optedIn, prevErr)
	if !write {
		return
	}
	body = mdtext.NeutralizeQuickActions(body)
	if noteID != 0 && body == existing {
		return
	}
	if noteID != 0 {
		err := r.env.GitLab.UpdateNote(ctx, job.ProjectID, job.MRIID, noteID, body)
		if err == nil {
			log.Info("review: config note updated", "note", noteID)
			return
		}
		if !errors.Is(err, core.ErrNotFound) {
			log.Warn("review: update config note", "err", err)
			return
		}
	}
	if _, err := r.env.GitLab.CreateNote(ctx, job.ProjectID, job.MRIID, body); err != nil {
		log.Warn("review: create config note", "err", err)
		return
	}
	log.Info("review: config note created")
}

// configNoteBody renders the config note; write=false when there is nothing
// to say (no errors anywhere and no earlier error note to close).
func configNoteBody(branch string, target cfgFile, head *cfgFile, optedIn, prevErr bool) (string, bool) {
	br := cfgCode(branch)
	targetBad := target.found && len(target.issues) > 0
	headBad := head != nil && !head.valid()
	var b strings.Builder
	switch {
	case targetBad:
		b.WriteString(marker.ConfigNote + " state=error -->\n")
		b.WriteString("### ⚠️ Конфигурация бота невалидна — ревью не выполняется\n\n")
		fmt.Fprintf(&b, "`.reviewbot.yaml` в целевой ветке %s содержит ошибки (бот читает конфиг из целевой ветки, даже если этот MR его не менял):\n\n", br)
		writeIssues(&b, target.issues)
		switch {
		case head != nil && head.valid():
			b.WriteString("\n✅ В этом MR `.reviewbot.yaml` исправлен — ревью заработает после его мержа.\n")
		case headBad:
			b.WriteString("\n⚠️ `.reviewbot.yaml` в этом MR тоже невалиден:\n\n")
			writeIssues(&b, head.issues)
		}
		b.WriteString(configFixHint)
	case headBad:
		b.WriteString(marker.ConfigNote + " state=error -->\n")
		b.WriteString("### ⚠️ `.reviewbot.yaml` в этом MR невалиден\n\n")
		writeIssues(&b, head.issues)
		if optedIn {
			fmt.Fprintf(&b, "\nПока действует конфиг из %s, ревью идёт как обычно; после мержа с такими ошибками бот перестанет ревьюить этот репозиторий — исправьте до мержа.\n", br)
		} else {
			fmt.Fprintf(&b, "\nРепозиторий подключится к ревью после мержа валидного `.reviewbot.yaml` в %s.\n", br)
		}
		b.WriteString(configFixHint)
	case head != nil && !optedIn:
		b.WriteString(marker.ConfigNote + " state=ok -->\n")
		fmt.Fprintf(&b, "✅ Новый `.reviewbot.yaml` валиден — ревью этого репозитория начнётся после мержа MR в %s.\n", br)
	case head != nil && prevErr:
		b.WriteString(marker.ConfigNote + " state=ok -->\n")
		b.WriteString("✅ Новый `.reviewbot.yaml` валиден.\n")
	case prevErr && optedIn:
		b.WriteString(marker.ConfigNote + " state=ok -->\n")
		fmt.Fprintf(&b, "✅ Конфиг исправлен: `.reviewbot.yaml` в %s валиден, ревью выполняется.\n", br)
	default:
		return "", false
	}
	return b.String(), true
}

const configFixHint = "\n**Как исправить:** поправьте файл (для целевой ветки — отдельным MR), проверить локально — `reviewbot validate-repo-config .reviewbot.yaml`; " +
	"ключи и значения — [docs/configuration.md](" + docsConfiguration + "), подсказки в редакторе — JSON Schema `docs/reviewbot.schema.json`. " +
	"Эта заметка обновится при следующем ревью.\n"

// maxNoteIssues caps the issues listed in the note.
const maxNoteIssues = 20

func writeIssues(b *strings.Builder, issues config.RepoErrors) {
	for i, is := range issues {
		if i == maxNoteIssues {
			fmt.Fprintf(b, "- … и ещё %d\n", len(issues)-maxNoteIssues)
			break
		}
		b.WriteString("- ")
		if is.Line > 0 {
			fmt.Fprintf(b, "строка %d: ", is.Line)
		}
		if is.Key != "" {
			b.WriteString(cfgCode(is.Key))
			b.WriteString(" — ")
		}
		b.WriteString(cfgText(is.Message, 300))
		b.WriteString("\n")
	}
}

// cfgText makes text derived from a repository file (keys, values in error
// messages; the MR author controls the HEAD version) safe for a note: one
// line, no HTML comments or tags (problems.CleanText), no mentions, no
// #12 / !12 references (GitLab would add a "mentioned in" note to that
// issue or MR), no Markdown links or code spans.
func cfgText(s string, n int) string {
	s = problems.CleanText(s, n)
	s = strings.NewReplacer("@", "@\u200b", "`", "'", "](", "] (").Replace(s)
	return cfgRefRe.ReplaceAllStringFunc(s, func(m string) string { return m[:1] + "\u200b" + m[1:] })
}

// cfgRefRe: a #12 / !12 issue or MR reference.
var cfgRefRe = regexp.MustCompile(`[#!][0-9]`)

// cfgCode renders s as an inline code span that cannot be broken out of.
func cfgCode(s string) string {
	s = problems.CleanText(s, 200)
	s = strings.ReplaceAll(s, "`", "'")
	if s == "" {
		s = " "
	}
	return "`" + s + "`"
}
