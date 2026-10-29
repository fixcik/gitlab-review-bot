package config

// Validation of .reviewbot.yaml: every problem of the file at once (syntax,
// unknown keys, wrong types, invalid values), with line numbers where the
// YAML parser knows them. ParseRepo is the strict loader built on it.

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

// MaxRepoConfigBytes caps the size of .reviewbot.yaml.
const MaxRepoConfigBytes = 256 << 10

// RepoIssue is one problem of a .reviewbot.yaml.
type RepoIssue struct {
	// Line is 1-based; 0 when the position is unknown.
	Line int
	// Key is the key path ("lint.disable", "path_instructions[1].path");
	// empty for the whole file.
	Key string
	// Message says what is wrong (English, like the service config errors).
	Message string

	// removed: the key no longer exists (removed.go); such issues make the
	// error match ErrRemovedKey.
	removed bool
}

// String renders "line 3: lint.disable: unknown tool ...".
func (i RepoIssue) String() string {
	var b strings.Builder
	if i.Line > 0 {
		fmt.Fprintf(&b, "line %d: ", i.Line)
	}
	if i.Key != "" {
		b.WriteString(i.Key)
		b.WriteString(": ")
	}
	b.WriteString(i.Message)
	return b.String()
}

// RepoErrors are all problems of a .reviewbot.yaml (ParseRepo's error).
type RepoErrors []RepoIssue

func (e RepoErrors) Error() string {
	parts := make([]string, len(e))
	for i, is := range e {
		parts[i] = is.String()
	}
	return RepoConfigPath + ": " + strings.Join(parts, "; ")
}

// Is makes errors.Is(err, ErrRemovedKey) true when one of the issues is a
// removed key (setup, tools, compat.commands, lint.custom, …).
func (e RepoErrors) Is(target error) bool {
	if target != ErrRemovedKey {
		return false
	}
	for _, is := range e {
		if is.removed {
			return true
		}
	}
	return false
}

// RepoIssuesOf returns the issues of a ParseRepo error (one issue with the
// error text for any other error, nil for nil).
func RepoIssuesOf(err error) RepoErrors {
	if err == nil {
		return nil
	}
	var re RepoErrors
	if errors.As(err, &re) {
		return re
	}
	return RepoErrors{{Message: err.Error()}}
}

// RepoTooLarge is the issue of a .reviewbot.yaml over MaxRepoConfigBytes
// (what ValidateRepo reports for it; callers that never read the whole
// file report the same).
func RepoTooLarge() RepoErrors {
	return RepoErrors{{Message: fmt.Sprintf("the file is larger than %d KiB", MaxRepoConfigBytes>>10)}}
}

