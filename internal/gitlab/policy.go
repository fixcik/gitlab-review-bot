package gitlab

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// WritePolicy is the set of write (non-GET) endpoints a Client may call.
// It is a second line of defence on top of the bot's GitLab role: even if a
// bug or a prompt injection led the code towards pushing, merging, approving
// or changing settings, the request is refused before it leaves the process.
type WritePolicy uint8

const (
	// WriteMRComments: MR notes, discussions (reply, resolve) and draft notes.
	WriteMRComments WritePolicy = 1 << iota
	// WriteProjectHooks: create and edit project webhooks (hook sync only).
	WriteProjectHooks
	// WriteReviewers: change an MR's reviewer list, and nothing else about
	// the MR (the request body may only carry reviewer_ids).
	WriteReviewers
	// WriteResolve: resolve or unresolve MR discussions.
	WriteResolve
	// WriteAwards: add and remove award emoji (reactions) on an MR and on
	// its notes. Removing is limited to an award id; GitLab lets a user
	// remove only their own awards.
	WriteAwards
	// WriteLabels: add or remove an MR's labels, and nothing else about the
	// MR (the request body may only carry add_labels / remove_labels).
	WriteLabels
	// WriteCommitComments: add a comment to a commit (POST only; the CI
	// failure digest tells people about a red main branch after a direct
	// push, where there is no MR to write in).
	WriteCommitComments
	// WriteDescription: replace an MR's description, and nothing else about
	// the MR (the request body may only carry description; the bot's own
	// summary block between markers, review/descsum).
	WriteDescription
)

// ServiceWrites is the policy of the service-token client: hook sync,
// assigning the bot as reviewer, resolving threads, MR labels. No notes, no
// code.
const ServiceWrites = WriteProjectHooks | WriteReviewers | WriteResolve | WriteLabels | WriteDescription

// BotWrites is the policy of the bot's own client: MR comments, award emoji
// (reactions), MR labels (metadata; a Reporter gets 403 and the service
// client is used) and commit comments (red main after a direct push).
const BotWrites = WriteMRComments | WriteAwards | WriteLabels | WriteCommitComments | WriteDescription

// WithWrites replaces the default write policy (WriteMRComments).
func WithWrites(p WritePolicy) Option {
	return func(c *Client) error {
		c.writes = p
		return nil
	}
}

var (
	// Paths are relative to /api/v4 and use numeric project IDs.
	mrCommentWriteRe = regexp.MustCompile(`^/projects/\d+/merge_requests/\d+/(` +
		`notes|notes/\d+|` +
		`discussions/[0-9a-f]+|discussions/[0-9a-f]+/notes|` +
		`draft_notes|draft_notes/\d+|draft_notes/bulk_publish)$`)
	// awardWriteRe: award emoji of the MR or of one of its notes; POST
	// adds, DELETE (with an award id) removes.
	awardWriteRe = regexp.MustCompile(`^/projects/\d+/merge_requests/\d+/(notes/\d+/)?award_emoji(/\d+)?$`)
	hookWriteRe  = regexp.MustCompile(`^/projects/\d+/hooks(/\d+)?$`)
	// commitCommentRe: comments of one commit (POST adds one).
	commitCommentRe = regexp.MustCompile(`^/projects/\d+/repository/commits/[0-9a-f]{7,64}/comments$`)
	mrRe            = regexp.MustCompile(`^/projects/\d+/merge_requests/\d+$`)
	resolveRe       = regexp.MustCompile(`^/projects/\d+/merge_requests/\d+/discussions/[0-9a-f]+$`)
)

// checkWrite refuses write requests outside the client's policy.
func (c *Client) checkWrite(r request) error {
	if r.method == http.MethodGet || r.method == http.MethodHead {
		return nil
	}
	path := strings.TrimPrefix(r.u.Path, c.base.Path)
	if writeAllowed(c.writes, r.method, path) {
		return nil
	}
	if writeAllowedWithBody(c.writes, r.method, path, r.body) {
		return nil
	}
	return fmt.Errorf("gitlab: write %s %s is not allowed for this client: %w", r.method, path, core.ErrForbidden)
}

func writeAllowed(p WritePolicy, method, path string) bool {
	if p&WriteMRComments != 0 && mrCommentWriteRe.MatchString(path) {
		switch method {
		case http.MethodPost, http.MethodPut:
			return true
		case http.MethodDelete:
			// Only leftover draft notes are ever deleted.
			return strings.Contains(path, "/draft_notes/")
		}
	}
	if p&WriteAwards != 0 && awardWriteRe.MatchString(path) {
		switch method {
		case http.MethodPost:
			return strings.HasSuffix(path, "/award_emoji")
		case http.MethodDelete:
			return !strings.HasSuffix(path, "/award_emoji")
		}
	}
	if p&WriteProjectHooks != 0 && hookWriteRe.MatchString(path) {
		return method == http.MethodPost || method == http.MethodPut
	}
	if p&WriteCommitComments != 0 && commitCommentRe.MatchString(path) {
		return method == http.MethodPost
	}
	return false
}

// writeAllowedWithBody covers writes whose safety depends on the body: an MR
// update is allowed only when it changes nothing but the reviewers (or only
// the labels), and a discussion update only when it just (un)resolves it.
func writeAllowedWithBody(p WritePolicy, method, path string, body any) bool {
	if method != http.MethodPut {
		return false
	}
	switch body.(type) {
	case reviewersUpdate, *reviewersUpdate:
		return p&WriteReviewers != 0 && mrRe.MatchString(path)
	case resolveUpdate, *resolveUpdate:
		return p&WriteResolve != 0 && resolveRe.MatchString(path)
	case labelsUpdate, *labelsUpdate:
		return p&WriteLabels != 0 && mrRe.MatchString(path)
	case descriptionUpdate, *descriptionUpdate:
		return p&WriteDescription != 0 && mrRe.MatchString(path)
	}
	return false
}
