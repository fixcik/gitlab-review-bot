// Package compat runs the deterministic backward-compatibility checks of a
// merge request: it finds the API spec files the MR changes (OpenAPI, proto,
// GraphQL), puts their base and head versions side by side in a scratch
// directory inside the checkout, and runs the spec diff tools (oasdiff, buf,
// graphql-inspector) in the sandbox image with no network and no credentials
// (core.Runner.Exec). Nothing from the repository is executed: the
// repository-defined checker commands (compat.commands) were removed.
//
// A tool that is not installed (exit 127) or fails is never a breaking
// change: it is reported as an Issue and the review goes on.
package compat

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Sources of a Change.
const (
	SourceOasdiff = "oasdiff" // also the Swagger 2.0 conversion (oas2to3)
	SourceBuf     = "buf"
	SourceGraphQL = "graphql-inspector"
	// SourceCustom marked the threads of compat.commands (removed). It is
	// kept so threads published before the removal are still recognised;
	// they are never auto-resolved (internal/review).
	SourceCustom = "custom"
)

// Spec kinds.
const (
	KindOpenAPI = "openapi"
	KindProto   = "proto"
	KindGraphQL = "graphql"
)

// Issue kinds.
const (
	// IssueUnavailable: the tool is not installed in the sandbox image.
	IssueUnavailable = "unavailable"
	// IssueFailed: the tool ran but could not check (spec does not load,
	// compile error, timeout, rejected command, ...).
	IssueFailed = "failed"
	// IssueSkipped: a file was deliberately not checked (e.g. too large).
	IssueSkipped = "skipped"
)

// ScratchDir is the directory inside the checkout where base and head
// versions of the specs are materialized. It is git-excluded and removed
// after the checks.
const ScratchDir = ".reviewbot-compat"

// Limits for materializing files.
const (
	maxFilesPerSide = 5000
	maxBytesPerSide = 50 << 20
	maxSniffBytes   = 2 << 20
	defaultTimeout  = 3 * time.Minute
)

// Change is one backward-incompatible change found by a tool.
type Change struct {
	Source string // oasdiff | buf | graphql-inspector | custom
	Path   string // repository path of the file; "" if unknown
	Line   int    // line in the NEW file; 0 if unknown
	Rule   string // tool rule id (oasdiff id, buf type), or the custom command
	// Message says what breaks (tool text; for custom commands the output).
	Message string
	// Fingerprint is a stable id the tool gives the change (oasdiff), if any.
	Fingerprint string
}

// ID is the stable id of the change for thread dedupe: first 12 hex chars
// of sha1(source, path, rule, fingerprint-or-message). Line numbers are not
// part of it, so the id survives unrelated edits above the change.
func (c Change) ID() string {
	key := c.Fingerprint
	if key == "" {
		key = strings.Join(strings.Fields(c.Message), " ")
	}
	return ID(c.Source, c.Path, c.Rule+"\n"+key)
}

// ID hashes the identity of a compat item (shared with Claude-reported
// items, whose key is the lowercased title).
func ID(source, filePath, key string) string {
	sum := sha1.Sum([]byte(source + "\n" + filePath + "\n" + strings.ToLower(strings.TrimSpace(key))))
	return hex.EncodeToString(sum[:])[:12]
}

// Issue is a check that could not be done.
type Issue struct {
	Source string
	Path   string // spec path or the custom command
	Kind   string // unavailable | failed | skipped
	Reason string // Russian, one line
}

// APIChange is one item of an OpenAPI changelog (oasdiff changelog): every
// level, not only breaking changes.
type APIChange struct {
	// Level: 3 breaking (oasdiff ERR), 2 potentially breaking (WARN),
	// 1 compatible (INFO: additions, deprecations, ...).
	Level int
	Rule  string // oasdiff rule id ("endpoint-added", ...)
	// Operation is the HTTP method (upper case) and Endpoint the path
	// template; both "" for changes outside an operation (components,
	// global security, ...).
	Operation, Endpoint string
	// Text is the tool's description (localized with Request.Lang). It is
	// derived from repository content: untrusted.
	Text string
	// File and Line locate the change in the NEW version (File may be a
	// $ref'd sibling of the spec); Line is 0 when unknown, for removals and
	// for specs converted from Swagger 2.0.
	File string
	Line int
}

// APIChanges is the changelog of one OpenAPI spec changed by the MR.
type APIChanges struct {
	Spec string
	// Converted: at least one side was Swagger 2.0 and was converted to
	// OpenAPI 3.0 before the diff (line numbers are then not reliable).
	Converted bool
	Items     []APIChange
}

// Spec is a discovered spec file changed by the MR.
type Spec struct {
	Kind string
	Path string
}

