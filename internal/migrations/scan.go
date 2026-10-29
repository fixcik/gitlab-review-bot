package migrations

// A small lexer shared by the extractors. Source text is "masked": comments
// become spaces and the inside of every string literal becomes spaces too
// (newlines are kept, so offsets and line numbers stay the same). Structural
// regular expressions run on the masked text — they never match inside a
// comment or a string — and string values are looked up by offset.

import (
	"regexp"
	"sort"
	"strings"
)

// kwArgRe: a keyword argument ("name=value", PHP "name: value").
var kwArgRe = regexp.MustCompile(`^[A-Za-z_]\w*\s*(?:=[^=]|:[^:])`)

// lit is one string literal of the source.
type lit struct {
	start, end int // span in the source, quotes included
	// content is the offset where the value starts (after the opening
	// quote or the heredoc header line).
	content int
	value   string
}

// src is masked source text.
type src struct {
	raw        string
	code       string // masked
	lits       []lit  // sorted by start
	lineStarts []int
}

func newSrc(raw, code string, lits []lit) *src {
	s := &src{raw: raw, code: code, lits: lits, lineStarts: []int{0}}
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\n' {
			s.lineStarts = append(s.lineStarts, i+1)
		}
	}
	return s
}

// line is the 1-based line of offset off.
func (s *src) line(off int) int {
	return sort.Search(len(s.lineStarts), func(i int) bool { return s.lineStarts[i] > off })
}

// litsIn returns the literals that start inside [start, end).
func (s *src) litsIn(start, end int) []lit {
	i := sort.Search(len(s.lits), func(i int) bool { return s.lits[i].start >= start })
	var out []lit
	for ; i < len(s.lits) && s.lits[i].start < end; i++ {
		out = append(out, s.lits[i])
	}
	return out
}

// unmask returns code[start:end] with every literal's value put back
// between single quotes (for keyword checks on arguments).
func (s *src) unmask(start, end int) string {
	var b strings.Builder
	pos := start
	for _, l := range s.litsIn(start, end) {
		if l.end > end {
			break
		}
		b.WriteString(s.code[pos:l.start])
		b.WriteString("'" + l.value + "'")
		pos = l.end
	}
	b.WriteString(s.code[pos:end])
	return b.String()
}

// matchClose returns the offset of the bracket closing the one at open
// (masked code: brackets in strings and comments are gone), or -1.
func (s *src) matchClose(open int) int {
	depth := 0
	for i := open; i < len(s.code); i++ {
		switch s.code[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// span is a trimmed range of the source.
type span struct{ start, end int }

func (s *src) trimSpan(a, b int) span {
	for a < b && isSpace(s.code[a]) {
		a++
	}
	for b > a && isSpace(s.code[b-1]) {
		b--
	}
	return span{a, b}
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// args splits the arguments of the call whose opening bracket is at open.
// It returns nil when the bracket is not closed.
func (s *src) args(open int) []span {
	cl := s.matchClose(open)
	if cl < 0 {
		return nil
	}
	return s.split(open+1, cl, ',')
}

// split cuts [a, b) at top-level sep characters.
func (s *src) split(a, b int, sep byte) []span {
	var out []span
	depth, from := 0, a
	for i := a; i < b; i++ {
		switch c := s.code[i]; c {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		default:
			if c == sep && depth == 0 {
				if sp := s.trimSpan(from, i); sp.end > sp.start {
					out = append(out, sp)
				}
				from = i + 1
			}
		}
	}
	if sp := s.trimSpan(from, b); sp.end > sp.start {
		out = append(out, sp)
	}
	return out
}

func (s *src) text(sp span) string {
	if sp.start < 0 || sp.end > len(s.code) || sp.end < sp.start {
		return "" // a virtual argument (Alembic batch operations)
	}
	return s.code[sp.start:sp.end]
}

// str returns the value of an argument that is one string literal, or
// literals joined by "." / "+" / juxtaposition; ok=false otherwise.
func (s *src) str(sp span) (string, bool) {
	ls := s.litsIn(sp.start, sp.end)
	if len(ls) == 0 {
		return "", false
	}
	var b strings.Builder
	pos := sp.start
	for _, l := range ls {
		if strings.Trim(s.code[pos:l.start], " \t\r\n.+") != "" {
			return "", false
		}
		b.WriteString(l.value)
		pos = l.end
	}
	if strings.TrimSpace(s.code[pos:sp.end]) != "" {
		return "", false
	}
	return b.String(), true
}

// strs returns the values of an argument that is a string or a list of
// strings ("['a', 'b']", "array('a')").
func (s *src) strs(sp span) []string {
	if v, ok := s.str(sp); ok {
		return []string{v}
	}
	t := s.text(sp)
	open := strings.IndexAny(t, "([")
	if open < 0 {
		return nil
	}
	var out []string
	for _, a := range s.args(sp.start + open) {
		if v, ok := s.str(a); ok {
			out = append(out, v)
		}
	}
	return out
}

// sqlText is the SQL an argument carries: the literals inside it, joined
// (a "," between two literals — a list of statements — becomes ";"). The
// text is padded with newlines so that every literal's value starts on its
// own file line (line numbers inside the text stay file lines). The offset
// is where the first value starts.
func (s *src) sqlText(sp span) (string, int, bool) {
	ls := s.litsIn(sp.start, sp.end)
	if len(ls) == 0 {
		return "", 0, false
	}
	var b strings.Builder
	line := s.line(ls[0].content) // file line at the end of b
	for i, l := range ls {
		if i > 0 {
			if strings.Contains(s.code[ls[i-1].end:l.start], ",") {
				b.WriteString(";")
			}
			if want := s.line(l.content); want > line {
				b.WriteString(strings.Repeat("\n", want-line))
				line = want
			}
		}
		b.WriteString(l.value)
		line += strings.Count(l.value, "\n")
	}
	return b.String(), ls[0].content, true
}

// kwarg finds a keyword argument "name=value" (Python) / "name: value"
// (PHP named arguments) among args; it returns the value span.
func (s *src) kwarg(args []span, name string) (span, bool) {
	for _, a := range args {
		t := s.text(a)
		if !strings.HasPrefix(t, name) {
			continue
		}
		rest := strings.TrimLeft(t[len(name):], " \t\n")
		if strings.HasPrefix(rest, "==") {
			continue
		}
		if strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, ":") && !strings.HasPrefix(rest, "::") {
			off := a.end - len(rest) + 1
			return s.trimSpan(off, a.end), true
		}
	}
	return span{}, false
}

// positional returns the i-th argument that is not a keyword argument.
func (s *src) positional(args []span, i int) (span, bool) {
	n := 0
	for _, a := range args {
		if kwArgRe.MatchString(s.text(a)) {
			continue
		}
		if n == i {
			return a, true
		}
		n++
	}
	return span{}, false
}

// arg is a positional argument or, failing that, the keyword one.
func (s *src) arg(args []span, i int, name string) (span, bool) {
	if name != "" {
		if sp, ok := s.kwarg(args, name); ok {
			return sp, true
		}
	}
	return s.positional(args, i)
}

// regionsContain reports whether off is inside one of the spans.
func regionsContain(rs []span, off int) bool {
	for _, r := range rs {
		if off >= r.start && off < r.end {
			return true
		}
	}
	return false
}
