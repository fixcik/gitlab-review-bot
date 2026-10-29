package tracing

import (
	"math"
	"regexp"
	"strings"
	"unicode/utf8"
)

// clean scrubs and caps string values; drops non-finite doubles (JSON
// cannot carry them) and empty keys.
func (s *Span) clean(attrs []Attr) []Attr {
	if len(attrs) == 0 {
		return nil
	}
	out := make([]Attr, 0, len(attrs))
	for _, a := range attrs {
		if a.Key == "" {
			continue
		}
		switch a.Value.Type {
		case TypeString:
			a.Value.Str = s.tr.cleanString(a.Value.Str)
		case TypeFloat:
			if math.IsNaN(a.Value.F) || math.IsInf(a.Value.F, 0) {
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

func (t *Tracer) cleanString(v string) string {
	if t.scrub != nil {
		v = t.scrub(v)
	}
	return Truncate(StripURLQuery(v), MaxAttrLen)
}

// urlQueryRe matches the query (and fragment) of an http(s) URL in text.
var urlQueryRe = regexp.MustCompile(`(?i)(https?://[^\s?#"'<>]+)[?#][^\s"'<>]*`)

// StripURLQuery removes the query and fragment of every http(s) URL in s
// (tokens travel there: ?private_token=, presigned links). Applied to
// every string that goes into a span.
func StripURLQuery(s string) string {
	if !strings.Contains(s, "://") {
		return s
	}
	return urlQueryRe.ReplaceAllString(s, "$1")
}

// Truncate cuts s to at most n bytes on a rune boundary, marking the cut
// with "…" (included in n).
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const ell = "…"
	cut := max(n-len(ell), 0)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ell
}
