package compat

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// openAPINeighbors caps how many sibling files (for relative $refs) are
// materialized next to an OpenAPI spec.
const openAPINeighbors = 500

// convertedSuffix names the OpenAPI 3.0 copy of a Swagger 2.0 spec, written
// by oas2to3 next to the materialized spec.
const convertedSuffix = ".reviewbot-oas3.yaml"

// convertedVersion is the OpenAPI version oas2to3 produces (kin-openapi
// openapi2conv).
const convertedVersion = "3.0.3"

func (c *Checker) checkOpenAPI(ctx context.Context, m *materializer, spec, lang string, res *Result) {
	for _, side := range sides {
		if err := m.writeOpenAPI(ctx, side, spec); err != nil {
			c.failed(res, SourceOasdiff, spec, "не удалось подготовить файлы: "+err.Error(), nil)
			return
		}
	}
	allowRefs, why, ok := oasdiffRefsFlag(m.dir, spec)
	if !ok {
		c.failed(res, SourceOasdiff, spec, why, nil)
		return
	}
	old, ok := c.openAPISide(ctx, m, "old", spec, res)
	if !ok {
		return
	}
	cur, ok := c.openAPISide(ctx, m, "new", spec, res)
	if !ok {
		return
	}

	argv := []string{"oasdiff", "changelog", "--fail-on", "ERR", "--format", "json", "--lang", lang, allowRefs}
	if minor(old.version) != "" && minor(cur.version) != "" && minor(old.version) != minor(cur.version) {
		// 3.0 vs 3.1: canonicalize both sides to the latest 3.x first.
		argv = append(argv, "--auto-upgrade")
	}
	argv = append(argv, old.arg, cur.arg)
	out, ok := c.run(ctx, m.dir, argv, SourceOasdiff, spec, res)
	if !ok {
		return
	}
	switch {
	case out.ExitCode == 0, out.ExitCode == 1:
	case strings.Contains(out.Stderr, "disallowed external reference"):
		c.failed(res, SourceOasdiff, spec, "спецификация ссылается на внешний $ref, а загрузка внешних ссылок отключена", out)
		return
	default:
		c.failed(res, SourceOasdiff, spec, oasdiffExitReason(out.ExitCode), out)
		return
	}
	items, errs, warns, err := ParseOasdiffChangelog([]byte(out.Stdout), spec, cur.converted)
	if err != nil {
		c.failed(res, SourceOasdiff, spec, "непонятный вывод oasdiff", out)
		return
	}
	if out.ExitCode == 1 && len(errs) == 0 {
		c.failed(res, SourceOasdiff, spec, "oasdiff завершился с ошибкой", out)
		return
	}
	if old.converted && !cur.converted {
		// A removal is located in the converted old file: no line anyway,
		// but its file name must not leak into the repository path.
		for i := range errs {
			errs[i].Path = fixConvertedPath(errs[i].Path, spec)
		}
		for i := range warns {
			warns[i].Path = fixConvertedPath(warns[i].Path, spec)
		}
		for i := range items {
			items[i].File = fixConvertedPath(items[i].File, spec)
		}
	}
	res.Changes = append(res.Changes, errs...)
	res.Warnings = append(res.Warnings, warns...)
	res.API = append(res.API, APIChanges{Spec: spec, Converted: old.converted || cur.converted, Items: items})
}

// writeOpenAPI copies spec of side into the scratch dir, and the sibling
// YAML/JSON files below its directory so relative $refs resolve. The
// siblings are best effort: skipped when there are too many.
func (m *materializer) writeOpenAPI(ctx context.Context, side, spec string) error {
	if err := m.write(ctx, side, spec); err != nil {
		return err
	}
	all, err := m.files(ctx, side)
	if err != nil {
		return nil
	}
	dir := path.Dir(spec)
	var sib []string
	for _, f := range all {
		if f == spec || !(dir == "." || strings.HasPrefix(f, dir+"/")) {
			continue
		}
		switch strings.ToLower(path.Ext(f)) {
		case ".yaml", ".yml", ".json":
			sib = append(sib, f)
		}
	}
	if len(sib) > openAPINeighbors {
		return nil
	}
	for _, f := range sib {
		if m.write(ctx, side, f) != nil {
			break
		}
	}
	return nil
}