// ValidateRepo parses .reviewbot.yaml and reports every problem it finds:
// a syntax error (then nothing else can be checked), unknown keys (a typo
// must not be silently ignored), removed keys (with the reason they are
// gone, see removed.go), values of the wrong type (one per
// top-level key) and invalid values (one per section). Empty input, or a
// file with only comments, is a valid empty config (all defaults). The
// returned config is nil when there are issues.
func ValidateRepo(b []byte) (*Repo, RepoErrors) {
	if len(b) > MaxRepoConfigBytes {
		return nil, RepoTooLarge()
	}
	var r Repo
	if len(strings.TrimSpace(string(b))) == 0 {
		return &r, nil
	}
	f, err := parser.ParseBytes(b, 0)
	if err != nil {
		return nil, RepoErrors{yamlIssue(err, "")}
	}
	var body ast.Node
	if len(f.Docs) > 0 {
		body = unwrap(f.Docs[0].Body)
	}
	if len(f.Docs) > 1 {
		return nil, RepoErrors{{Line: docLine(f.Docs[1]), Message: "only one YAML document is allowed"}}
	}
	if body == nil {
		return &r, nil // comments only
	}
	if _, null := body.(*ast.NullNode); null {
		return &r, nil
	}
	top, ok := mappingValues(body)
	if !ok {
		return nil, RepoErrors{{Line: nodeLine(body), Message: "the file must be a mapping of keys (key: value), see docs/configuration.md"}}
	}
	var issues RepoErrors
	checkKeys(reflect.TypeOf(r), top, "", &issues)

	// Types: the whole document at once; when it fails, each top-level key
	// separately, so that every section with a wrong type is reported.
	if err := yaml.Unmarshal(b, &r); err != nil {
		r = Repo{}
		issues = append(issues, decodeTopLevel(top, &r)...)
	}

	// Values: every section is checked even after another one failed.
	lineOf := func(key string) int {
		for _, mv := range top {
			if n, _ := keyName(mv.Key); n == key {
				return nodeLine(mv.Key)
			}
		}
		return 0
	}
	// add records a section's error: messages that name their key path
	// ("tbd.max_lines must be >= 0") are kept as they are, the others get
	// the key (sub is the key under the section when the message lacks it).
	add := func(section, sub string, err error) {
		if err == nil {
			return
		}
		is := RepoIssue{Line: lineOf(section), Message: err.Error()}
		if !strings.HasPrefix(is.Message, section) {
			is.Key = joinKey(section, sub)
		}
		issues = append(issues, is)
	}
	if r.Jira != nil {
		keys, err := normalizeProjectKeys(r.Jira.ProjectKeys)
		add("jira", "project_keys", err)
		r.Jira.ProjectKeys = keys
		for _, msg := range validateTitle("jira", r.Jira.TitlePrompt, r.Jira.TitleLanguage) {
			add("jira", "", errors.New(msg))
		}
	}
	add("context", "exclude", r.Context.normalize())
	if pis, err := normalizePathInstructions(r.PathInstructions, "path_instructions"); err != nil {
		add("path_instructions", "", err)
	} else {
		r.PathInstructions = pis
	}
	add("coverage", "", r.Coverage.normalize())
	add("tbd", "", r.TBD.normalize())
	add("migrations", "", r.Migrations.normalize())
	add("lint", "", r.Lint.normalize())
	add("todo", "", r.Todo.normalize())
	add("sysmap", "", r.Sysmap.normalize())
	add("labels", "", r.Labels.normalize())
	add("max_mr_lines", "", r.normalizeBigMR())
	add("ci", "", r.CI.normalize())
	if len(issues) == 0 {
		return &r, nil
	}
	sort.SliceStable(issues, func(i, j int) bool {
		li, lj := issues[i].Line, issues[j].Line
		if li == 0 || lj == 0 {
			return li != 0 && lj == 0
		}
		return li < lj
	})
	return nil, dedupIssues(issues)
}

// decodeTopLevel decodes each known top-level key into r separately and
// reports a type issue per key that fails.
func decodeTopLevel(top []*ast.MappingValueNode, r *Repo) RepoErrors {
	var issues RepoErrors
	rv := reflect.ValueOf(r).Elem()
	fields := yamlFields(rv.Type())
	for _, mv := range top {
		name, _ := keyName(mv.Key)
		fi, known := fields[name]
		if !known {
			continue
		}
		if _, null := unwrap(mv.Value).(*ast.NullNode); null {
			continue
		}
		fv := rv.Field(fi)
		// go-yaml does not allocate through a **T: decode into a fresh *T
		// for pointer fields.
		dst := fv.Addr()
		if fv.Kind() == reflect.Pointer {
			dst = reflect.New(fv.Type().Elem())
		}
		if err := yaml.NodeToValue(mv.Value, dst.Interface()); err != nil {
			issues = append(issues, yamlIssue(err, name))
			continue
		}
		if fv.Kind() == reflect.Pointer {
			fv.Set(dst)
		}
	}
	return issues
}

func dedupIssues(in RepoErrors) RepoErrors {
	seen := map[string]bool{}
	out := in[:0]
	for _, is := range in {
		if k := is.String(); !seen[k] {
			seen[k] = true
			out = append(out, is)
		}
	}
	return out
}

// yamlIssue converts a go-yaml error into an issue with its position.
func yamlIssue(err error, key string) RepoIssue {
	var ye yaml.Error
	if errors.As(err, &ye) {
		is := RepoIssue{Key: key, Message: oneLineMsg(ye.GetMessage())}
		if tk := ye.GetToken(); tk != nil && tk.Position != nil {
			is.Line = tk.Position.Line
		}
		return is
	}
	return RepoIssue{Key: key, Message: oneLineMsg(err.Error())}
}

