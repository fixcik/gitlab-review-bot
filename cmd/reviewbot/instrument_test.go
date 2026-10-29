package main

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/diag"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/telemetry"
)

func TestInstrument(t *testing.T) {
	cfg := config.Defaults()
	d := instrument(instruments{}, &cfg, review.Deps{})
	if d.Compat != nil || d.Lint != nil || d.Limiter != nil || d.Verify != nil || d.CI != nil || d.GitLab != nil ||
		d.Labels != nil || d.ServiceLabels != nil || d.Moderator != nil {
		t.Fatal("no sinks and no collaborators must not change deps")
	}
	d = instrument(instruments{tm: telemetry.New("x")}, &cfg, review.Deps{})
	if d.Compat == nil || d.Lint == nil {
		t.Fatal("checkers not wrapped")
	}
	if d.Verify == nil {
		t.Fatal("verification metrics not wired")
	}
	if d.CI == nil {
		t.Fatal("CI metrics not wired")
	}
	if d.ServiceLabels != nil || d.Moderator != nil {
		t.Fatal("nil optional clients must stay nil")
	}

	// Diagnostics wrap the label clients too (the service one only when
	// set), and the diag GitLab wrapper is outside the queue's marker.
	gl, err := gitlab.New("https://gitlab.example.com", "x")
	if err != nil {
		t.Fatal(err)
	}
	in := instruments{hooks: diag.Hooks{Trace: true}}
	d = instrument(in, &cfg, review.Deps{GitLab: gl, Labels: gl})
	if _, ok := d.GitLab.(*diag.GitLab); !ok {
		t.Fatalf("GitLab: %T, want the diag wrapper outermost", d.GitLab)
	}
	if _, ok := d.Labels.(*gitlab.Client); ok {
		t.Fatal("bot label client not wrapped")
	}
	if d.ServiceLabels != nil {
		t.Fatal("nil service label client must stay nil")
	}
	d = instrument(in, &cfg, review.Deps{ServiceLabels: gl})
	if _, ok := d.ServiceLabels.(*gitlab.Client); ok || d.ServiceLabels == nil {
		t.Fatal("service label client not wrapped")
	}
}
