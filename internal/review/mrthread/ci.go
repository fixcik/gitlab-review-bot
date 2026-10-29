package mrthread

import (
	"strconv"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
)

// CIThread is the bot's explanation thread of an MR.
type CIThread struct {
	DiscussionID string
	NoteID       int64
	Body         string
	Resolved     bool
	Pipeline     int64  // the failed pipeline the thread is about
	FP           string // its fingerprint
	Explained    int64  // the pipeline whose explanation it shows (0 = none)
	Green        bool
}

// Active: open for the bot (not resolved, not turned green).
func (t *CIThread) Active() bool { return t != nil && !t.Resolved && !t.Green }

// About: the thread is about this failure (pipeline, fingerprint).
func (t *CIThread) About(pipeline int64, fp string) bool {
	return t != nil && t.Pipeline == pipeline && t.FP == fp
}

// CIThreadOf parses the first note of a discussion (nil when it is not the
// bot's CI thread); isBot tells the bot's notes.
func CIThreadOf(d core.Discussion, isBot func(core.Note) bool) *CIThread {
	if len(d.Notes) == 0 {
		return nil
	}
	n := d.Notes[0]
	if n.System || !isBot(n) {
		return nil
	}
	m := marker.CIThreadRe.FindStringSubmatch(marker.Header(n.Body))
	if m == nil {
		return nil
	}
	p, _ := strconv.ParseInt(m[1], 10, 64)
	e, _ := strconv.ParseInt(m[3], 10, 64)
	return &CIThread{DiscussionID: d.ID, NoteID: n.ID, Body: n.Body, Resolved: d.Resolved, Pipeline: p, FP: m[2],
		Explained: e, Green: m[4] == "green"}
}
