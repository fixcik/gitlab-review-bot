package lint

import (
	"context"
	"regexp"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/tasklink"
)

// KeyState is the result of looking a Jira key up (verify_keys).
type KeyState int

const (
	// KeyUnchecked: not looked up (lookup cap): treated as fine.
	KeyUnchecked KeyState = iota
	// KeyExists: Jira knows the key.
	KeyExists
	// KeyMissing: Jira does not know the key or the bot has no access.
	KeyMissing
)

// KeyChecker looks a Jira key up. An error means it could not be checked
// (the key is then treated as fine and the run is partial).
type KeyChecker func(ctx context.Context, key string) (KeyState, error)

// keyedTodo is a marker that references only Jira keys: it becomes a
// task-not-found result when Jira knows none of them (verifyTodoKeys).
type keyedTodo struct {
	f    Finding
	keys []string
}

// verifyTodoKeys looks the keys up (each once) and returns a
// task-not-found result for every marker none of whose keys Jira knows,
// and the number of keys that could not be checked (treated as fine).
func verifyTodoKeys(ctx context.Context, check KeyChecker, todos []keyedTodo) (out []Finding, failed int) {
	known := map[string]KeyState{}
	state := func(key string) KeyState {
		s, seen := known[key]
		if !seen {
			var err error
			if s, err = check(ctx, key); err != nil {
				failed++
				s = KeyUnchecked
			}
			known[key] = s
		}
		return s
	}
	for _, k := range todos {
		missing := ""
		fine := false
		for _, key := range k.keys {
			if state(key) != KeyMissing {
				fine = true
				break
			}
			if missing == "" {
				missing = key
			}
		}
		if fine || missing == "" {
			continue
		}
		f := k.f
		f.Todo.Key = missing
		f.Message = "задача " + missing + " не найдена — «" + f.Todo.Comment + "»"
		out = append(out, f)
	}
	return out, failed
}

func isWordByte(b byte) bool {
	return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// findMarker returns the first keyword in the comments of a line (whole
// word, case-sensitive; not "x.TODO" and not a call "TODO()") and the
// comment text from it on.
func findMarker(comments []string, kws []string) (kw, text string, ok bool) {
	for _, c := range comments {
		best, bestKw := -1, ""
		for _, k := range kws {
			for from := 0; from < len(c); {
				i := strings.Index(c[from:], k)
				if i < 0 {
					break
				}
				i += from
				from = i + 1
				end := i + len(k)
				if i > 0 && (isWordByte(c[i-1]) || c[i-1] == '.') {
					continue
				}
				if end < len(c) && isWordByte(c[end]) {
					continue
				}
				if strings.HasPrefix(c[end:], "()") {
					continue
				}
				if best < 0 || i < best {
					best, bestKw = i, k
				}
				break
			}
		}
		if best >= 0 {
			return bestKw, shortText(c[best:], todoMaxComment), true
		}
	}
	return "", "", false
}

// taskRef is what findTaskRef found in a comment.
type taskRef struct {
	keys  []string // Jira keys written as text (verifiable)
	other bool     // a GitLab issue reference or an issue / Jira URL
}

func (r taskRef) any() bool      { return r.other || len(r.keys) > 0 }
func (r taskRef) onlyKeys() bool { return !r.other && len(r.keys) > 0 }

var (
	todoURLRe = regexp.MustCompile(`https?://[^\s<>"'(){}\[\]]+`)
	// issueURLRe: a Jira browse page or an issue / work item of GitLab,
	// GitHub, Gitea, YouTrack, Redmine.
	issueURLRe = regexp.MustCompile(`/browse/[A-Z][A-Z0-9_]*-[0-9]+|[?&](?:selectedIssue|issueKey)=[A-Z][A-Z0-9_]*-[0-9]+|/(?:-/)?(?:issues|work_items)/[0-9]+|/issue/[A-Z][A-Z0-9_]*-[0-9]+`)
	// gitlabRefRe: "#123", "project#123", "group/sub/project#123".
	gitlabRefRe = regexp.MustCompile(`(?:^|[\s(\[{,;:'"])(?:[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*)?#[0-9]+\b`)
)

// notTaskPrefixes are key-like prefixes that are not tasks ("UTF-8",
// "SHA-256", "RFC-7231"); used when no Jira project keys are configured.
var notTaskPrefixes = map[string]bool{
	"UTF": true, "ISO": true, "SHA": true, "MD": true, "AES": true, "RSA": true, "RFC": true, "CVE": true,
	"CWE": true, "PEP": true, "ECMA": true, "ES": true, "HTTP": true, "TLS": true, "SSL": true, "IPV": true,
	"X": true, "UTC": true, "GMT": true, "ISBN": true, "WCAG": true, "PSR": true, "JSR": true, "EIP": true,
	"ERC": true, "BIP": true, "KB": true, "MB": true, "GB": true, "TB": true, "CP": true, "WIN": true,
}

// findTaskRef looks for a task reference in comment text: a Jira key of
// the configured projects (any ABC-123 when none are configured), a
// GitLab issue reference or an issue URL.
func findTaskRef(text string, projectKeys, keywords []string) taskRef {
	var r taskRef
	for _, u := range todoURLRe.FindAllString(text, -1) {
		if issueURLRe.MatchString(u) {
			r.other = true
		}
	}
	plain := todoURLRe.ReplaceAllString(text, " ")
	if gitlabRefRe.MatchString(plain) {
		r.other = true
	}
	kw := map[string]bool{}
	for _, k := range keywords {
		kw[k] = true
	}
	for _, k := range tasklink.ExtractKeys(plain, projectKeys) {
		pre := tasklink.KeyPrefix(k)
		if kw[pre] || (len(projectKeys) == 0 && notTaskPrefixes[pre]) {
			continue
		}
		r.keys = append(r.keys, k)
	}
	return r
}
