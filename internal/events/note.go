package events

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// NoteEvent is a note on an MR (Note Hook) in neutral form.
type NoteEvent struct {
	ProjectID    int64
	ProjectPath  string
	IID          int64
	NoteID       int64
	DiscussionID string
	Body         string
	System       bool
	// Threaded: the note belongs to a threaded discussion (not a plain,
	// individual comment).
	Threaded bool
	UserID   int64
	Username string
}

// Note returns the job for e, or nil and the reason it is ignored.
func (r *Router) Note(e NoteEvent) (*core.Job, string) {
	return r.finish(r.noteJob(e))
}

func (r *Router) noteJob(e NoteEvent) (*core.Job, string) {
	if e.System {
		return nil, "system note"
	}
	if r.opts.BotUsername != "" && strings.EqualFold(e.Username, strings.TrimPrefix(r.opts.BotUsername, "@")) {
		return nil, "own note"
	}
	if r.mention == "" {
		return nil, "no mention configured"
	}
	kind, question, ok := ParseCommand(e.Body, r.mention)
	if !ok {
		return r.replyJob(e)
	}
	kind = routeMRCommands(kind, question) // pause/resume/ignore/resolve/config (commands.go)
	kind = routeRules(kind, question)
	kind = routeMemory(kind, question)
	kind = r.routeReact(kind, e.Body) // pure thanks / ack -> JobReact (react.go)
	if e.ProjectID == 0 || e.IID == 0 {
		return nil, "missing project or iid"
	}
	return &core.Job{
		Kind:         kind,
		ProjectID:    e.ProjectID,
		ProjectPath:  e.ProjectPath,
		MRIID:        e.IID,
		UserID:       e.UserID,
		Username:     e.Username,
		DiscussionID: e.DiscussionID,
		NoteID:       e.NoteID,
		Question:     question,
	}, ""
}

// ParseCommand finds mention in text (case-insensitive, whole word) and
// classifies what follows. ok is false when the mention is absent. A bare
// mention with no other text is treated as JobHelp.
func ParseCommand(text, mention string) (kind core.JobKind, question string, ok bool) {
	start, end, found := findMention(text, mention)
	if !found {
		return "", "", false
	}
	rest := strings.TrimSpace(strings.TrimLeft(text[end:], " \t,:;—-"))
	if strings.TrimFunc(rest, func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSpace(r) }) == "" {
		rest = ""
	}
	if rest != "" {
		// A command is the whole (single-line) text after the mention.
		if k, ok := mentionCommands[normalizeCommand(rest)]; ok && onlyLine(rest) {
			return k, "", true
		}
		return core.JobChat, rest, true
	}
	// Mention at the end: the question is the rest of the note.
	q := strings.TrimSpace(strings.TrimRight(text[:start], " \t,:;—-"))
	if q == "" {
		return core.JobHelp, "", true
	}
	return core.JobChat, q, true
}

// mentionCommands are the commands ParseCommand recognizes (normalized):
// Commands of the kinds in the kind table that are not MR commands
// (core.KindInfo).
var mentionCommands = func() map[string]core.JobKind {
	m := map[string]core.JobKind{}
	for _, k := range core.AllJobKinds() {
		if info, _ := k.Info(); !info.MRCommand {
			for _, w := range info.Commands {
				m[w] = k
			}
		}
	}
	return m
}()

func onlyLine(s string) bool { return !strings.Contains(strings.TrimSpace(s), "\n") }

// normalizeCommand lowercases, collapses whitespace and strips trailing
// punctuation.
func normalizeCommand(s string) string {
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	return strings.TrimRight(s, ".!?")
}

// findMention returns the byte range of the first whole-word,
// case-insensitive occurrence of mention in text.
func findMention(text, mention string) (int, int, bool) {
	if mention == "" {
		return 0, 0, false
	}
	n := len(mention)
	for i := 0; i+n <= len(text); i++ {
		if !strings.EqualFold(text[i:i+n], mention) {
			continue
		}
		if i > 0 {
			prev, _ := utf8.DecodeLastRuneInString(text[:i])
			if isNameRune(prev) || prev == '@' {
				continue
			}
		}
		if i+n < len(text) {
			next, size := utf8.DecodeRuneInString(text[i+n:])
			if isNameRune(next) {
				continue
			}
			// "@bot.foo" is another username; "@bot." ends a sentence.
			if next == '.' && i+n+size < len(text) {
				after, _ := utf8.DecodeRuneInString(text[i+n+size:])
				if isNameRune(after) {
					continue
				}
			}
		}
		return i, i + n, true
	}
	return 0, 0, false
}

func isNameRune(r rune) bool {
	return r == '_' || r == '-' || unicode.IsLetter(r) || unicode.IsDigit(r)
}
