package diag

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/fixcik/gitlab-review-bot/internal/learn"
)

// Redacted replaces every secret in stored strings; the same placeholder
// learn.Scrub writes, so redacted text reads alike everywhere.
const Redacted = learn.Redacted

// The secret patterns are the specific credential shapes of learn.Scrub
// (token formats, URLs with credentials, private keys, bearer and key=value
// secrets). learn.Scrub's generic "long hex" rule is left out on purpose:
// diagnostics are full of commit SHAs, which are not secrets; long opaque
// strings are handled by looksOpaque instead. They apply in this order:
// credURLRe, secretPatterns, keyValueSecretRe.
var (
	// credURLRe keeps the scheme and host readable: scheme://[скрыто]@host.
	credURLRe      = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s/@:"'<>]+:[^\s/@"'<>]+@`)
	secretPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{10,}`),
		regexp.MustCompile(`\bgl(?:dt|ft|oas|rt|cbt|ptt|soat|imt|agent)-[A-Za-z0-9_\-]{10,}`),
		regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{10,}`),
		regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}`),
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),
		regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9\-]{10,}`),
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`), // JWT
		regexp.MustCompile(`(?i)-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z ]*PRIVATE KEY-----|$)`),
		regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/\-]{16,}=*`),
	}
	// keyValueSecretRe keeps the key and the separator: password=[скрыто].
	keyValueSecretRe = regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?token)(["']?\s*[:=]\s*["']?)[^\s"',;]{6,}`)
)

// opaqueRe finds candidates for "long opaque string that looks like a key".
var opaqueRe = regexp.MustCompile(`[A-Za-z0-9_\-+]{40,}={0,2}`)

// scrubber removes secrets from strings.
type scrubber struct {
	literals []string // known secret values, longest first
}

func newScrubber(secrets []string) *scrubber {
	s := &scrubber{}
	seen := map[string]bool{}
	for _, v := range secrets {
		v = strings.TrimSpace(v)
		if len(v) < 8 || seen[v] {
			continue
		}
		seen[v] = true
		s.literals = append(s.literals, v)
	}
	sort.Slice(s.literals, func(i, j int) bool { return len(s.literals[i]) > len(s.literals[j]) })
	return s
}

// str scrubs one string. safe values (known commit SHAs) are never
// replaced by the opaque-string rule.
func (s *scrubber) str(v string, safe []string) string {
	if v == "" {
		return v
	}
	for _, lit := range s.literals {
		v = strings.ReplaceAll(v, lit, Redacted)
	}
	v = credURLRe.ReplaceAllStringFunc(v, func(m string) string {
		k := strings.Index(m, "://")
		return m[:k+3] + Redacted + "@"
	})
	for _, re := range secretPatterns {
		v = re.ReplaceAllString(v, Redacted)
	}
	v = keyValueSecretRe.ReplaceAllString(v, "${1}${2}"+Redacted)
	return opaqueRe.ReplaceAllStringFunc(v, func(m string) string {
		if looksOpaque(m) && !slices.Contains(safe, m) {
			return Redacted
		}
		return m
	})
}

// looksOpaque reports whether a long token mixes upper case, lower case and
// digits densely enough to be a random key rather than an identifier, a
// commit SHA (lower-case hex) or a path segment.
func looksOpaque(m string) bool {
	m = strings.TrimRight(m, "=")
	var up, lo, dig, trans int
	prev := 0
	for _, r := range m {
		c := 0
		switch {
		case unicode.IsUpper(r):
			up++
			c = 1
		case unicode.IsLower(r):
			lo++
			c = 2
		case unicode.IsDigit(r):
			dig++
			c = 3
		}
		if c != 0 && prev != 0 && c != prev {
			trans++
		}
		if c != 0 {
			prev = c
		}
	}
	// Random keys (base62/base64) mix all three classes throughout; an
	// identifier (TestFooBar2Times) has few digits and changes class mostly
	// at word starts.
	if up < 4 || lo < 4 || dig < 4 {
		return false
	}
	return trans*3 >= len(m)
}

// jsonLine scrubs every string inside one JSON value (keys included) and
// re-encodes it, so a replacement can never break the JSON structure.
// Invalid JSON is scrubbed as plain text and wrapped as a JSON string event.
func (s *scrubber) jsonLine(line []byte, safe []string) []byte {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		b, _ := json.Marshal(map[string]string{"type": "reviewbot_unparsed", "text": s.str(string(line), safe)})
		return b
	}
	v = s.walk(v, safe)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// v came from the decoder (json.Number, strings, maps, slices): it
	// always encodes.
	_ = enc.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func (s *scrubber) walk(v any, safe []string) any {
	switch x := v.(type) {
	case string:
		return s.str(x, safe)
	case []any:
		for i := range x {
			x[i] = s.walk(x[i], safe)
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[s.str(k, safe)] = s.walk(val, safe)
		}
		return out
	default:
		return v
	}
}

// ScrubFunc returns the scrubbing of stored diagnostics (literal secrets,
// credential patterns, opaque key-like strings; lower-case hex SHAs kept)
// as a function, for other stores of untrusted text such as the
// verification problems (internal/problems).
func ScrubFunc(secrets []string) func(string) string {
	s := newScrubber(secrets)
	return func(v string) string { return s.str(v, nil) }
}
