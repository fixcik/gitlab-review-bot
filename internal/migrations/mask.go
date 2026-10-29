package migrations

import "strings"

type masker struct {
	raw  string
	code []byte
	lits []lit
}

func newMasker(raw string) *masker { return &masker{raw: raw, code: []byte(raw)} }

// blank masks [a, b) keeping newlines.
func (m *masker) blank(a, b int) {
	for i := a; i < b && i < len(m.code); i++ {
		if m.code[i] != '\n' {
			m.code[i] = ' '
		}
	}
}

// literal masks a string literal [a, b) as '   ' and records its value.
func (m *masker) literal(a, b, content int, value string) {
	if b > len(m.code) {
		b = len(m.code)
	}
	m.blank(a, b)
	if b-a >= 2 {
		m.code[a], m.code[b-1] = '\'', '\''
	} else if b > a {
		m.code[a] = '\''
	}
	m.lits = append(m.lits, lit{start: a, end: b, content: content, value: value})
}

func (m *masker) src() *src { return newSrc(m.raw, string(m.code), m.lits) }

// lineComment masks a comment from i to the end of its line and returns
// where scanning resumes.
func (m *masker) lineComment(i int) int {
	e := lineEnd(m.raw, i)
	m.blank(i, e)
	return e
}

// blockComment masks the /* */ comment at i (to the end of the text when
// it is not closed) and returns where scanning resumes.
func (m *masker) blockComment(i int) int {
	e := strings.Index(m.raw[i+2:], "*/")
	if e < 0 {
		m.blank(i, len(m.raw))
		return len(m.raw)
	}
	end := i + 2 + e + 2
	m.blank(i, end)
	return end
}

// lineEnd is the offset of the newline ending the line at i (or len).
func lineEnd(s string, i int) int {
	if j := strings.IndexByte(s[i:], '\n'); j >= 0 {
		return i + j
	}
	return len(s)
}

// maskSQL masks SQL: -- and /* */ comments, '...' strings (” escapes;
// backslash escapes for MySQL and E'...'), $tag$...$tag$ bodies. Quoted
// identifiers ("x", `x`) are kept.
func maskSQL(raw string, d Dialect) *src {
	m := newMasker(raw)
	n := len(raw)
	for i := 0; i < n; {
		c := raw[i]
		switch {
		case c == '-' && i+1 < n && raw[i+1] == '-', c == '#' && d == MySQL:
			i = m.lineComment(i)
		case c == '/' && i+1 < n && raw[i+1] == '*':
			i = m.blockComment(i)
		case c == '\'':
			esc := d == MySQL || (i > 0 && (raw[i-1] == 'E' || raw[i-1] == 'e') && (i < 2 || !isIdent(raw[i-2])))
			end, v := sqlString(raw, i, esc)
			m.literal(i, end, i+1, v)
			i = end
		case c == '"' || c == '`':
			j := strings.IndexByte(raw[i+1:], c)
			if j < 0 {
				i = n
				continue
			}
			i += j + 2
		case c == '$' && (i == 0 || !isIdent(raw[i-1])):
			// $tag$ ... $tag$ (tag may be empty; $1 is a parameter).
			j := i + 1
			for j < n && isIdent(raw[j]) && !(j == i+1 && raw[j] >= '0' && raw[j] <= '9') {
				j++
			}
			if j < n && raw[j] == '$' && !(j > i+1 && raw[i+1] >= '0' && raw[i+1] <= '9') {
				tag := raw[i : j+1]
				e := strings.Index(raw[j+1:], tag)
				end := n
				if e >= 0 {
					end = j + 1 + e + len(tag)
				}
				valEnd := max(end-len(tag), j+1)
				m.literal(i, end, j+1, raw[j+1:valEnd])
				i = end
				continue
			}
			i++
		default:
			i++
		}
	}
	return m.src()
}

// sqlString reads the '...' literal at i (a doubled quote escapes one;
// backslash escapes when esc) and returns its end (after the closing
// quote, or the end of the text) and its value.
func sqlString(raw string, i int, esc bool) (end int, value string) {
	n := len(raw)
	j := i + 1
	var b strings.Builder
	for j < n {
		if esc && raw[j] == '\\' && j+1 < n {
			b.WriteByte(raw[j+1])
			j += 2
			continue
		}
		if raw[j] == '\'' {
			if j+1 < n && raw[j+1] == '\'' {
				b.WriteByte('\'')
				j += 2
				continue
			}
			break
		}
		b.WriteByte(raw[j])
		j++
	}
	return min(j+1, n), b.String()
}

