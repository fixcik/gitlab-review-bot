package compat

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// oasdiff
// ---------------------------------------------------------------------------

// oasdiffChange is one element of `oasdiff changelog --format json` (a bare
// JSON array of formatters.Change; level is checker.Level: 3=ERR, 2=WARN,
// 1=INFO). Sources carry file/line only for YAML specs. Field names verified
// against oasdiff v1.34 (formatters/changes.go, checker/location/source.go).
type oasdiffChange struct {
	ID             string          `json:"id"`
	Text           string          `json:"text"`
	Comment        string          `json:"comment"`
	Level          json.RawMessage `json:"level"`
	Operation      string          `json:"operation"`
	OperationID    string          `json:"operationId"`
	Path           string          `json:"path"`
	Section        string          `json:"section"`
	BaseSource     *oasdiffSource  `json:"baseSource"`
	RevisionSource *oasdiffSource  `json:"revisionSource"`
	Fingerprint    string          `json:"fingerprint"`
}

type oasdiffSource struct {
	File string `json:"file"`
	Line int    `json:"line"`
}

// ParseOasdiff parses oasdiff JSON output for spec (the repository path; the
// tool saw it as old/<spec> and new/<spec>). It returns ERR-level changes
// (breaking) and WARN-level ones separately.
func ParseOasdiff(out []byte, spec string) (errs, warns []Change, err error) {
	_, errs, warns, err = ParseOasdiffChangelog(out, spec, false)
	return errs, warns, err
}

// ParseOasdiffChangelog parses `oasdiff changelog --format json` output (the
// same array as `oasdiff breaking`, plus INFO-level items) for spec. It
// returns every item as an APIChange (in tool order) and the ERR / WARN
// items as Changes. converted means the NEW side was converted from Swagger
// 2.0 before the diff: its line numbers point into the converted file, so
// they are dropped and every item is located at spec.
func ParseOasdiffChangelog(out []byte, spec string, converted bool) (items []APIChange, errs, warns []Change, err error) {
	t := bytes.TrimSpace(out)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return nil, nil, nil, nil
	}
	var raw []oasdiffChange
	if err := json.Unmarshal(t, &raw); err != nil {
		var wrapped struct {
			Changes []oasdiffChange `json:"changes"`
		}
		if werr := json.Unmarshal(t, &wrapped); werr != nil {
			return nil, nil, nil, fmt.Errorf("oasdiff json: %w", err)
		}
		raw = wrapped.Changes
	}
	for _, it := range raw {
		lvl := oasdiffLevel(it.Level)
		c := Change{Source: SourceOasdiff, Path: spec, Rule: it.ID, Fingerprint: it.Fingerprint}
		switch {
		case converted:
			// Lines and files of the converted document mean nothing in
			// the repository.
		case it.RevisionSource != nil && it.RevisionSource.File != "":
			if p, side := scratchPath(it.RevisionSource.File); p != "" && side == "new" {
				c.Path, c.Line = p, it.RevisionSource.Line
			}
		case it.BaseSource != nil && it.BaseSource.File != "":
			if p, _ := scratchPath(it.BaseSource.File); p != "" {
				c.Path = p // a removal: the line is in the old file
			}
		}
		text := strings.TrimSpace(it.Text)
		if text == "" {
			text = it.ID
		}
		if cm := strings.TrimSpace(it.Comment); cm != "" {
			text += " (" + cm + ")"
		}
		if lvl < 1 {
			lvl = 1
		}
		items = append(items, APIChange{
			Level: lvl, Rule: it.ID, Operation: strings.ToUpper(strings.TrimSpace(it.Operation)),
			Endpoint: strings.TrimSpace(it.Path), Text: text, File: c.Path, Line: c.Line,
		})
		if lvl < 2 {
			continue
		}
		msg := text
		if it.Operation != "" || it.Path != "" {
			msg = strings.TrimSpace(it.Operation+" "+it.Path) + ": " + msg
		}
		c.Message = msg
		if lvl >= 3 {
			errs = append(errs, c)
		} else {
			warns = append(warns, c)
		}
	}
	return items, errs, warns, nil
}

// oasdiffLevel accepts the numeric level and, defensively, its names.
func oasdiffLevel(raw json.RawMessage) int {
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(s) {
		case "err", "error":
			return 3
		case "warn", "warning":
			return 2
		case "info":
			return 1
		}
	}
	return 0
}

