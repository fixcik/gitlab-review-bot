package review

// What the model of a review may declare fixed (flow step 12): only a
// finding in a file of the reviewed range — the files Claude was shown,
// old paths of renamed files included — and, on the light route, never a
// blocking one. A "resolved" entry outside that scope is ignored (logged):
// the run did not see a change that could have fixed it, so closing it
// would rest on the model's word alone (or on text injected into the
// diff). The thread stays open; a person or "@bot check" closes it.

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

// generalThreadFileRe finds the file of a bot thread posted outside the
// diff: "_Файл `path`, строка N (вне диффа)._" (finding.NoteBodyOpts,
// compatcheck noteBody). The bot writes it before the model's text, so the
// first match is the bot's.
var generalThreadFileRe = regexp.MustCompile("(?m)^_Файл `([^`]+)`")

// reviewedFiles is the set of files of the reviewed range that Claude saw:
// the changed files minus those the diff cap left out, plus the old paths of
// renamed or copied files.
func (rv *reviewRun) reviewedFiles() map[string]bool {
	set := map[string]bool{}
	for _, f := range rv.files {
		if !slices.Contains(rv.capped.Omitted, f) {
			set[f] = true
		}
	}
	for _, f := range oldPaths(rv.capped.Diff) {
		set[f] = true
	}
	return set
}

// resolvable returns why the model may not close the thread of a bot result
// at path ("" = it may). body is the thread's text: a thread outside the
// diff has no position, its file is named in the text.
func (rv *reviewRun) resolvable(files map[string]bool, path, body string, blocking bool) string {
	if blocking && rv.rt.light() {
		return "blocking finding on the light route"
	}
	if path == "" {
		if m := generalThreadFileRe.FindStringSubmatch(body); m != nil {
			path = m[1]
		}
	}
	switch {
	case path == "":
		return "no file"
	case !files[path]:
		return "file not in the reviewed range"
	}
	return ""
}

// compatResolvable is the review output with compatibility.resolved
// limited to the open breaking-change threads the model may close
// (resolvable; they always block).
func (rv *reviewRun) compatResolvable(files map[string]bool) *output.ReviewOutput {
	c := rv.out.Compatibility
	if c == nil || len(c.Resolved) == 0 {
		return rv.out
	}
	cc := *c
	cc.Resolved = nil
	for _, x := range c.Resolved {
		oc, ok := rv.threads.OpenCompat[x.ID]
		if !ok {
			continue
		}
		if why := rv.resolvable(files, oc.Path, oc.Body, true); why != "" {
			rv.log.Info("review: resolved breaking change ignored", "id", x.ID, "path", oc.Path, "reason", why)
			continue
		}
		cc.Resolved = append(cc.Resolved, x)
	}
	out := *rv.out
	out.Compatibility = &cc
	return &out
}

// oldPaths returns the pre-image paths of a `git diff`: the "rename from" /
// "copy from" headers and the "--- a/…" lines (for a modified file the
// same as its new path; /dev/null skipped).
func oldPaths(diff string) []string {
	var out []string
	inHunk := false
	for _, l := range strings.Split(diff, "\n") {
		var p string
		switch {
		case strings.HasPrefix(l, "diff --git "):
			inHunk = false
			continue
		case inHunk:
			continue
		case strings.HasPrefix(l, "@@"):
			inHunk = true
			continue
		case strings.HasPrefix(l, "rename from "):
			p = diffPath(l[len("rename from "):], "")
		case strings.HasPrefix(l, "copy from "):
			p = diffPath(l[len("copy from "):], "")
		case strings.HasPrefix(l, "--- "):
			p = diffPath(l[len("--- "):], "a/")
		default:
			continue
		}
		if p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// diffPath decodes a path of a diff header: trailing tab cut, git's C-style
// quoting undone, the side prefix dropped ("" for /dev/null).
func diffPath(s, prefix string) string {
	s = strings.TrimRight(s, "\r")
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		s = s[:i]
	}
	if strings.HasPrefix(s, `"`) {
		if u, err := strconv.Unquote(s); err == nil {
			s = u
		}
	}
	if s == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(s, prefix)
}
