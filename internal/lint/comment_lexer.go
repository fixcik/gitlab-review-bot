package lint

import (
	"regexp"
	"strings"
)

// srcLine is one line of a file; ok=false for a line not known (a gap in
// the diff-only fallback).
type srcLine struct {
	text string
	ok   bool
}

// splitLines splits text into 1-based lines (index 0 unused) without
// their "\r".
func splitLines(text string) []srcLine {
	raw := strings.Split(text, "\n")
	out := make([]srcLine, len(raw)+1)
	for i, l := range raw {
		out[i+1] = srcLine{text: strings.TrimSuffix(l, "\r"), ok: true}
	}
	return out
}

// lineScan is what the lexer found on one line.
type lineScan struct {
	comments []string // comment texts without delimiters
	code     bool     // non-blank text outside comments (strings count)
	// segs is the line in order: code runs (with their whitespace), string
	// literals verbatim (str) and comment pieces (cmt). Used by CompareCode
	// (trivial.go); the todo check reads comments and code only.
	segs []codeSeg
	// contStr: the line starts inside a multi-line string (for CompareCode:
	// also a Python docstring that is not inert, see lexState.docCode).
	contStr bool
	// unsure: the lexer guessed on this line (an unterminated quote, a
	// comment starter right after a backslash or "$", a docstring followed by
	// code): CompareCode never calls such a file trivial.
	unsure bool
}

// codeSeg is a piece of a line.
type codeSeg struct {
	text string
	str  bool // a string literal (or its part): whitespace is significant
	// cmt: a comment piece (text without delimiters); open is the delimiter
	// that opens it on this line, "" for the next lines of a block comment
	// or a docstring.
	cmt  bool
	open string
}

// comment records a comment piece.
func (l *lineScan) comment(open, text string) {
	l.comments = append(l.comments, text)
	l.segs = append(l.segs, codeSeg{text: text, cmt: true, open: open})
}

func (l *lineScan) add(text string, str bool) {
	if text == "" && !str {
		return
	}
	l.segs = append(l.segs, codeSeg{text: text, str: str})
}

// commentOnly: the line is a comment and nothing else (a continuation).
func (l lineScan) commentOnly() bool { return len(l.comments) > 0 && !l.code }

// lexState carries what spans lines.
type lexState struct {
	block int    // index+1 of the open block comment, 0 = none
	str   string // open multi-line string delimiter ("`", `"""`, "'''")
	doc   bool   // the open triple-quoted string is a docstring
	// docCode: the open docstring is not inert (an argument, a value, an
	// f-string): a comment for the todo check, code for CompareCode.
	docCode bool
	// unknown: the lines before are not known (a gap of the diff-only
	// fallback); only then is a "* " line taken for a docblock line.
	unknown bool
	// depth is the bracket depth and cont a trailing backslash of the
	// previous line: Python docstrings are inert only at depth 0, not
	// continued.
	depth int
	cont  bool
	// yaml: inside a YAML block scalar whose key line has indentation
	// yamlIndent (yamlKeep: "+" chomping, trailing blank lines count).
	yaml       bool
	yamlIndent int
	yamlKeep   bool
}

// scanComments runs the lexer over lines (1-based; gaps reset the state).
func scanComments(lines []srcLine, syn *commentSyntax) []lineScan {
	out, _ := scanFile(lines, syn)
	return out
}

// scanFile is scanComments that also returns the state after the last line.
func scanFile(lines []srcLine, syn *commentSyntax) ([]lineScan, lexState) {
	out := make([]lineScan, len(lines))
	var st lexState
	for i := 1; i < len(lines); i++ {
		if !lines[i].ok {
			st = lexState{unknown: true}
			continue
		}
		if syn.yamlBlocks && yamlBlockLine(lines, i, &st) {
			out[i] = lineScan{code: true, contStr: true, segs: []codeSeg{{text: lines[i].text, str: true}}}
			continue
		}
		out[i] = scanLine(lines[i].text, syn, &st)
		if syn.yamlBlocks {
			yamlBlockStart(lines[i].text, &st)
		}
	}
	return out, st
}

// yamlBlockIndicator: a line that ends with a block scalar indicator ("|",
// ">-", "|+2", maybe a comment after it). It may match more than YAML
// does: more lines are then code, never fewer.
var yamlBlockIndicator = regexp.MustCompile(`(^|[\s:?-])[|>][-+0-9]*(\s+#.*)?$`)

