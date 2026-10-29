// Package mrthread is the model of the bot's threads in an MR: scanning the
// discussions for the summary note and the open finding, compat, lint, size
// and CI threads, and where a note sits. It is a leaf: it must not depend on
// internal/review or any of its feature packages.
package mrthread

import (
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
)

// OpenFinding is an unresolved bot finding thread.
type OpenFinding struct {
	DiscussionID string
	Path         string
	Line         int
	Title        string
	Body         string
	Blocking     bool // the thread was posted as merge-blocking
}

// OpenCompat is an unresolved bot breaking-change thread.
type OpenCompat struct {
	DiscussionID string
	Source       string
	Path         string
	Line         int
	Title        string
	Body         string
}

// Threads is what the reviewer extracts from the MR discussions.
type Threads struct {
	Open        map[string]OpenFinding
	OpenOrder   []string
	OpenCompat  map[string]OpenCompat
	CompatOrder []string
	// DismissedCompat are compat threads a human resolved (not the bot):
	// a deliberate override; the same change is not re-posted.
	DismissedCompat map[string]OpenCompat
	SummaryID       int64
	SummaryBody     string
	// OpenLintBlocking counts open merge-blocking lint threads; used for
	// the gate line when the lint step is off this run (lint.go tracks
	// them itself otherwise).
	OpenLintBlocking int
	// OpenLint counts all open lint threads (blocking or not; "@bot check"
	// reports them without re-running the tools, check.go).
	OpenLint int
	// FixedPendingBlocking counts merge-blocking bot threads (findings,
	// compat, lint) that the bot declared fixed earlier but could not
	// resolve (no service token): still unresolved in GitLab, so they still
	// block the merge and count in the gate line.
	FixedPendingBlocking int
	// OpenSizeBlocking counts the open blocking size threads
	// (review.big_mr.block, bigmr.go) and OpenOtherBlocking every other
	// open bot thread with the blocking marker that is not a finding,
	// compat or lint thread. Both count in the merge gate: a recount
	// (linters.GateCount: "@bot check", the CI lint refresh) must agree with the
	// review, which decides on the size thread itself (tooBigThread,
	// closeTooBigThread) and adds OpenOtherBlocking.
	OpenSizeBlocking  int
	OpenOtherBlocking int
	// CI is the bot's active CI explanation thread (ci.go); nil = none.
	CI *CIThread
}

// Scan extracts the bot's threads and summary note from the MR
// discussions; isBot tells the bot's notes. knownSummaryID is the summary
// note id the store remembers (0 = none): it wins over the newest note with
// the summary marker.
func Scan(ds []core.Discussion, knownSummaryID int64, isBot func(core.Note) bool) *Threads {
	t := &Threads{Open: map[string]OpenFinding{}, OpenCompat: map[string]OpenCompat{}, DismissedCompat: map[string]OpenCompat{}}
	var byMarker *core.Note
	knownFound := false
	for _, d := range ds {
		for _, n := range d.Notes {
			if n.System || !isBot(n) {
				continue
			}
			if knownSummaryID != 0 && n.ID == knownSummaryID {
				t.SummaryID, t.SummaryBody = n.ID, n.Body
				knownFound = true
			} else if marker.IsSummaryNote(n.Body) && (byMarker == nil || n.ID > byMarker.ID) {
				n := n
				byMarker = &n
			}
		}
		if len(d.Notes) == 0 || d.Notes[0].System || !isBot(d.Notes[0]) {
			continue // not a bot thread
		}
		first := d.Notes[0]
		// A thread the bot already declared fixed (but could not resolve,
		// e.g. as a Reporter) is no longer "open" for the bot: it waits for a
		// human to close it and must not get the same reply again.
		fixedByBot := FixedByBot(d, isBot)
		if fixedByBot && !d.Resolved && marker.HasBlocking(first.Body) {
			t.FixedPendingBlocking++
		}
		if th := CIThreadOf(d, isBot); th.Active() {
			t.CI = th // the newest active one wins
		}
		switch {
		case d.Resolved && !fixedByBot:
			if cid, src, ok := marker.ParseCompat(first.Body); ok {
				t.DismissedCompat[cid] = OpenCompat{DiscussionID: d.ID, Source: src, Body: first.Body, Title: ParseCompatNote(first.Body)}
			}
		case !d.Resolved && !fixedByBot:
			t.addOpen(d.ID, first)
		}
	}
	if !knownFound && byMarker != nil {
		t.SummaryID, t.SummaryBody = byMarker.ID, byMarker.Body
	}
	return t
}

// addOpen counts the open bot thread whose first note is n.
func (t *Threads) addOpen(discussionID string, n core.Note) {
	if cid, src, ok := marker.ParseCompat(n.Body); ok {
		if _, dup := t.OpenCompat[cid]; !dup {
			oc := OpenCompat{DiscussionID: discussionID, Source: src, Body: n.Body, Title: ParseCompatNote(n.Body)}
			oc.Path, oc.Line = NotePathLine(n)
			t.OpenCompat[cid] = oc
			t.CompatOrder = append(t.CompatOrder, cid)
		}
		return
	}
	if _, _, ok := marker.ParseLint(n.Body); ok {
		t.OpenLint++
		if marker.HasBlocking(n.Body) {
			t.OpenLintBlocking++
		}
		return
	}
	id, ok := marker.ParseFinding(n.Body)
	if !ok {
		if marker.HasBlocking(n.Body) && !marker.IsSummaryNote(n.Body) {
			if strings.Contains(marker.Header(n.Body), marker.TooBigThread) {
				t.OpenSizeBlocking++
			} else {
				t.OpenOtherBlocking++
			}
		}
		return
	}
	if _, dup := t.Open[id]; dup {
		return
	}
	f := OpenFinding{DiscussionID: discussionID, Blocking: marker.HasBlocking(n.Body)}
	f.Path, f.Line = NotePathLine(n)
	f.Title, f.Body = finding.ParseNote(n.Body)
	t.Open[id] = f
	t.OpenOrder = append(t.OpenOrder, id)
}

// FixedByBot reports whether the bot declared the thread fixed; isBot tells
// the bot's notes.
func FixedByBot(d core.Discussion, isBot func(core.Note) bool) bool {
	for _, n := range d.Notes {
		if !n.System && isBot(n) && marker.HasFixed(n.Body) {
			return true
		}
	}
	return false
}
