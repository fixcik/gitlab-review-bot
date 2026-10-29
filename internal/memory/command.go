package memory

import (
	"regexp"
	"strings"
)

// CommandKind is a memory command.
type CommandKind int

const (
	CmdShow  CommandKind = iota + 1 // память / memory
	CmdClear                        // память очистить [раздел] / memory clear [section]
	CmdAdd                          // память запомни <раздел>: <текст> / memory add <section>: <text>
)

// Command is a parsed memory command.
type Command struct {
	Kind CommandKind
	// Section: the target section key; "" for CmdClear means everything.
	Section string
	// BadSection is the section name the user wrote when it is unknown (the
	// handler replies with the list of sections).
	BadSection string
	Text       string // CmdAdd
}

var (
	showRe  = regexp.MustCompile(`(?i)^(?:память|memory)(?:\s+(?:проекта|project|показать|покажи|show|list))?$`)
	clearRe = regexp.MustCompile(`(?i)^(?:память|memory)\s+(?:очистить|очисти|сбросить|сбрось|clear|reset|wipe)(?:\s+(.+))?$`)
	addRe   = regexp.MustCompile(`(?is)^(?:память|memory)\s+(?:запомни|запомнить|добавь|добавить|add|remember)\s+([^:\n]+?)\s*:\s*(.+)$`)
	allRe   = regexp.MustCompile(`(?i)^(?:всё|все|всю|целиком|полностью|all|everything)$`)
)

// ParseCommand parses the text after the bot mention. ok is false when it is
// not a memory command (then it may be a rule command or a chat question).
func ParseCommand(rest string) (Command, bool) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return Command{}, false
	}
	if m := addRe.FindStringSubmatch(rest); m != nil {
		text := strings.TrimSpace(m[2])
		if text == "" {
			return Command{}, false
		}
		c := Command{Kind: CmdAdd, Text: text}
		if c.Section = ParseSection(m[1]); c.Section == "" {
			c.BadSection = strings.TrimSpace(m[1])
		}
		return c, true
	}
	if strings.Contains(rest, "\n") {
		return Command{}, false
	}
	line := strings.TrimRight(strings.Join(strings.Fields(rest), " "), ".!?")
	if showRe.MatchString(line) {
		return Command{Kind: CmdShow}, true
	}
	if m := clearRe.FindStringSubmatch(line); m != nil {
		c := Command{Kind: CmdClear}
		if arg := strings.TrimSpace(m[1]); arg != "" && !allRe.MatchString(arg) {
			if c.Section = ParseSection(arg); c.Section == "" {
				c.BadSection = arg
			}
		}
		return c, true
	}
	return Command{}, false
}

// IsCommand reports whether rest is a memory command (webhook routing).
func IsCommand(rest string) bool {
	_, ok := ParseCommand(rest)
	return ok
}