func isIdent(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// unescape decodes backslash escapes of PHP double-quoted and Python
// strings (the common ones; others stay as written; \n becomes a space).
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			// An escaped line break is not a source line: a space keeps
			// the SQL tokens apart and line numbers on file lines.
			b.WriteByte(' ')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '\\', '"', '\'', '$', '`':
			b.WriteByte(s[i])
		default:
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// maskPHP masks PHP: //, # (not #[ attributes) and /* */ comments; '...',
// "..." strings; heredoc and nowdoc.
func maskPHP(raw string) *src {
	m := newMasker(raw)
	n := len(raw)
	for i := 0; i < n; {
		c := raw[i]
		switch {
		case c == '/' && i+1 < n && raw[i+1] == '/', c == '#' && !(i+1 < n && raw[i+1] == '['):
			i = m.lineComment(i)
		case c == '/' && i+1 < n && raw[i+1] == '*':
			i = m.blockComment(i)
		case c == '\'' || c == '"':
			j := i + 1
			for j < n && raw[j] != c {
				if raw[j] == '\\' {
					j++
				}
				j++
			}
			end := min(j+1, n)
			v := raw[i+1 : max(end-1, i+1)]
			if c == '\'' {
				v = strings.NewReplacer(`\\`, `\`, `\'`, `'`).Replace(v)
			} else {
				v = unescape(v)
			}
			m.literal(i, end, i+1, v)
			i = end
		case c == '<' && strings.HasPrefix(raw[i:], "<<<"):
			if end, content, v, ok := heredoc(raw, i); ok {
				m.literal(i, end, content, v)
				i = end
				continue
			}
			i += 3
		default:
			i++
		}
	}
	return m.src()
}

// heredoc parses <<<ID / <<<"ID" / <<<'ID' at i up to the closing ID.
func heredoc(raw string, i int) (end, content int, value string, ok bool) {
	j := i + 3
	for j < len(raw) && (raw[j] == ' ' || raw[j] == '\t') {
		j++
	}
	q := byte(0)
	if j < len(raw) && (raw[j] == '\'' || raw[j] == '"') {
		q = raw[j]
		j++
	}
	k := j
	for k < len(raw) && isIdent(raw[k]) {
		k++
	}
	id := raw[j:k]
	if id == "" {
		return 0, 0, "", false
	}
	if q != 0 {
		if k >= len(raw) || raw[k] != q {
			return 0, 0, "", false
		}
		k++
	}
	nl := strings.IndexByte(raw[k:], '\n')
	if nl < 0 {
		return 0, 0, "", false
	}
	content = k + nl + 1
	// The closing identifier: on its own line (PHP 7.3+: may be indented),
	// followed by a non-identifier character.
	for p := content; p < len(raw); {
		le := lineEnd(raw, p)
		line := raw[p:le]
		t := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(t, id) && (len(t) == len(id) || !isIdent(t[len(id)])) {
			indent := len(line) - len(t)
			var lines []string
			if p > content {
				lines = strings.Split(raw[content:p-1], "\n")
			}
			for x, l := range lines {
				if len(l) >= indent && strings.TrimLeft(l[:indent], " \t") == "" {
					lines[x] = l[indent:]
				}
			}
			v := strings.Join(lines, "\n")
			if q != '\'' {
				v = unescape(v)
			}
			return p + indent + len(id), content, v, true
		}
		if le == len(raw) {
			break
		}
		p = le + 1
	}
	return 0, 0, "", false
}

// maskPython masks Python: # comments; '...', "...", triple-quoted
// strings with optional r/b/u/f prefixes.
func maskPython(raw string) *src {
	m := newMasker(raw)
	n := len(raw)
	for i := 0; i < n; {
		c := raw[i]
		switch {
		case c == '#':
			i = m.lineComment(i)
		case c == '\'' || c == '"':
			rawStr := false
			for p := i - 1; p >= 0 && p >= i-2 && isIdent(raw[p]); p-- {
				if raw[p] == 'r' || raw[p] == 'R' {
					rawStr = true
				}
			}
			q := string(c)
			if strings.HasPrefix(raw[i:], q+q+q) {
				q = q + q + q
			}
			j := i + len(q)
			closed := false
			for j < n {
				if strings.HasPrefix(raw[j:], q) {
					closed = true
					break
				}
				if raw[j] == '\\' {
					j++
				} else if len(q) == 1 && raw[j] == '\n' {
					break // unterminated
				}
				j++
			}
			end := min(j+len(q), n)
			if !closed {
				end = min(j, n)
			}
			v := raw[min(i+len(q), n):min(j, n)]
			if !rawStr {
				v = unescape(v)
			}
			m.literal(i, end, i+len(q), v)
			i = end
		default:
			i++
		}
	}
	return m.src()
}
