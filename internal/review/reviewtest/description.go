package reviewtest

import (
	"context"
	"sync"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// FakeDescription is an MR description client (review.Deps.Description /
// ServiceDescription) over the harness's fake MR: GetMR reads GL.MR,
// UpdateMRDescription records the text and stores it in GL.MR, or fails
// with Err.
type FakeDescription struct {
	GL  *FakeGitLab
	Err error
	// Fresh, when set, changes the MR read right before the write (the MR
	// turned draft or was closed while the review ran).
	Fresh func(*core.MergeRequest)

	mu   sync.Mutex
	Puts []string
}

func (d *FakeDescription) GetMR(ctx context.Context, p, i int64) (*core.MergeRequest, error) {
	mr, err := d.GL.GetMR(ctx, p, i)
	if err == nil && d.Fresh != nil {
		d.Fresh(mr)
	}
	return mr, err
}

func (d *FakeDescription) UpdateMRDescription(_ context.Context, _, _ int64, desc string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Puts = append(d.Puts, desc)
	if d.Err != nil {
		return d.Err
	}
	d.GL.Mu.Lock()
	d.GL.MR.Description = desc
	d.GL.Mu.Unlock()
	return nil
}

// WithDescription turns review.description_summary on with fake bot and
// service clients (svcErr nil = no service client).
func WithDescription(h *Harness, botErr error) (bot *FakeDescription) {
	h.Cfg.Review.DescriptionSummary = true
	bot = &FakeDescription{GL: h.GL, Err: botErr}
	h.Deps.Description = bot
	return bot
}