// scratchPath maps a path the tools saw ("old/x/y.proto", "./new/a.yaml",
// or an absolute /work/.reviewbot-compat/new/...) back to the repository
// path and the side.
func scratchPath(p string) (repoPath, side string) {
	p = strings.ReplaceAll(p, "\\", "/")
	if i := strings.Index(p, ScratchDir+"/"); i >= 0 {
		p = p[i+len(ScratchDir)+1:]
	}
	p = strings.TrimPrefix(p, "./")
	for _, s := range sides {
		if strings.HasPrefix(p, s+"/") {
			return p[len(s)+1:], s
		}
	}
	return "", ""
}

// ---------------------------------------------------------------------------
// buf
// ---------------------------------------------------------------------------

// bufAnnotation is one line of `buf breaking --error-format json` (one JSON
// object per line; bufanalysis.externalFileAnnotation). Verified against
// buf v1.75 (private/bufpkg/bufanalysis/print.go).
type bufAnnotation struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	Type      string `json:"type"`
	Message   string `json:"message"`
}

// ParseBuf parses `buf breaking --error-format json` output (stdout and
// stderr together). Annotations of type COMPILE (or without a type) mean the
// files did not build: they are returned as compileErr, not as changes.
func ParseBuf(out string) (changes []Change, compileErr string, err error) {
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	jsonLines := 0
	var compile []string
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(l, "{") {
			continue
		}
		var a bufAnnotation
		if json.Unmarshal([]byte(l), &a) != nil {
			continue
		}
		jsonLines++
		if a.Type == "" || strings.EqualFold(a.Type, "COMPILE") {
			compile = append(compile, strings.TrimSpace(a.Path+": "+a.Message))
			continue
		}
		c := Change{Source: SourceBuf, Rule: a.Type, Message: strings.TrimSpace(a.Message)}
		if p, side := scratchPath(a.Path); p != "" {
			c.Path = p
			if side == "new" {
				c.Line = a.StartLine
			}
		} else {
			c.Path = strings.TrimPrefix(a.Path, "./")
		}
		changes = append(changes, c)
	}
	if len(compile) > 0 {
		return changes, shortText(strings.Join(compile, "; "), 300), nil
	}
	if jsonLines == 0 && strings.TrimSpace(out) != "" {
		return nil, "", errors.New("no buf annotations in output")
	}
	return changes, "", nil
}

// ---------------------------------------------------------------------------
// graphql-inspector
// ---------------------------------------------------------------------------

// graphql-inspector `diff` prints each change as "<symbol>  <message>" via its
// logger; without a TTY every line is prefixed with "[log] " (logger
// emit()). Breaking changes use log-symbols' error symbol: "✖" (or "×"
// where unicode is unsupported). Dangerous use "⚠"/"‼", safe "✔"/"√".
// Exit code 1 means breaking changes (failOnBreakingChanges). Verified
// against @graphql-inspector/cli 7.0 (packages/commands/diff/src/index.ts,
// packages/logger/src/index.ts).
var (
	ansiRe       = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	gqlLogPrefix = regexp.MustCompile(`^\[(?:log|error|warn|info|success)\]\s*`)
)

// ParseGraphQLInspector extracts breaking changes from graphql-inspector
// diff output.
func ParseGraphQLInspector(out, spec string) (breaking []Change, dangerous []string) {
	for _, raw := range strings.Split(stripANSI(out), "\n") {
		l := strings.TrimSpace(gqlLogPrefix.ReplaceAllString(strings.TrimSpace(raw), ""))
		switch {
		case strings.HasPrefix(l, "✖"), strings.HasPrefix(l, "×"):
			msg := strings.TrimSpace(strings.TrimLeft(l, "✖× "))
			if msg != "" {
				breaking = append(breaking, Change{Source: SourceGraphQL, Path: spec, Rule: "breaking", Message: msg})
			}
		case strings.HasPrefix(l, "⚠"), strings.HasPrefix(l, "‼"):
			if msg := strings.TrimSpace(strings.TrimLeft(l, "⚠‼ ")); msg != "" {
				dangerous = append(dangerous, msg)
			}
		}
	}
	return breaking, dangerous
}

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }
