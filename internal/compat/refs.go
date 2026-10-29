package compat

import (
	"bytes"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
)

// External $refs of OpenAPI specs.
//
// `oasdiff --allow-external-refs=false` (an SSRF guard: oasdiff fetches URL
// $refs by default) also refuses relative file $refs, so multi-file specs
// would no longer load. The bot therefore scans the materialized spec on the
// host, following file $refs transitively:
//
//   - no external $ref at all: --allow-external-refs=false (any $ref the
//     scan missed fails closed: oasdiff refuses it);
//   - only relative file $refs that stay inside the materialized side
//     (old/ or new/): --allow-external-refs=true, relying on the sandbox
//     (--network none) as the SSRF barrier;
//   - a URL, an absolute path, a path that leaves the side, or a value the
//     scan cannot read safely (YAML block scalars, tags, anchors, escapes):
//     oasdiff is not run, the check is an Issue.

type refPolicy int

const (
	refsNone    refPolicy = iota // no external $ref
	refsLocal                    // only relative files inside the side
	refsRefused                  // URL / outside / unreadable
)

// refRe finds "$ref" keys (YAML or JSON, quoted or not) up to the value.
// Every key is matched, also several on one line (minified JSON).
var refRe = regexp.MustCompile(`["']?\$ref["']?[ \t]*:[ \t]*`)

// maxRefFiles bounds the transitive scan.
const maxRefFiles = 2000

// maxRefLine bounds the text read after one "$ref:" key; a longer value is
// cut, so a quoted one stays unterminated and is refused.
const maxRefLine = 4096

// scanRefs classifies the external $refs reachable from spec in the side
// directory sideDir (absolute) and returns a reason for refsRefused.
func scanRefs(sideDir, spec string) (refPolicy, string) {
	root, err := os.OpenRoot(sideDir)
	if err != nil {
		return refsRefused, "не удалось прочитать спецификацию"
	}
	defer root.Close()
	policy := refsNone
	seen := map[string]bool{spec: true}
	queue := []string{spec}
	for len(queue) > 0 {
		f := queue[0]
		queue = queue[1:]
		b, ok := readRoot(root, f)
		if !ok {
			continue // not materialized: oasdiff reports the missing file
		}
		for _, loc := range refRe.FindAllIndex(b, -1) {
			rest := b[loc[1]:min(len(b), loc[1]+maxRefLine)]
			if i := bytes.IndexAny(rest, "\r\n"); i >= 0 {
				rest = rest[:i]
			}
			v, ok := refValue(string(rest))
			if !ok {
				return refsRefused, "спецификация содержит $ref, который бот не может безопасно разобрать"
			}
			file, _, _ := strings.Cut(v, "#")
			if file == "" {
				continue // local: #/components/...
			}
			if strings.Contains(file, ":") || strings.HasPrefix(file, "//") || strings.HasPrefix(file, "/") {
				return refsRefused, "спецификация ссылается на внешний $ref по URL или абсолютному пути (" + shortText(file, 80) + ") — такие ссылки не загружаются"
			}
			target := path.Clean(path.Join(path.Dir(f), file))
			if target == ".." || strings.HasPrefix(target, "../") {
				return refsRefused, "спецификация ссылается на файл вне репозитория (" + shortText(file, 80) + ")"
			}
			policy = refsLocal
			if !seen[target] && len(seen) < maxRefFiles {
				seen[target] = true
				queue = append(queue, target)
			}
		}
	}
	return policy, ""
}

// refValue extracts a $ref value from the rest of its line. ok=false for
// values the scan cannot read reliably.
func refValue(rest string) (string, bool) {
	v := strings.TrimSpace(rest)
	if v == "" {
		return "", false // value on the next line (block scalar, ...)
	}
	switch v[0] {
	case '"', '\'':
		end := strings.IndexByte(v[1:], v[0])
		if end < 0 {
			return "", false
		}
		v = v[1 : end+1]
	case '|', '>', '&', '*', '!', '[', '{', '?':
		return "", false
	default:
		if i := strings.IndexAny(v, " \t,}]"); i >= 0 {
			v = v[:i]
		}
	}
	if strings.ContainsAny(v, "\\%") {
		return "", false // escapes could hide a scheme
	}
	return strings.TrimSpace(v), true
}

func readRoot(root *os.Root, name string) ([]byte, bool) {
	if !safeRel(name) {
		return nil, false
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(f, maxBytesPerSide))
	if err != nil {
		return nil, false
	}
	return b, true
}
