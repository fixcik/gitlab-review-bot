package main

import (
	"context"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/doctor"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
)

// sandboxImageRetry: how often a failed pull of the sandbox image is tried
// again.
const sandboxImageRetry = time.Hour

// sandboxImageDeps is what ensureSandboxImage uses (tests replace them).
type sandboxImageDeps struct {
	image  string
	exec   doctor.Exec // `docker image inspect` (nil = os/exec)
	pull   func(ctx context.Context, image string) error
	probs  *problems.Service // nil = problems off
	logger *slog.Logger
	// retry returns the channel of the next attempt (nil = sandboxImageRetry).
	retry func() <-chan time.Time
}

func (d sandboxImageDeps) withDefaults() sandboxImageDeps {
	if d.exec == nil {
		d.exec = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		}
	}
	if d.pull == nil {
		d.pull = dockerPullOutput
	}
	if d.retry == nil {
		d.retry = func() <-chan time.Time { return time.After(sandboxImageRetry) }
	}
	if d.logger == nil {
		d.logger = slog.New(slog.DiscardHandler)
	}
	return d
}

// pullError carries the output of a failed `docker pull` (the reason:
// unauthorized, not found, no network).
type pullError struct{ out string }

func (e pullError) Error() string { return e.out }

// dockerPullOutput pulls quietly and returns docker's message on failure.
func dockerPullOutput(ctx context.Context, image string) error {
	out, err := exec.CommandContext(ctx, "docker", "pull", "--quiet", image).CombinedOutput()
	if err != nil {
		msg := strings.Join(strings.Fields(string(out)), " ")
		if msg == "" {
			msg = err.Error()
		}
		return pullError{msg}
	}
	return nil
}

// ensureSandboxImage runs in the background at the start of serve
// (sandbox.mode docker): when the sandbox image is not on the host it is
// pulled, so the first review does not wait for it (docker run would pull
// it too) and a registry that refuses is reported before a review fails.
// A failed pull is logged and recorded as the problem sandbox_image_missing
// and tried again every sandboxImageRetry until it succeeds (then the
// problem is resolved) or ctx ends.
func ensureSandboxImage(ctx context.Context, d sandboxImageDeps) {
	d = d.withDefaults()
	if _, err := d.exec(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", d.image); err == nil {
		return
	}
	for {
		d.logger.Info("sandbox image missing: pulling", "image", d.image)
		pctx, cancel := context.WithTimeout(ctx, sandboxPullTimeout)
		err := d.pull(pctx, d.image)
		cancel()
		if err == nil {
			d.logger.Info("sandbox image pulled", "image", d.image)
			d.probs.ResolveOutside(context.Background(), problems.SandboxImageMissing, "", d.image)
			return
		}
		if ctx.Err() != nil {
			return
		}
		d.logger.Warn("sandbox image: pull failed, retrying in an hour", "image", d.image, "err", err)
		d.probs.Record(context.Background(), problems.Problem{Kind: problems.SandboxImageMissing, Source: problems.SourceBot,
			Subject: d.image, Detail: "docker pull: " + err.Error()})
		select {
		case <-ctx.Done():
			return
		case <-d.retry():
		}
	}
}
