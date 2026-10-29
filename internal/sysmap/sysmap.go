// Package sysmap builds the "system map": who provides and who consumes
// which contract across the repositories of gitlab.groups. Contracts are
// REST endpoints (http), webhooks, WebSocket (ws) and Server-Sent Events
// (sse) endpoints and events.
//
// Extraction works on the default-branch snapshots of internal/contextrepos
// (trunk-based: default branch = deployed):
//
//   - OpenAPI / Swagger specs are parsed deterministically (openapi.go);
//   - code files are prefiltered cheaply (prefilter.go) and sent in batches
//     to the light model without tools (extract.go); the files are untrusted
//     data in the prompt;
//   - per-file content hashes make it incremental: a new default-branch SHA
//     re-extracts only changed or new candidate files (scanner.go).
//
// Consumers are matched to providers deterministically (index.go, path
// normalization in normalize.go). A call that does not match exactly one
// provider repository stays "unresolved" with its raw expression, so a
// human can add a hint (review.sysmap.hints); nothing is guessed silently.
//
// Nothing here can fail a review: the reviewer only reads the last built
// Index, and every error is logged.
package sysmap

import "github.com/fixcik/gitlab-review-bot/internal/sysmap/model"

// Versions of the extractors. Bumping one re-extracts every file of that
// kind on the next pass (the version is stored per file).
const (
	// CodeVersion covers the code prefilter, prompt and schema.
	CodeVersion = "code-3"
	// SpecVersion covers the OpenAPI/Swagger parser.
	SpecVersion = "spec-1"
)

// The stored records and the Store interface live in the leaf
// internal/sysmap/model (the store implements it); they are aliased here
// for the map's code and its callers.
type (
	Side       = model.Side
	Protocol   = model.Protocol
	Entry      = model.Entry
	FileRecord = model.FileRecord
	RepoState  = model.RepoState
	RunRecord  = model.RunRecord
	RunStats   = model.RunStats
	Store      = model.Store
)

// Sides, protocols, sources, statuses and run kinds (see internal/sysmap/model).
const (
	Provides = model.Provides
	Consumes = model.Consumes

	HTTP    = model.HTTP
	Webhook = model.Webhook
	WS      = model.WS
	SSE     = model.SSE

	SourceOpenAPI = model.SourceOpenAPI
	SourceCode    = model.SourceCode

	StatusOK      = model.StatusOK
	StatusPartial = model.StatusPartial
	StatusError   = model.StatusError

	RunKindScan = model.RunKindScan
	RunKindDeps = model.RunKindDeps
)

// Protocols lists every protocol in display order.
var Protocols = []Protocol{HTTP, Webhook, WS, SSE}
