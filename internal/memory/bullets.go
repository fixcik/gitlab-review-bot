package memory

import (
	"strings"
	"unicode"

	"github.com/fixcik/gitlab-review-bot/internal/learn"
)

// Bullets parses a section's Markdown into bullet texts (without "- ").
func Bullets(content string) []string {
	var out []string
	for _, l := range strings.Split(content, "\n") {
		l = strings.TrimSpace(l)
		l = strings.TrimSpace(strings.TrimLeft(l, "-*•"))
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Render turns bullets into the stored Markdown.
func Render(bullets []string) string {
	if len(bullets) == 0 {
		return ""
	}
	var b strings.Builder
	for _, x := range bullets {
		b.WriteString("- ")
		b.WriteString(x)
		b.WriteString("\n")
	}
	return b.String()
}

// Size is the rendered size of bullets in characters (runes).
func Size(bullets []string) int { return len([]rune(Render(bullets))) }

// CleanBullet turns Claude's / a user's text into a stored bullet: HTML
// comments removed, secrets scrubbed (tokens, URLs with credentials, keys),
// one line, without a leading list marker, capped. Empty when nothing
// useful is left.
func CleanBullet(s string) string {
	s = learn.StripHTMLComments(s)
	s = learn.Scrub(s)
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimSpace(strings.TrimLeft(s, "-*• "))
	if r := []rune(s); len(r) > MaxBulletRunes {
		s = strings.TrimSpace(string(r[:MaxBulletRunes])) + "…"
	}
	if Key(strings.ReplaceAll(s, learn.Redacted, "")) == "" {
		return ""
	}
	return s
}

// Key is the dedupe key of a bullet: lowercased words without punctuation,
// backticks and quotes.
func Key(s string) string {
	f := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '/' || r == '_' || r == '-' || r == '=')
	})
	out := f[:0]
	for _, w := range f {
		if w = strings.Trim(w, ".-"); w != "" {
			out = append(out, w)
		}
	}
	return strings.Join(out, " ")
}

// Similar reports whether two bullets say (nearly) the same thing: equal
// keys, or word sets that overlap by at least 85% (Jaccard) for bullets of
// four or more words.
func Similar(a, b string) bool {
	ka, kb := Key(a), Key(b)
	if ka == "" || kb == "" {
		return false
	}
	if ka == kb {
		return true
	}
	wa, wb := strings.Fields(ka), strings.Fields(kb)
	if len(wa) < 4 || len(wb) < 4 {
		return false
	}
	set := map[string]int{}
	for _, w := range wa {
		set[w] |= 1
	}
	for _, w := range wb {
		set[w] |= 2
	}
	inter := 0
	for _, v := range set {
		if v == 3 {
			inter++
		}
	}
	return float64(inter)/float64(len(set)) >= 0.85
}

// Dedupe drops near-identical bullets, keeping the LAST (newest) one of each
// group in its position.
func Dedupe(bullets []string) []string {
	var out []string
	for i, b := range bullets {
		dup := false
		for _, later := range bullets[i+1:] {
			if Similar(b, later) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, b)
		}
	}
	return out
}

// TrimNewest keeps the newest bullets (the end of the list) that fit into
// limit characters.
func TrimNewest(bullets []string, limit int) []string {
	if limit <= 0 || Size(bullets) <= limit {
		return bullets
	}
	total, start := 0, len(bullets)
	for i := len(bullets) - 1; i >= 0; i-- {
		n := len([]rune(bullets[i])) + 3 // "- " + "\n"
		if total+n > limit {
			break
		}
		total += n
		start = i
	}
	return append([]string(nil), bullets[start:]...)
}

// FitTotal drops the oldest bullets of the largest sections until the total
// size fits limit. It returns the keys of the sections it changed.
func FitTotal(mem map[string][]string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	changed := map[string]bool{}
	for {
		total, biggest, bigSize := 0, "", -1
		for _, k := range Sections {
			n := Size(mem[k])
			total += n
			if n > bigSize && len(mem[k]) > 0 {
				biggest, bigSize = k, n
			}
		}
		if total <= limit || biggest == "" {
			break
		}
		mem[biggest] = mem[biggest][1:]
		changed[biggest] = true
	}
	var out []string
	for _, k := range Sections {
		if changed[k] {
			out = append(out, k)
		}
	}
	return out
}
