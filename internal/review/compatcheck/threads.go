package compatcheck

// Breaking-change threads: one blocking thread per item (Publish), closed
// when fixed (ResolveFixed).

import (
	"context"
	"fmt"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/publish"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// badge heads every breaking-change note.
const badge = "🧩 Breaking change"

func sourceName(s string) string {
	if s == SourceClaude {
		return "анализ Claude"
	}
	return s
}

// noteBody renders the first note of a breaking-change thread. It is
// always blocking.
func noteBody(it Item, outsideDiff bool, snappedFrom int) string {
	var b strings.Builder
	b.WriteString(marker.Compat(it.ID, it.Source) + "\n" + marker.Blocking + "\n")
	title := it.Title
	if it.Source != SourceClaude && it.Source != compat.SourceCustom {
		title = mdtext.Inline(title, 200) // tool text derived from repo content
	} else {
		title = mdtext.OneLine(title)
	}
	fmt.Fprintf(&b, "**%s** · %s\n", badge, title)
	b.WriteString("\n⛔ **Блокирует мерж:** обратно несовместимое изменение. При rolling deploy старая и новая версии работают одновременно, поэтому изменение должно быть совместимо в обе стороны. Исправьте и закройте тред.\n")
	src := "_Источник: " + sourceName(it.Source)
	switch {
	case it.Source == compat.SourceCustom || it.Rule != "":
		src += " (" + mdtext.Code(it.Rule) + ")"
	case it.Kind != "":
		src += " (" + compatKindName(it.Kind) + ")"
	}
	b.WriteString("\n" + src + "._\n")
	if outsideDiff && it.Path != "" {
		fmt.Fprintf(&b, "\n_Файл %s (вне диффа)._\n", mdtext.FileLineRU(it.Path, it.Line))
	}
	if snappedFrom > 0 {
		fmt.Fprintf(&b, "\n_Относится к строке %d._\n", snappedFrom)
	}
	switch it.Source {
	case SourceClaude:
		if it.Message != "" {
			b.WriteString("\n" + it.Message + "\n")
		}
		if it.PreviousBehavior != "" {
			b.WriteString("\n**Было:** " + mdtext.OneLine(it.PreviousBehavior) + "\n")
		}
		if it.NewBehavior != "" {
			b.WriteString("\n**Стало:** " + mdtext.OneLine(it.NewBehavior) + "\n")
		}
		if it.SafeAlternative != "" {
			b.WriteString("\n**Безопасная альтернатива:** " + it.SafeAlternative + "\n")
		}
		if it.Unverified {
			b.WriteString("\n_⚠️ Не перепроверено (" + it.VerifyNote + ") — считается блокирующим, пока не доказано обратное._\n")
		} else if it.VerifyNote != "" {
			b.WriteString("\n_Подтверждено перепроверкой: " + mdtext.OneLine(it.VerifyNote) + "_\n")
		}
	case compat.SourceCustom:
		fence := mdtext.CodeFence(it.Message)
		b.WriteString("\n" + fence + "\n" + strings.TrimRight(it.Message, "\n") + "\n" + fence + "\n")
		b.WriteString("\n**Безопасная альтернатива:** сделайте изменение совместимым (expand/contract: сначала добавить новое, сохранив старое; удалять — отдельным релизом, когда старая версия больше не работает).\n")
	default:
		b.WriteString("\n**Безопасная альтернатива:** сохраните старый контракт (поле, метод, значение) рядом с новым и удаляйте его отдельным релизом, когда клиенты предыдущей версии больше не работают (expand/contract), или выпустите новую версию API.\n")
	}
	return mdtext.NeutralizeQuickActions(b.String())
}

func compatKindName(k string) string {
	switch k {
	case output.CompatKindAPI:
		return "API"
	case output.CompatKindEvent:
		return "события/сообщения"
	case output.CompatKindDB:
		return "миграция БД"
	case output.CompatKindConfig:
		return "конфигурация"
	default:
		return "другое"
	}
}

// ResolveFixed closes open compat threads that are fixed:
// deterministic ones when the source ran completely and no longer reports
// them; Claude ones when Claude lists them in compatibility.resolved.
// It returns how many of them were marked fixed but left unresolved
// (Reporter bot): compat threads always block, so those still block.
func (cf *Feature) ResolveFixed(ctx context.Context, job core.Job, cr *Run, out *output.ReviewOutput, threads *mrthread.Threads) (pending int) {
	if cr == nil || !cr.Enabled {
		return 0
	}
	current := map[string]bool{}
	for _, it := range cr.Items {
		current[it.ID] = true
	}
	claudeFixed := map[string]string{}
	if out.Compatibility != nil {
		for _, x := range out.Compatibility.Resolved {
			claudeFixed[x.ID] = x.Comment
		}
	}
	for _, id := range threads.CompatOrder {
		oc, ok := threads.OpenCompat[id]
		if !ok {
			continue
		}
		fixed, comment := false, ""
		switch oc.Source {
		case SourceClaude:
			if c, ok := claudeFixed[id]; ok && !current[id] {
				fixed, comment = true, c
			}
		case compat.SourceCustom:
			// compat.commands were removed: nothing re-runs the check, so
			// its absence proves nothing. A person closes the thread.
		default:
			if cr.Res != nil && cr.Err == nil && cr.Res.SourceOK(oc.Source) && !current[id] {
				fixed, comment = true, "проверка "+oc.Source+" больше не находит эту несовместимость"
			}
		}
		if !fixed {
			cr.Remaining = append(cr.Remaining, oc)
			continue
		}
		if done, unresolved := cf.pub.MarkFixed(ctx, job, oc.DiscussionID, comment); done {
			delete(threads.OpenCompat, id)
			cr.Closed++
			if unresolved {
				pending++
			}
		} else {
			cr.Remaining = append(cr.Remaining, oc)
		}
	}
	return pending
}

// openCompatID returns the id/source of an open thread: from its marker,
// else derived like a new item's.
func openCompatID(oc mrthread.OpenCompat) (id, source string) {
	if id, src, ok := marker.ParseCompat(oc.Body); ok {
		return id, src
	}
	return compat.ID(oc.Source, oc.Path, oc.Title), oc.Source
}

// Unpublished is the number of breaking changes whose thread could not be
// published (Outside): they always block, so they still count in the merge
// gate (fail closed).
func (cr *Run) Unpublished() int {
	if cr == nil {
		return 0
	}
	return len(cr.Outside)
}

// Publish posts every new breaking change as its own blocking thread
// (inline when the line is in the diff, else a general thread naming the
// file and line). Items whose thread is already open are skipped; items
// whose thread could not be published go to Outside (Unpublished).
func (cf *Feature) Publish(ctx context.Context, job core.Job, mr *core.MergeRequest, idx env.DiffIndex, cr *Run, threads *mrthread.Threads) {
	ctx, tsp := tracing.Start(ctx, "review.publish.compat")
	defer tsp.End()
	if cr == nil || len(cr.Items) == 0 {
		return
	}
	var batch []publish.Draft
	var batched []Item
	for _, it := range cr.Items {
		if _, open := threads.OpenCompat[it.ID]; open {
			continue
		}
		if _, dismissed := threads.DismissedCompat[it.ID]; dismissed {
			cr.dismissed = append(cr.dismissed, it)
			continue
		}
		pos, _, snappedFrom := cf.pub.Anchor(idx, mr.DiffRefs, output.Finding{Path: it.Path, Line: it.Line})
		batch = append(batch, publish.Draft{Body: noteBody(it, pos == nil, snappedFrom), Pos: pos, What: "compat " + it.ID})
		batched = append(batched, it)
	}
	created, err := cf.pub.Drafts(ctx, job, batch)
	for i, it := range batched {
		if created[i] && err == nil {
			cr.Published = append(cr.Published, it)
		} else {
			cr.Outside = append(cr.Outside, it)
		}
	}
}