func oneLineMsg(s string) string { return strings.Join(strings.Fields(s), " ") }

// checkKeys reports keys of a mapping that t (a struct) does not know, and
// descends into nested structs and lists of structs.
func checkKeys(t reflect.Type, values []*ast.MappingValueNode, prefix string, issues *RepoErrors) {
	fields := yamlFields(t)
	known := make([]string, 0, len(fields))
	for k := range fields {
		known = append(known, k)
	}
	sort.Strings(known)
	// Duplicate keys never get here: the parser refuses them.
	for _, mv := range values {
		name, merge := keyName(mv.Key)
		if merge {
			continue // "<<: *anchor": checked where the anchor is defined
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		fi, ok := fields[name]
		if !ok {
			if reason, gone := removedRepoKeyReason(path); gone {
				*issues = append(*issues, RepoIssue{Line: nodeLine(mv.Key), Key: path, Message: reason, removed: true})
				continue
			}
			msg := "unknown key"
			if s := closest(name, known); s != "" {
				msg += fmt.Sprintf(" (did you mean %q?)", joinKey(prefix, s))
			}
			*issues = append(*issues, RepoIssue{Line: nodeLine(mv.Key), Key: path, Message: msg})
			continue
		}
		ft := deref(t.Field(fi).Type)
		v := unwrap(mv.Value)
		switch {
		case ft.Kind() == reflect.Struct:
			if sub, ok := mappingValues(v); ok {
				checkKeys(ft, sub, path, issues)
			}
		case ft.Kind() == reflect.Slice && deref(ft.Elem()).Kind() == reflect.Struct:
			if seq, ok := v.(*ast.SequenceNode); ok {
				for i, item := range seq.Values {
					if sub, ok := mappingValues(unwrap(item)); ok {
						checkKeys(deref(ft.Elem()), sub, fmt.Sprintf("%s[%d]", path, i), issues)
					}
				}
			}
		}
	}
}

func joinKey(prefix, k string) string {
	if prefix == "" {
		return k
	}
	return prefix + "." + k
}

// deref strips pointer types (*T, **T) down to T.
func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// yamlFields maps yaml key names of struct t to field indexes.
func yamlFields(t reflect.Type) map[string]int {
	m := map[string]int{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		m[name] = i
	}
	return m
}

// unwrap strips tags, anchors and the explicit-key marker ("? key")
// around a node.
func unwrap(n ast.Node) ast.Node {
	for {
		switch v := n.(type) {
		case *ast.TagNode:
			n = v.Value
		case *ast.AnchorNode:
			n = v.Value
		case *ast.MappingKeyNode:
			n = v.Value
		default:
			return n
		}
	}
}

// mappingValues returns the entries of a mapping node (go-yaml v1.19 wraps
// every mapping, flow or block, one entry or more, in a MappingNode).
func mappingValues(n ast.Node) ([]*ast.MappingValueNode, bool) {
	if m, ok := n.(*ast.MappingNode); ok {
		return m.Values, true
	}
	return nil, false
}

// keyName returns the key text and whether it is a merge key ("<<"). Keys
// written as "? enabled", "!!str enabled" or "&a enabled" are the key
// "enabled", as for the decoder.
func keyName(k ast.MapKeyNode) (string, bool) {
	switch v := unwrap(k).(type) {
	case *ast.MergeKeyNode:
		return "<<", true
	case *ast.StringNode:
		return v.Value, false
	case nil:
		return "", false
	default:
		return strings.TrimSpace(v.String()), false
	}
}

func nodeLine(n ast.Node) int {
	if n == nil {
		return 0
	}
	if tk := n.GetToken(); tk != nil && tk.Position != nil {
		return tk.Position.Line
	}
	return 0
}

func docLine(d *ast.DocumentNode) int {
	if d.Start != nil && d.Start.Position != nil {
		return d.Start.Position.Line
	}
	return nodeLine(d.Body)
}

// closest returns the known key within edit distance 2 of name (the
// smallest distance, then alphabetical), or "".
func closest(name string, known []string) string {
	best, bestD := "", 3
	for _, k := range known {
		if d := editDistance(strings.ToLower(name), k); d < bestD {
			best, bestD = k, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}
