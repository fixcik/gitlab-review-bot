package compat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

var bufConfigNames = map[string]bool{"buf.yaml": true, "buf.work.yaml": true, "buf.lock": true}

// checkProto runs `buf breaking new/<root> --against old/<root>` for every
// buf module root that contains a changed .proto file. A root is the
// directory of a buf.yaml / buf.work.yaml (outermost wins); without any buf
// config the repository root is one default module. Both sides get every
// .proto file and buf config under the root, so imports resolve, and the
// input directory is the scratch copy (never the real checkout), so buf
// cannot pick up unrelated files.
func (c *Checker) checkProto(ctx context.Context, m *materializer, changed []string, res *Result) {
	files := map[string][]string{}
	for _, side := range []string{"new", "old"} {
		l, err := m.files(ctx, side)
		if err != nil {
			c.failed(res, SourceBuf, "", "не удалось получить список файлов: "+err.Error(), nil)
			return
		}
		files[side] = l
	}
	roots := bufRoots(files["new"])
	byRoot := map[string][]string{}
	for _, p := range changed {
		r, ok := rootOf(p, roots)
		if !ok {
			res.Issues = append(res.Issues, Issue{Source: SourceBuf, Path: p, Kind: IssueSkipped,
				Reason: "файл вне модулей buf (buf.yaml) — не проверен buf"})
			continue
		}
		byRoot[r] = append(byRoot[r], p)
	}
	keys := make([]string, 0, len(byRoot))
	for r := range byRoot {
		keys = append(keys, r)
	}
	sort.Strings(keys)
	for _, root := range keys {
		subject := root
		if root == "." {
			subject = strings.Join(byRoot[root], ", ")
		}
		if err := m.writeModule(ctx, root, files); err != nil {
			reason := "не удалось подготовить файлы: " + err.Error()
			if errors.Is(err, errTooMany) {
				reason = "слишком много proto-файлов для проверки"
			}
			c.failed(res, SourceBuf, subject, reason, nil)
			continue
		}
		c.bufBreaking(ctx, m, root, subject, res)
	}
}

// writeModule copies every .proto file and buf config under root of both
// sides (files: the file lists by side).
func (m *materializer) writeModule(ctx context.Context, root string, files map[string][]string) error {
	for _, side := range sides {
		for _, f := range files[side] {
			if !under(f, root) || !(strings.HasSuffix(f, ".proto") || bufConfigNames[path.Base(f)]) {
				continue
			}
			if err := m.write(ctx, side, f); err != nil {
				return err
			}
		}
	}
	return nil
}

// bufBreaking runs buf on one materialized module root.
func (c *Checker) bufBreaking(ctx context.Context, m *materializer, root, subject string, res *Result) {
	oldIn, newIn := path.Join("old", root), path.Join("new", root)
	if !hasProto(filepath.Join(m.dir, filepath.FromSlash(oldIn))) {
		return // the module is new: nothing to break
	}
	argv := []string{"buf", "breaking", newIn, "--against", oldIn, "--error-format", "json"}
	out, ok := c.run(ctx, m.dir, argv, SourceBuf, subject, res)
	if !ok || out.ExitCode == 0 {
		return
	}
	changes, compileErr, perr := ParseBuf(out.Stdout + "\n" + out.Stderr)
	switch {
	case out.ExitCode != 100:
		c.failed(res, SourceBuf, subject, fmt.Sprintf("buf завершился с кодом %d", out.ExitCode), out)
	case perr != nil || compileErr != "":
		why := "buf не смог собрать proto-файлы"
		if compileErr != "" {
			why += ": " + compileErr
			out = nil
		}
		c.failed(res, SourceBuf, subject, why, out)
	case len(changes) == 0:
		c.failed(res, SourceBuf, subject, "buf завершился с ошибкой", out)
	default:
		res.Changes = append(res.Changes, changes...)
	}
}

// bufRoots returns the outermost directories holding a buf.yaml or
// buf.work.yaml, or ["."] when there is none.
func bufRoots(files []string) []string {
	var dirs []string
	for _, f := range files {
		if b := path.Base(f); b == "buf.yaml" || b == "buf.work.yaml" {
			dirs = append(dirs, path.Dir(f))
		}
	}
	if len(dirs) == 0 {
		return []string{"."}
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) < len(dirs[j]) })
	var roots []string
	for _, d := range dirs {
		nested := false
		for _, r := range roots {
			if under(d, r) {
				nested = true
				break
			}
		}
		if !nested {
			roots = append(roots, d)
		}
	}
	return roots
}

func rootOf(p string, roots []string) (string, bool) {
	best, ok := "", false
	for _, r := range roots {
		if under(p, r) && (!ok || len(r) > len(best)) {
			best, ok = r, true
		}
	}
	return best, ok
}

// under reports whether p is r or inside directory r ("." contains all).
func under(p, r string) bool {
	return r == "." || p == r || strings.HasPrefix(p, r+"/")
}

func hasProto(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || found {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(p, ".proto") {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}