func yamlBlockStart(s string, st *lexState) {
	t := strings.TrimRight(s, " \t")
	if m := yamlBlockIndicator.FindString(t); m != "" {
		st.yaml, st.yamlIndent, st.yamlKeep = true, indentOf(s), strings.Contains(strings.SplitN(m, "#", 2)[0], "+")
	}
}

// yamlBlockLine reports whether line i is text of the open block scalar:
// indented more than its key, or a blank line followed by such a line
// (any blank line with "+" chomping).
func yamlBlockLine(lines []srcLine, i int, st *lexState) bool {
	if !st.yaml {
		return false
	}
	for j := i; j < len(lines) && lines[j].ok; j++ {
		t := lines[j].text
		if strings.TrimSpace(t) == "" {
			if st.yamlKeep {
				return true
			}
			continue
		}
		if indentOf(t) > st.yamlIndent {
			return true
		}
		break
	}
	st.yaml = false
	return false
}

func indentOf(s string) int { return len(s) - len(strings.TrimLeft(s, " \t")) }

func spaceBefore(s string, i int) bool { return i == 0 || s[i-1] == ' ' || s[i-1] == '\t' }

func blankBefore(s string, i int) bool { return strings.TrimSpace(s[:i]) == "" }

// resume continues what the previous line left open (a block comment, a
// multi-line string) or reads a docblock line. It returns where the rest
// of the line starts; done: the whole line is consumed.
func (ls *lineScan) resume(s string, syn *commentSyntax, st *lexState) (i int, done bool) {
	switch {
	case st.block > 0:
		end := syn.block[st.block-1][1]
		j := strings.Index(s, end)
		if j < 0 {
			ls.comment("", s)
			return 0, true
		}
		ls.comment("", s[:j])
		st.block = 0
		return j + len(end), false
	case st.str != "":
		j := closingQuote(s, 0, st.str)
		if j < 0 {
			ls.strText("", s, s, st, true)
			return 0, true
		}
		i = j + len(st.str)
		ls.strText("", s[:j], s[:i], st, true)
		if st.doc && !st.docCode {
			ls.unsure = ls.unsure || !restInert(s[i:])
		}
		st.str, st.doc, st.docCode = "", false, false
		return i, false
	case syn.star && st.unknown:
		if t := strings.TrimLeft(s, " \t"); strings.HasPrefix(t, "*") && !strings.HasPrefix(t, "*/") &&
			(len(t) == 1 || t[1] == ' ' || t[1] == '\t') {
			ls.comment("", t[1:])
			return 0, true
		}
	}
	return 0, false
}

// strText records a piece of a multi-line or triple-quoted string: text is
// its content (a docstring's comment text), raw the piece with its quotes;
// cont: the line starts inside the string.
func (ls *lineScan) strText(open, text, raw string, st *lexState, cont bool) {
	if st.doc && !st.docCode {
		ls.comment(open, text)
		return
	}
	if st.doc {
		ls.comments = append(ls.comments, text) // still a comment for the todo check
	} else {
		ls.code = ls.code || strings.TrimSpace(raw) != ""
	}
	ls.contStr = ls.contStr || cont
	ls.add(raw, true)
}

// restInert: what follows a docstring on its line ends the statement.
func restInert(rest string) bool {
	t := strings.TrimSpace(rest)
	return t == "" || t[0] == '#'
}

