package migrations

import "strings"

type tokKind int

const (
	tWord   tokKind = iota + 1 // bare word / keyword / identifier
	tQuoted                    // "ident" or `ident`
	tString                    // masked string literal
	tPunct
)

type tok struct {
	kind tokKind
	text string // identifier without quotes; punctuation char
	up   string // upper-case text of words
	off  int    // offset in the analysed text
}

func (t tok) ident() bool { return t.kind == tWord || t.kind == tQuoted }

func (t tok) is(kws ...string) bool {
	if t.kind != tWord {
		return false
	}
	for _, k := range kws {
		if t.up == k {
			return true
		}
	}
	return false
}

func (t tok) punct(c string) bool { return t.kind == tPunct && t.text == c }

// tokenize splits masked SQL code[a:b] into tokens.
func tokenize(code string, a, b int) []tok {
	var ts []tok
	for i := a; i < b; {
		c := code[i]
		switch {
		case isSpace(c):
			i++
		case c == '\'':
			j := strings.IndexByte(code[i+1:b], '\'')
			end := b
			if j >= 0 {
				end = i + 1 + j + 1
			}
			ts = append(ts, tok{kind: tString, off: i})
			i = end
		case c == '"' || c == '`':
			j := strings.IndexByte(code[i+1:b], c)
			end := b
			if j >= 0 {
				end = i + 1 + j
			}
			ts = append(ts, tok{kind: tQuoted, text: code[i+1 : end], off: i})
			i = min(end+1, b)
		case isIdent(c) || c == '$' || c >= 0x80:
			j := i
			for j < b && (isIdent(code[j]) || code[j] == '$' || code[j] >= 0x80) {
				j++
			}
			w := code[i:j]
			ts = append(ts, tok{kind: tWord, text: w, up: strings.ToUpper(w), off: i})
			i = j
		default:
			ts = append(ts, tok{kind: tPunct, text: string(c), off: i})
			i++
		}
	}
	return ts
}

// name reads a possibly schema-qualified identifier at ts[i]; it returns
// the last part and the index after it ("" when there is none).
func name(ts []tok, i int) (string, int) {
	if i >= len(ts) || !ts[i].ident() {
		return "", i
	}
	n := ts[i].text
	i++
	for i+1 < len(ts) && ts[i].punct(".") && ts[i+1].ident() {
		n = ts[i+1].text
		i += 2
	}
	return n, i
}

// skip advances over the optional keywords at ts[i].
func skip(ts []tok, i int, kws ...string) int {
	for i < len(ts) && ts[i].is(kws...) {
		i++
	}
	return i
}

// skipIf skips the keyword sequence seq when it is at ts[i].
func skipIf(ts []tok, i int, seq ...string) int {
	if hasSeqAt(ts, i, seq...) {
		return i + len(seq)
	}
	return i
}

func hasSeqAt(ts []tok, i int, seq ...string) bool {
	if i+len(seq) > len(ts) {
		return false
	}
	for k, w := range seq {
		if !ts[i+k].is(w) {
			return false
		}
	}
	return true
}

// hasSeq reports the keyword sequence anywhere in ts.
func hasSeq(ts []tok, seq ...string) bool {
	for i := range ts {
		if hasSeqAt(ts, i, seq...) {
			return true
		}
	}
	return false
}

// topLevel splits ts at commas outside parentheses.
func topLevel(ts []tok) [][]tok {
	var out [][]tok
	depth, from := 0, 0
	for i, t := range ts {
		switch {
		case t.punct("("):
			depth++
		case t.punct(")"):
			depth--
		case t.punct(",") && depth == 0:
			if i > from {
				out = append(out, ts[from:i])
			}
			from = i + 1
		}
	}
	if from < len(ts) {
		out = append(out, ts[from:])
	}
	return out
}

// hasTopKw reports a keyword outside parentheses.
func hasTopKw(ts []tok, kw string) bool {
	depth := 0
	for _, t := range ts {
		switch {
		case t.punct("("):
			depth++
		case t.punct(")"):
			depth--
		case depth == 0 && t.is(kw):
			return true
		}
	}
	return false
}

// lastIdent is the last identifier of ts that is not one of the keywords.
func lastIdent(ts []tok, kws ...string) string {
	for i := len(ts) - 1; i >= 0; i-- {
		if ts[i].ident() && !ts[i].is(kws...) {
			return ts[i].text
		}
	}
	return ""
}
