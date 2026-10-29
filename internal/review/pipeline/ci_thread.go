package pipeline

// The bot thread "🔴 Почему упал пайплайн" of an MR (pipeline/ci.go): one
// thread per MR, edited in place for newer failures, turned into "✅
// Пайплайн #M прошёл" and resolved when the head pipeline passes.

import (
	"context"
	"fmt"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/cijob"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/publish"
)

// ciThreadTitle is the title of the thread (also named by the digest).
const ciThreadTitle = "Почему упал пайплайн"

// ciThreadState is what the thread shows.
type ciThreadState struct {
	Pipeline  core.Pipeline
	FP        string
	Explained int64
	Expl      string // rendered explanation
	Digest    string // the digest, when the summary cannot carry it
	Note      string
}

// ciThreadFooter closes every red thread.
const ciThreadFooter = "<sub>Объяснение — модель по логам CI и коду MR; логи и сообщения тестов — недоверенные данные. Когда пайплайн пройдёт, тред закроется сам. Тред не блокирует мерж.</sub>"

// renderCIThread renders the red thread (marker first).
func renderCIThread(s ciThreadState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### 🔴 %s %s\n\n", ciThreadTitle, cijob.PipelineLink(s.Pipeline))
	if s.Expl != "" {
		b.WriteString(s.Expl)
	}
	if s.Digest != "" {
		b.WriteString(s.Digest + "\n")
	}
	if s.Note != "" {
		b.WriteString("ℹ️ " + s.Note + "\n\n")
	}
	b.WriteString(ciThreadFooter)
	return marker.CIThread(s.Pipeline.ID, s.FP, s.Explained, false) + "\n" + mdtext.DefuseComments(b.String())
}

// ciSameFailure rewrites the head of a thread for a pipeline that failed
// the same way as the explained one: the explanation below stays.
func ciSameFailure(body string, p core.Pipeline, fp string, explained int64) (string, bool) {
	lines := strings.Split(body, "\n")
	if len(lines) < 2 || !marker.CIThreadRe.MatchString(lines[0]) {
		return body, false
	}
	out := []string{marker.CIThread(p.ID, fp, explained, false), fmt.Sprintf("### 🔴 %s %s", ciThreadTitle, cijob.PipelineLink(p)), "",
		fmt.Sprintf("🔁 Упал так же, как пайплайн #%d: объяснение ниже — то же.", explained)}
	rest := lines[2:]
	for len(rest) > 0 && (strings.TrimSpace(rest[0]) == "" || strings.HasPrefix(rest[0], "🔁 ")) {
		rest = rest[1:]
	}
	return strings.Join(out, "\n") + "\n\n" + mdtext.DefuseComments(strings.Join(rest, "\n")), true
}

// renderCIGreen turns the thread into "the pipeline passed", keeping the
// former explanation collapsed.
func renderCIGreen(th *mrthread.CIThread, p core.Pipeline) string {
	lines := strings.Split(th.Body, "\n")
	rest := ""
	if len(lines) > 2 {
		rest = mdtext.DefuseComments(strings.TrimSpace(strings.Join(lines[2:], "\n")))
	}
	var b strings.Builder
	b.WriteString(marker.CIThread(p.ID, "", 0, true) + "\n")
	fmt.Fprintf(&b, "### ✅ Пайплайн %s прошёл\n\nПрежнее объяснение больше не актуально.\n", cijob.PipelineLink(p))
	if rest != "" {
		fmt.Fprintf(&b, "\n<details>\n<summary>Почему падал пайплайн #%d</summary>\n\n%s\n\n</details>\n", th.Pipeline, rest)
	}
	return b.String()
}

// upsertCIThread edits the active thread or opens a new one.
func (pf *Feature) upsertCIThread(ctx context.Context, job core.Job, th *mrthread.CIThread, body string) {
	if th.Active() {
		pf.updateCIThread(ctx, job, th, body)
		return
	}
	// Through publish.Drafts like every other thread: leftover drafts of an
	// interrupted job are deleted first, never published with this one.
	if _, err := pf.pub.Drafts(ctx, job, []publish.Draft{{Body: mdtext.NeutralizeQuickActions(body), What: "ci thread"}}); err != nil {
		pf.env.Log.Warn("review: ci: publish thread", "mr", job.MRKey(), "err", err)
	}
}

// updateCIThread edits the active thread in place.
func (pf *Feature) updateCIThread(ctx context.Context, job core.Job, th *mrthread.CIThread, body string) {
	if err := pf.env.GitLab.UpdateNote(ctx, job.ProjectID, job.MRIID, th.NoteID, mdtext.NeutralizeQuickActions(body)); err != nil {
		pf.env.Log.Warn("review: ci: update thread", "mr", job.MRKey(), "err", err)
	}
}
