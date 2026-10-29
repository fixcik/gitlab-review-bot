package migrations

import (
	"regexp"
	"strconv"
	"strings"
)

// Range is an inclusive range of lines.
type Range struct{ From, To int }

// DiffFile is one file of a unified `git diff`.
type DiffFile struct {
	OldPath, NewPath string
	New, Deleted     bool
	// Added are the added lines (new side).
	Added []Range
	// Removed: the diff removes lines (old side).
	Removed bool
}

// Existed reports that the file existed at the diff's base under OldPath.
func (f DiffFile) Existed() bool { return !f.New && f.OldPath != "" }

// Changed reports a content change (lines added or removed) or a rename.
func (f DiffFile) Changed() bool { return len(f.Added) > 0 || f.Removed || f.OldPath != f.NewPath }

var hunkRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// FilesFromDiff parses a `git diff` (with -M) into files.
func FilesFromDiff(diff string) []DiffFile {
	var out []DiffFile
	var cur *DiffFile
	newLine := 0
	inHunk := false
	flush := func() {
		if cur != nil && (cur.NewPath != "" || cur.OldPath != "") {
			out = append(out, *cur)
		}
	}
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			flush()
			cur = &DiffFile{}
			inHunk = false
			// Fallback paths for diffs without ---/+++ (pure renames).
			if a, b, ok := gitHeaderPaths(l[len("diff --git "):]); ok {
				cur.OldPath, cur.NewPath = a, b
			}
			continue
		case cur == nil:
			continue
		case !inHunk && strings.HasPrefix(l, "new file mode"):
			cur.New = true
			continue
		case !inHunk && strings.HasPrefix(l, "deleted file mode"):
			cur.Deleted = true
			continue
		case !inHunk && strings.HasPrefix(l, "rename from "):
			cur.OldPath = l[len("rename from "):]
			continue
		case !inHunk && strings.HasPrefix(l, "rename to "):
			cur.NewPath = l[len("rename to "):]
			continue
		case !inHunk && strings.HasPrefix(l, "--- "):
			if p := strings.TrimSpace(l[4:]); p == "/dev/null" {
				cur.OldPath, cur.New = "", true
			} else if strings.HasPrefix(p, "a/") {
				cur.OldPath = p[2:]
			}
			continue
		case !inHunk && strings.HasPrefix(l, "+++ "):
			if p := strings.TrimSpace(l[4:]); p == "/dev/null" {
				cur.NewPath, cur.Deleted = "", true
			} else if strings.HasPrefix(p, "b/") {
				cur.NewPath = p[2:]
			}
			continue
		case strings.HasPrefix(l, "@@ "):
			m := hunkRe.FindStringSubmatch(l)
			inHunk = m != nil
			if m != nil {
				newLine, _ = strconv.Atoi(m[1])
			}
			continue
		}
		if !inHunk {
			continue
		}
		switch {
		case strings.HasPrefix(l, "+"):
			if k := len(cur.Added); k > 0 && cur.Added[k-1].To == newLine-1 {
				cur.Added[k-1].To = newLine
			} else {
				cur.Added = append(cur.Added, Range{newLine, newLine})
			}
			newLine++
		case strings.HasPrefix(l, "-"):
			cur.Removed = true
		case strings.HasPrefix(l, `\`):
		default:
			newLine++
		}
	}
	flush()
	for i := range out {
		if out[i].New {
			out[i].OldPath = ""
		}
		if out[i].Deleted {
			out[i].NewPath = ""
		}
	}
	return out
}

// gitHeaderPaths splits "a/x b/y" (unquoted paths only).
func gitHeaderPaths(s string) (string, string, bool) {
	if !strings.HasPrefix(s, "a/") {
		return "", "", false
	}
	i := strings.Index(s, " b/")
	if i < 0 {
		return "", "", false
	}
	return s[2:i], s[i+3:], true
}

// Input is a changed file to analyse.
type Input struct {
	Path    string
	Content []byte // at the head
	// New: added by the merge request (every line is new).
	New bool
	// Existed: the file exists at the merge base (a modified migration).
	Existed bool
	// Added lines (new side); ignored when New.
	Added []Range
}

// modified: a migration that already exists at the merge base.
func (in Input) modified() bool { return in.Existed && !in.New }

func (in Input) added(line int) bool {
	if in.New {
		return true
	}
	for _, r := range in.Added {
		if line >= r.From && line <= r.To {
			return true
		}
	}
	return false
}

// Analyze detects the migrations among the inputs and returns the risky
// operations on their added lines plus the modified pre-existing
// migrations. Operations on tables created in the same merge request are
// skipped, as are drops, renames and type changes of columns it adds.
// Usage checks are left to CheckUsages.
func Analyze(inputs []Input, cfg Config) *Result {
	d := cfg.dialect()
	res := &Result{Dialect: d}
	var files []fileOps
	for _, in := range inputs {
		fw, ok := cfg.Detect(in.Path, in.Content)
		if !ok {
			continue
		}
		if len(res.Files) >= cfg.maxFiles() {
			res.SkippedFiles++
			continue
		}
		res.Files = append(res.Files, File{Path: in.Path, Framework: fw, New: in.New, Modified: in.modified()})
		files = append(files, fileOps{in: in, fw: fw, ops: addedOps(in, fw, d)})
	}

	created := createdIn(files)
	seen := map[string]bool{}
	for _, f := range files {
		if f.in.modified() {
			line := 1
			if len(f.in.Added) > 0 {
				line = f.in.Added[0].From
			}
			res.Findings = append(res.Findings, Finding{Op: Op{Kind: ModifiedMigration, Path: f.in.Path, Line: line, Framework: f.fw}})
		}
		for _, op := range f.ops {
			rule := RuleFor(op.Kind)
			if rule == nil || (rule.SkipMySQL && d == MySQL) || created.covers(op) {
				continue
			}
			k := op.Path + "|" + strconv.Itoa(op.Line) + "|" + string(op.Kind) + "|" + lowerName(op.Table) + "|" + lowerName(op.Column)
			if seen[k] {
				continue
			}
			seen[k] = true
			res.Findings = append(res.Findings, Finding{Op: op})
		}
	}
	res.setLevels()
	return res
}

// fileOps is a detected migration and its operations on added lines.
type fileOps struct {
	in  Input
	fw  Framework
	ops []Op
}

// addedOps extracts the operations of a migration that lie on lines the
// merge request adds.
func addedOps(in Input, fw Framework, d Dialect) []Op {
	var kept []Op
	for _, op := range extract(fw, string(in.Content), d) {
		if op.Line <= 0 {
			op.Line = 1
		}
		if !in.added(op.Line) {
			continue
		}
		op.Path, op.Framework = in.Path, fw
		kept = append(kept, op)
	}
	return kept
}

// createdObjects are the tables and the columns ("table.column") the merge
// request itself creates.
type createdObjects struct {
	tables, columns map[string]bool
}

func createdIn(files []fileOps) createdObjects {
	c := createdObjects{tables: map[string]bool{}, columns: map[string]bool{}}
	for _, f := range files {
		for _, op := range f.ops {
			switch op.Kind {
			case createTable:
				c.tables[lowerName(op.Table)] = true
			case addColumn:
				c.columns[lowerName(op.Table)+"."+lowerName(op.Column)] = true
			}
		}
	}
	return c
}

// covers reports an operation that is harmless because its object is new:
// anything on a table created in the merge request, and dropping, renaming
// or retyping a column it adds.
func (c createdObjects) covers(op Op) bool {
	if op.Table != "" && c.tables[lowerName(op.Table)] {
		return true
	}
	if op.Column != "" && c.columns[lowerName(op.Table)+"."+lowerName(op.Column)] {
		switch op.Kind {
		case DropColumn, RenameColumn, AlterType:
			return true
		}
	}
	return false
}
