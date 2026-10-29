// Package dotenv reads and updates a compose-style .env file (KEY=VALUE per
// line) without losing what it does not know: comments, blank lines and
// other keys are kept as they are, only the given keys change.
//
// Values are written so that docker compose reads them literally: a value
// with anything but [A-Za-z0-9_./:@+,=-] is single-quoted (no ${...}
// interpolation inside single quotes); a value with a newline or a single
// quote is refused.
package dotenv

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Mode is the permission of a written .env: it holds secrets.
const Mode os.FileMode = 0o600

var keyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// line splits "KEY=VALUE" (optionally "export KEY=VALUE"); ok is false for
// comments, blank lines and anything else.
func line(s string) (key, raw string, ok bool) {
	t := strings.TrimSpace(s)
	if t == "" || strings.HasPrefix(t, "#") {
		return "", "", false
	}
	t = strings.TrimPrefix(t, "export ")
	k, v, found := strings.Cut(t, "=")
	k = strings.TrimSpace(k)
	if !found || !keyRe.MatchString(k) {
		return "", "", false
	}
	return k, strings.TrimSpace(v), true
}

// unquote reads a raw value as compose does: '...' literal, "..." with \"
// and \\, unquoted up to an inline " #" comment.
func unquote(v string) string {
	switch {
	case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
		return v[1 : len(v)-1]
	case len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"':
		return strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(v[1 : len(v)-1])
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

// Parse returns the KEY=VALUE pairs of b (a later line wins).
func Parse(b []byte) map[string]string {
	out := map[string]string{}
	for _, s := range strings.Split(string(b), "\n") {
		if k, v, ok := line(s); ok {
			out[k] = unquote(v)
		}
	}
	return out
}

// Read parses the file at path; a missing file is an empty set.
func Read(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(b), nil
}

var plainRe = regexp.MustCompile(`^[A-Za-z0-9_./:@+,=-]*$`)

// format renders KEY=VALUE.
func format(k, v string) (string, error) {
	if strings.ContainsAny(v, "\n\r'") {
		return "", fmt.Errorf("%s: a value with a newline or a single quote cannot be written to .env", k)
	}
	if plainRe.MatchString(v) {
		return k + "=" + v, nil
	}
	return k + "='" + v + "'", nil
}

// Update sets the keys of set in b: an existing line is rewritten in place
// (the first one; later duplicates are dropped), a new key is appended in
// sorted order. Everything else stays.
func Update(b []byte, set map[string]string) ([]byte, error) {
	for k := range set {
		if !keyRe.MatchString(k) {
			return nil, fmt.Errorf("invalid .env key %q", k)
		}
	}
	text := string(b)
	lines := strings.Split(text, "\n")
	if strings.HasSuffix(text, "\n") || text == "" {
		lines = lines[:len(lines)-1]
	}
	done := map[string]bool{}
	out := make([]string, 0, len(lines)+len(set))
	for _, s := range lines {
		k, _, ok := line(s)
		v, want := set[k]
		switch {
		case !ok || !want:
			out = append(out, s)
		case done[k]:
			// duplicate of a rewritten key: dropped, the value is set once
		default:
			l, err := format(k, v)
			if err != nil {
				return nil, err
			}
			out = append(out, l)
			done[k] = true
		}
	}
	var add []string
	for k := range set {
		if !done[k] {
			add = append(add, k)
		}
	}
	slices.Sort(add)
	for _, k := range add {
		l, err := format(k, set[k])
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return []byte(strings.Join(out, "\n") + "\n"), nil
}

// WriteFile writes b to path with Mode: through a temporary file in the
// same directory and a rename, so a reader never sees half a file and an
// existing file with looser permissions ends up 600.
func WriteFile(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".env-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after the rename
	// os.CreateTemp creates the file with mode 0600 (= Mode).
	_, werr := f.Write(b)
	if err := errors.Join(werr, f.Close()); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