// scanLine lexes one line, updating the multi-line state.
func scanLine(s string, syn *commentSyntax, st *lexState) lineScan {
	var ls lineScan
	depth, cont := st.depth, st.cont
	st.cont = false
	i, done := ls.resume(s, syn, st)
	if done {
		return ls
	}
	var last byte // the last code character
	// run is the start of the current code run (-1: none).
	run := -1
	flush := func(end int) {
		if run >= 0 {
			ls.add(s[run:end], false)
			run = -1
		}
	}
	for i < len(s) {
		c := s[i]
		if c == ' ' || c == '\t' {
			if run < 0 {
				run = i
			}
			i++
			continue
		}
		// Python triple quotes (docstring when it starts the line).
		if syn.triple && (strings.HasPrefix(s[i:], `"""`) || strings.HasPrefix(s[i:], `'''`)) {
			flush(i)
			q := s[i : i+3]
			st.doc = blankBefore(s, i) || docPrefix(s, i)
			// Inert (a statement of its own): for CompareCode.
			st.docCode = st.doc && (depth != 0 || cont || !inertPrefix(s, i))
			j := closingQuote(s, i+3, q)
			if j < 0 {
				st.str = q
				ls.strText(q, s[i+3:], s[i:], st, false)
				return ls
			}
			ls.strText(q, s[i+3:j], s[i:j+3], st, false)
			if st.doc && !st.docCode && !restInert(s[j+3:]) {
				ls.unsure = true
			}
			st.doc, st.docCode = false, false
			i, last = j+3, '"'
			continue
		}
		if strings.IndexByte(syn.quotes, c) >= 0 && !syn.notQuote(s, i) {
			j := closingQuote(s, i+1, string(c))
			if j < 0 {
				if strings.IndexByte(syn.multi, c) >= 0 {
					flush(i)
					st.str = string(c)
					ls.code = true
					ls.add(s[i:], true)
					return ls
				}
				// Unterminated on this line (an apostrophe, a string
				// continued on the next line): not a string, a guess.
				ls.code, ls.unsure, last = true, true, c
				if run < 0 {
					run = i
				}
				i++
				continue
			}
			flush(i)
			ls.code = true
			ls.add(s[i:j+1], true)
			i, last = j+1, c
			continue
		}
		if k, ok := lineStarter(s, i, syn); ok {
			flush(i)
			ls.unsure = ls.unsure || escapedStarter(s, i, k)
			ls.comment(k, s[i+len(k):])
			return ls
		}
		if b, ok := blockStarter(s, i, syn); ok {
			flush(i)
			open, end := syn.block[b][0], syn.block[b][1]
			ls.unsure = ls.unsure || escapedStarter(s, i, open)
			j := strings.Index(s[i+len(open):], end)
			if j < 0 {
				st.block = b + 1
				ls.comment(open, s[i+len(open):])
				return ls
			}
			ls.comment(open, s[i+len(open):i+len(open)+j])
			i += len(open) + j + len(end)
			continue
		}
		ls.code, last = true, c
		switch c {
		case '(', '[', '{':
			st.depth++
		case ')', ']', '}':
			st.depth--
		}
		if run < 0 {
			run = i
		}
		i++
	}
	flush(len(s))
	st.cont = last == '\\'
	return ls
}

// escapedStarter: a comment starter right after a backslash or "$" (a regular
// expression, an Erlang character literal) or "/" starter after "[" (a
// character class) may be no comment at all.
func escapedStarter(s string, i int, k string) bool {
	if i == 0 {
		return false
	}
	p := s[i-1]
	return p == '\\' || p == '$' || (p == '[' && k[0] == '/')
}

// inertPrefix: nothing but r/u prefixes before the triple quote at i (an
// f-string runs code, b and the rest are not docstrings).
func inertPrefix(s string, i int) bool {
	for j := i - 1; j >= 0 && s[j] != ' ' && s[j] != '\t'; j-- {
		if strings.IndexByte("rRuU", s[j]) < 0 {
			return false
		}
	}
	return true
}

// docPrefix: r"""…""" / u”'…”' at the start of a line is a docstring too.
func docPrefix(s string, i int) bool {
	j := i
	for j > 0 && strings.IndexByte("rRuUbBfF", s[j-1]) >= 0 && i-j < 2 {
		j--
	}
	return j < i && blankBefore(s, j)
}

// closingQuote finds the closing delimiter q from position from (escapes
// with a backslash, except in backtick strings); -1 when not on the line.
func closingQuote(s string, from int, q string) int {
	for j := from; j < len(s); j++ {
		if s[j] == '\\' && q != "`" {
			j++
			continue
		}
		if strings.HasPrefix(s[j:], q) {
			return j
		}
	}
	return -1
}

func lineStarter(s string, i int, syn *commentSyntax) (string, bool) {
	for _, k := range syn.line {
		if !strings.HasPrefix(s[i:], k) {
			continue
		}
		if syn.spaced[k] && !spaceBefore(s, i) {
			continue
		}
		if syn.lineStart[k] && !blankBefore(s, i) {
			continue
		}
		if k == "%" && i > 0 && s[i-1] == '\\' {
			continue
		}
		return k, true
	}
	return "", false
}

func blockStarter(s string, i int, syn *commentSyntax) (int, bool) {
	for b, d := range syn.block {
		if strings.HasPrefix(s[i:], d[0]) {
			return b, true
		}
	}
	return 0, false
}
