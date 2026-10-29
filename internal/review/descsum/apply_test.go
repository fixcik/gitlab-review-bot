package descsum

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

type fakeClient struct {
	mr      core.MergeRequest
	getErr  error
	putErr  error
	puts    []string
	getRead int
}

func (f *fakeClient) GetMR(context.Context, int64, int64) (*core.MergeRequest, error) {
	f.getRead++
	m := f.mr
	return &m, f.getErr
}

func (f *fakeClient) UpdateMRDescription(_ context.Context, _, _ int64, d string) error {
	f.puts = append(f.puts, d)
	if f.putErr == nil {
		f.mr.Description = d
	}
	return f.putErr
}

func testEnv(now *time.Time) *env.Env {
	return &env.Env{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return *now }}
}

func TestApplyClients(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	eff := config.Effective{DescriptionSummary: true}
	job := core.Job{ProjectID: 1, MRIID: 2}
	out := &output.ReviewOutput{DescriptionSummary: sample()}
	forbidden := errors.Join(errors.New("403"), core.ErrForbidden)

	// The bot's token is refused: the service token writes; the bot is not
	// tried again within the hour.
	bot := &fakeClient{mr: core.MergeRequest{State: "opened"}, putErr: forbidden}
	svc := &fakeClient{}
	f := New(testEnv(&now), bot, svc)
	if got := f.Apply(context.Background(), job, eff, "Russian", out, ""); got != Written+"\n" || len(svc.puts) != 1 || len(bot.puts) != 1 {
		t.Fatalf("fallback: %q bot %d svc %d", got, len(bot.puts), len(svc.puts))
	}
	bot.mr.Description = "другое"
	f.Apply(context.Background(), job, eff, "Russian", out, "")
	if len(bot.puts) != 1 || len(svc.puts) != 2 {
		t.Errorf("deny cache: bot %d svc %d", len(bot.puts), len(svc.puts))
	}
	// Both refused: nothing is tried until the hour passes.
	svc.putErr = forbidden
	bot.mr.Description = "ещё"
	f.Apply(context.Background(), job, eff, "Russian", out, "")
	n := len(svc.puts)
	bot.mr.Description = "снова"
	if got := f.Apply(context.Background(), job, eff, "Russian", out, ""); got != "" || len(svc.puts) != n {
		t.Errorf("denied: %q svc %d", got, len(svc.puts))
	}
	now = now.Add(2 * time.Hour)
	bot.putErr = errors.New("500")
	f.Apply(context.Background(), job, eff, "Russian", out, "")
	if len(bot.puts) != 2 {
		t.Errorf("retried after the hour: bot %d", len(bot.puts))
	}

	// Without clients, with an empty answer, off, a failed read, an
	// oversized description or the opt-out: nothing written.
	for name, c := range map[string]struct {
		f    *Feature
		eff  config.Effective
		out  *output.ReviewOutput
		prev string
		want string
	}{
		"no clients":         {New(testEnv(&now), nil, nil), eff, out, Written, Written + "\n"},
		"off":                {New(testEnv(&now), &fakeClient{}, nil), config.Effective{}, out, "", ""},
		"nil out":            {New(testEnv(&now), &fakeClient{}, nil), eff, nil, "", ""},
		"read fails":         {New(testEnv(&now), &fakeClient{getErr: errors.New("x")}, nil), eff, out, "", ""},
		"too big":            {New(testEnv(&now), &fakeClient{mr: core.MergeRequest{State: "opened", Description: strings.Repeat("x", maxDescriptionBytes)}}, nil), eff, out, "", ""},
		"opted out":          {New(testEnv(&now), &fakeClient{mr: core.MergeRequest{State: "opened", Description: Off}}, nil), eff, out, Written, ""},
		"draft":              {New(testEnv(&now), &fakeClient{mr: core.MergeRequest{State: "opened", Draft: true}}, nil), eff, out, "", ""},
		"api error":          {New(testEnv(&now), &fakeClient{mr: core.MergeRequest{State: "opened"}, putErr: errors.New("500")}, nil), eff, out, "", ""},
		"denied, no service": {New(testEnv(&now), &fakeClient{mr: core.MergeRequest{State: "opened"}, putErr: forbidden}, nil), eff, out, "", ""},
		"same":               {New(testEnv(&now), &fakeClient{mr: core.MergeRequest{State: "opened", Description: Render(sample(), "Russian")}}, nil), eff, out, "", Written + "\n"},
		"broken":             {New(testEnv(&now), &fakeClient{mr: core.MergeRequest{State: "opened", Description: Start}}, nil), eff, out, "", ""},
		"service only reads": {New(testEnv(&now), nil, &fakeClient{mr: core.MergeRequest{State: "opened"}}), eff, out, "", Written + "\n"},
	} {
		if got := c.f.Apply(context.Background(), job, c.eff, "Russian", c.out, c.prev); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}
