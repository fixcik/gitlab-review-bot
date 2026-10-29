package events

import (
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// MR commands (internal/review/commands.go; "check" in internal/review/
// check.go, the only one with a Claude run). They are recognized in what
// ParseCommand classified as a chat question, before the rule commands and
// before chat.

// mrCommandWords maps normalized command phrases to job kinds; withReason
// are the commands that may carry a short reason after a separator on the
// same line: "@bot пауза — идёт рефакторинг". Both come from the kind table
// (core.KindInfo: Commands of the MRCommand kinds, CommandReason).
var mrCommandWords, withReason = mrCommandTables()

func mrCommandTables() (map[string]core.JobKind, map[core.JobKind]bool) {
	words, reason := map[string]core.JobKind{}, map[core.JobKind]bool{}
	for _, k := range core.AllJobKinds() {
		info, _ := k.Info()
		if !info.MRCommand {
			continue
		}
		for _, w := range info.Commands {
			words[w] = k
		}
		if info.CommandReason {
			reason[k] = true
		}
	}
	return words, reason
}

// ParseMRCommand classifies the text after the mention as an MR command.
// The command must be the whole (single-line) text, apart from trailing
// punctuation; pause/resume may be followed by a reason after ",", ":",
// ";", "—", "–" or " - ".
func ParseMRCommand(rest string) (core.JobKind, bool) {
	rest = strings.TrimSpace(rest)
	if rest == "" || strings.Contains(rest, "\n") {
		return "", false
	}
	if k, ok := mrCommandWords[normalizeCommand(rest)]; ok {
		return k, true
	}
	cut := -1
	for _, sep := range []string{",", ":", ";", "—", "–", " - "} {
		if i := strings.Index(rest, sep); i > 0 && (cut < 0 || i < cut) {
			cut = i
		}
	}
	if cut < 0 {
		return "", false
	}
	if k, ok := mrCommandWords[normalizeCommand(rest[:cut])]; ok && withReason[k] {
		return k, true
	}
	return "", false
}

// routeMRCommands turns a chat question that is an MR command into its job
// kind.
func routeMRCommands(kind core.JobKind, question string) core.JobKind {
	if kind != core.JobChat {
		return kind
	}
	if k, ok := ParseMRCommand(question); ok {
		return k
	}
	return kind
}