// oasdiffRefsFlag picks --allow-external-refs from the external $refs of
// both sides (refs.go). ok=false: a side has a refused $ref (why).
func oasdiffRefsFlag(dir, spec string) (flag, why string, ok bool) {
	flag = "--allow-external-refs=false"
	for _, side := range sides {
		switch policy, why := scanRefs(filepath.Join(dir, side), spec); policy {
		case refsRefused:
			return "", why, false
		case refsLocal:
			flag = "--allow-external-refs=true"
		}
	}
	return flag, "", true
}

// oasSide is one side of an oasdiff run.
type oasSide struct {
	arg       string // the spec file oasdiff reads (scratch-relative)
	version   string // OpenAPI version ("" when unknown)
	converted bool   // converted from Swagger 2.0
}

// openAPISide prepares one side for oasdiff. oasdiff only understands
// OpenAPI 3.x: on Swagger 2.0 it silently reports no changes at all. Such a
// side is converted to OpenAPI 3.0 first (oas2to3, in the sandbox); a
// 2.0 -> 3.x migration converts only the old side. ok=false means an Issue
// was recorded.
func (c *Checker) openAPISide(ctx context.Context, m *materializer, side, spec string, res *Result) (oasSide, bool) {
	s := oasSide{arg: side + "/" + spec}
	b, err := m.co.ShowFile(ctx, m.rev(side), spec)
	if err != nil {
		c.failed(res, SourceOasdiff, spec, "не удалось прочитать спецификацию: "+err.Error(), nil)
		return s, false
	}
	key, ver := SpecVersion(b)
	if key != "swagger" {
		s.version = ver
		return s, true
	}
	if !strings.HasPrefix(ver, "2") {
		c.failed(res, SourceOasdiff, spec, fmt.Sprintf("неподдерживаемая версия swagger: %q", shortText(ver, 20)), nil)
		return s, false
	}
	out := s.arg + convertedSuffix
	if !c.convertSwagger2(ctx, m.dir, s.arg, out, spec, res) {
		return s, false
	}
	return oasSide{arg: out, version: convertedVersion, converted: true}, true
}

// convertSwagger2 runs `oas2to3 in out` in the scratch dir. ok=false means an
// Issue was recorded: the spec must not be diffed (never "no changes").
func (c *Checker) convertSwagger2(ctx context.Context, dir, in, out, spec string, res *Result) bool {
	argv := []string{"oas2to3", in, out}
	r, ok := c.run(ctx, dir, argv, SourceOasdiff, spec, res)
	if !ok {
		if n := len(res.Issues); n > 0 && res.Issues[n-1].Kind == IssueUnavailable {
			res.Issues[n-1].Reason = "oas2to3 не установлен в sandbox-образе — спецификацию Swagger 2.0 нельзя сравнить (пересоберите образ из Dockerfile.sandbox)"
		}
		return false
	}
	switch r.ExitCode {
	case 0:
		return true
	case 3:
		c.failed(res, SourceOasdiff, spec, "файл не распознан как Swagger 2.0", r)
	default:
		c.failed(res, SourceOasdiff, spec, "не удалось преобразовать Swagger 2.0 в OpenAPI 3.0", r)
	}
	return false
}

// fixConvertedPath maps the converted copy of spec back to spec.
func fixConvertedPath(p, spec string) string {
	if p == spec+convertedSuffix {
		return spec
	}
	return p
}

// minor returns "3.0" for "3.0.3", "" for anything that is not 3.x.
func minor(v string) string {
	parts := strings.SplitN(strings.TrimSpace(v), ".", 3)
	if len(parts) < 2 || parts[0] != "3" || parts[1] == "" {
		return ""
	}
	return parts[0] + "." + parts[1]
}

// oasdiffLang maps the repository language to an oasdiff --lang value.
func oasdiffLang(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "", "ru", "rus", "russian", "русский":
		return "ru"
	default:
		return "en"
	}
}

func oasdiffExitReason(code int) string {
	switch code {
	case 101:
		return "неверные флаги oasdiff"
	case 102:
		return "не удалось загрузить спецификацию"
	case 123:
		return "спецификация ссылается на внешний $ref"
	default:
		return fmt.Sprintf("oasdiff завершился с кодом %d", code)
	}
}
