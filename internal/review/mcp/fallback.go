package mcp

// MCP fallback (review.mcp). The MCP path depends on the CLI actually
// connecting to the bot's server through the egress forward; a missing
// forward, a firewall or a CLI change would otherwise leave the model with a
// prompt whose big sections are one-line pointers to tools it does not have.
//
// After each MCP review run the bot decides whether the tools were usable
// (runFailure): the CLI's stream-json init event reports the server's
// status (RunResult.MCPStatus); when it is not reported or still "pending",
// the session itself tells whether any authenticated request arrived
// (mcpserver.Session.Connected). On a failure the answer is discarded, an
// mcp_failed problem is recorded, the session is dropped (from then on the
// run behaves as with MCP off: no tools in the verification run either)
// and the review runs ONCE more without MCP: the full prompt sections, the
// answer's "problems" array, no MCP tools. That run counts like the retry of
// env.RunStructured: the global hourly limit is checked first and the run is
// recorded. When the limit denies it, the first answer is used as it is.
// The server then skips MCP for new runs for a while (Server.Fallback, a
// per-process circuit breaker; Feature.Open), so a broken deployment costs one
// extra run per cooldown, not one per review.

import (
	"context"
	"errors"
	"fmt"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/diag"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
)

// Fallback reasons (reviewbot_mcp_fallbacks_total{reason}).
const (
	// fallbackDropped: the runner could not write the MCP config.
	fallbackDropped = "config_dropped"
	// fallbackNotConnected: the init event reported the server as not
	// connected (failed, needs-auth, ...).
	fallbackNotConnected = "not_connected"
	// fallbackNoRequest: no status (or "pending") and not a single
	// authenticated request reached the session.
	fallbackNoRequest = "no_request"
)

// MCP server statuses of the CLI's init event (core.RunResult.MCPStatus).
const (
	StatusConnected = "connected" // the CLI connected to the server
	statusPending   = "pending"
)

// runFailure tells why a run could not use the MCP tools ("" = it
// could). The CLI's own status wins; without one the server's view decides.
func runFailure(dropped bool, status string, connected bool) string {
	switch {
	case dropped:
		return fallbackDropped
	case status == StatusConnected:
		return ""
	case status != "" && status != statusPending:
		return fallbackNotConnected
	case !connected:
		return fallbackNoRequest
	}
	return ""
}

// fallbackError ends an MCP run whose tools were unusable.
type fallbackError struct {
	reason string
	status string // the init event's status ("" = not reported)
	output string // the run's answer (used when the re-run is denied)
}

func (e *fallbackError) Error() string {
	return fmt.Sprintf("mcp tools unusable in the run (%s, status %q)", e.reason, e.status)
}

// runCheck is the env.Env.RunStructuredCheck hook of an MCP run.
func (m *Run) runCheck(res *core.RunResult) error {
	if m == nil || m.srv == nil || res == nil {
		return nil
	}
	reason := runFailure(res.MCPDropped, res.MCPStatus, m.sess.Connected())
	if reason == "" {
		return nil
	}
	return &fallbackError{reason: reason, status: res.MCPStatus, output: res.Output}
}

// drop ends the MCP side of the run after a failed connection: the session
// is closed, anything submitted is forgotten and every helper answers as
// with MCP off from now on (m.srv = nil). It records the problem and opens
// the server's circuit breaker.
func (m *Run) drop(ctx context.Context, fb *fallbackError) {
	m.log.Warn("review: mcp: the CLI did not use the bot's MCP server, running the review again without the tools",
		"reason", fb.reason, "status", fb.status)
	subject, detail := "MCP-сервер бота недоступен из песочницы", "статус в CLI: "+fb.status
	switch fb.reason {
	case fallbackDropped:
		subject, detail = "конфигурация MCP не записана", "запуск выполнен без инструментов бота"
	case fallbackNoRequest:
		detail = "CLI не прислал ни одного запроса на " + m.srv.URL
	}
	jobctx.BotProblem(ctx, problems.MCPFailed, subject, detail+"; ревью повторено без MCP (полный промпт), MCP пропускается до конца паузы")
	diag.Note(ctx, "mcp: fallback", diag.Fields{"reason": fb.reason, "status": fb.status})
	if m.sess != nil {
		m.sess.Close()
		m.sess = nil
	}
	m.mu.Lock()
	m.drafts = nil
	m.mu.Unlock()
	m.srv = nil
	m.f.srv.Fallback(fb.reason)
}

// RunReview makes the review run (build(ms)) and, when its MCP tools were
// unusable, once more without them (see mcp/fallback.go).
func (mf *Feature) RunReview(ctx context.Context, job core.Job, ms *Run, build func(*Run) (core.RunRequest, func(string) error, error)) error {
	req, parse, err := build(ms)
	if err != nil {
		return err
	}
	if ms.Server() == nil {
		return mf.env.RunStructured(ctx, job, req, parse)
	}
	err = mf.env.RunStructuredCheck(ctx, job, req, parse, ms.runCheck)
	var fb *fallbackError
	if !errors.As(err, &fb) {
		return err
	}
	ms.drop(ctx, fb)
	if req, parse, err = build(ms); err != nil {
		return err
	}
	// The re-run is another Claude run: the global hourly limit applies
	// like to env.RunStructured's retry (env.RunStructured records it).
	dec, err := mf.env.Limiter.CheckGlobal(ctx)
	if err != nil {
		return err
	}
	if !dec.Allowed {
		mf.env.Log.Warn("review: mcp fallback run denied by the global limit, using the answer of the run without the tools",
			"mr", job.MRKey(), "reason", dec.Reason)
		if perr := parse(fb.output); perr != nil {
			return &env.InvalidOutputError{Err: perr}
		}
		return nil
	}
	return mf.env.RunStructured(ctx, job, req, parse)
}