// Result of Check.
type Result struct {
	// Specs are the changed spec files that exist in both base and head.
	Specs []Spec
	// Deleted are spec files that exist in base but not in head.
	Deleted []Spec
	// Changes are definite breaking changes (they block the merge).
	Changes []Change
	// Warnings are potential breaking changes a tool could not confirm
	// (oasdiff WARN level). Context for Claude, not published as threads.
	Warnings []Change
	Issues   []Issue
	// API is the full changelog of every OpenAPI spec that could be diffed
	// (in spec order), for the "what changed in the API" summary.
	API []APIChanges
}

// SourceOK reports whether every check of source completed. Threads of a
// source are only auto-resolved when it did (a missing tool must not
// silently close a blocking thread).
func (r *Result) SourceOK(source string) bool {
	if r == nil {
		return false
	}
	for _, is := range r.Issues {
		if is.Source == source && (is.Kind == IssueUnavailable || is.Kind == IssueFailed) {
			return false
		}
	}
	return true
}

// Config is what the checker needs from config.Compat.
type Config struct {
	OpenAPI, Proto, GraphQL []string // extra path globs
}

// FromEffective converts the effective repo config.
func FromEffective(c config.Compat) Config {
	return Config{OpenAPI: c.OpenAPI, Proto: c.Proto, GraphQL: c.GraphQL}
}

// Request is one check.
type Request struct {
	Checkout core.Checkout
	// Base is the merge base (compatibility is judged against the target
	// branch, not the previous review), Head the MR head.
	Base, Head string
	Config     Config
	// Lang is the language of oasdiff texts: "ru" or "en" (anything else
	// is treated as "en"; "" as "ru").
	Lang string
	// SummaryOnly runs only the OpenAPI changelog (Result.API): no proto or
	// GraphQL checks, and Changes / Warnings stay empty. Used when
	// the repository turned the compat check off but the API summary is
	// still wanted.
	SummaryOnly bool
}

// Checker runs the checks.
type Checker struct {
	Runner  core.Runner
	Timeout time.Duration // per tool run
	Logger  *slog.Logger
}

// Check runs every applicable check. The error is non-nil only when the
// checks could not start at all (git failure); tool problems are Issues.
func (c *Checker) Check(ctx context.Context, req Request) (*Result, error) {
	log := c.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	co := req.Checkout
	changed, err := co.ChangedFiles(ctx, req.Base, nil)
	if err != nil {
		return nil, fmt.Errorf("compat: changed files: %w", err)
	}
	res := &Result{}
	specs := Discover(changed, req.Config, func(p string) []byte {
		b, err := co.ShowFile(ctx, req.Head, p)
		if err != nil {
			b, err = co.ShowFile(ctx, req.Base, p)
		}
		if err != nil || len(b) > maxSniffBytes {
			return nil
		}
		return b
	})
	// Keep only files that exist on both sides: new specs cannot break
	// anyone, deleted ones are reported to Claude as data.
	var both []Spec
	for _, s := range specs {
		if req.SummaryOnly && s.Kind != KindOpenAPI {
			continue
		}
		inHead := exists(ctx, co, req.Head, s.Path)
		inBase := exists(ctx, co, req.Base, s.Path)
		switch {
		case inHead && inBase:
			both = append(both, s)
		case inBase:
			res.Deleted = append(res.Deleted, s)
		}
	}
	res.Specs = both

	if len(both) > 0 {
		m, err := newMaterializer(co, req.Base, req.Head)
		if err != nil {
			return nil, err
		}
		defer m.cleanup()
		for _, s := range both {
			if s.Kind == KindOpenAPI {
				c.checkOpenAPI(ctx, m, s.Path, oasdiffLang(req.Lang), res)
			}
		}
		if !req.SummaryOnly {
			c.checkOthers(ctx, m, both, res)
		}
	}
	if req.SummaryOnly {
		// The repository turned the check off: nothing may block.
		res.Changes, res.Warnings = nil, nil
	}
	for _, is := range res.Issues {
		log.Info("compat: check issue", "source", is.Source, "path", is.Path, "kind", is.Kind, "reason", is.Reason)
	}
	return res, nil
}

// checkOthers runs buf and graphql-inspector on the changed specs.
func (c *Checker) checkOthers(ctx context.Context, m *materializer, both []Spec, res *Result) {
	var protos []string
	for _, s := range both {
		if s.Kind == KindProto {
			protos = append(protos, s.Path)
		}
	}
	if len(protos) > 0 {
		c.checkProto(ctx, m, protos, res)
	}
	for _, s := range both {
		if s.Kind == KindGraphQL {
			c.checkGraphQL(ctx, m, s.Path, res)
		}
	}
}

func exists(ctx context.Context, co core.Checkout, rev, p string) bool {
	_, err := co.ShowFile(ctx, rev, p)
	return err == nil
}
