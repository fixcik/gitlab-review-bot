package marker

import (
	"fmt"
	"regexp"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Summary blocks: each feature's section of the summary note lives between
// a start and an end marker, so it can be replaced in place.

const (
	TBDStart = "<!-- reviewbot:tbd -->"
	TBDEnd   = "<!-- /reviewbot:tbd -->"
)

var TBDRe = regexp.MustCompile(`(?s)<!--\s*reviewbot:tbd\s*-->.*?<!--\s*/reviewbot:tbd\s*-->`)

// CIBlockStart opens the CI block of a failed pipeline.
func CIBlockStart(p core.Pipeline, fp string) string {
	return fmt.Sprintf("<!-- reviewbot:ci pipeline=%d sha=%s fp=%s -->", p.ID, p.SHA, FP(fp))
}

const CIBlockEnd = "<!-- /reviewbot:ci -->"

var (
	CIBlockRe    = regexp.MustCompile(`(?s)<!--\s*reviewbot:ci\s+[^>]*-->.*?<!--\s*/reviewbot:ci\s*-->`)
	CIBlockHdrRe = regexp.MustCompile(`<!--\s*reviewbot:ci\s+pipeline=(\d+)\s+sha=([0-9a-fA-F]*)(?:\s+fp=([0-9a-f-]*))?\s*-->`)
)

const (
	CILintStart = "<!-- reviewbot:cilint -->"
	CILintEnd   = "<!-- /reviewbot:cilint -->"
)

var (
	CILintRe        = regexp.MustCompile(`(?s)<!--\s*reviewbot:cilint\s*-->.*?<!--\s*/reviewbot:cilint\s*-->`)
	CILintPendingRe = regexp.MustCompile(`<!--\s*reviewbot:cilint-pending\s+([a-z0-9:._,-]*)\s*-->`)
)

const (
	CoverageStart = "<!-- reviewbot:coverage -->"
	CoverageEnd   = "<!-- /reviewbot:coverage -->"
)

var CoverageRe = regexp.MustCompile(`(?s)<!--\s*reviewbot:coverage\s*-->.*?<!--\s*/reviewbot:coverage\s*-->`)

// The diagram section lives between these markers, so an incremental review
// without a new diagram can keep the previous one.
const (
	DiagramStart = "<!-- reviewbot:diagram -->"
	DiagramEnd   = "<!-- /reviewbot:diagram -->"
)

var DiagramRe = regexp.MustCompile(`(?s)<!--\s*reviewbot:diagram\s*-->.*?<!--\s*/reviewbot:diagram\s*-->`)

const (
	ProblemsStart = "<!-- reviewbot:problems -->"
	ProblemsEnd   = "<!-- /reviewbot:problems -->"
)

const (
	SysmapStart = "<!-- reviewbot:sysmap -->"
	SysmapEnd   = "<!-- /reviewbot:sysmap -->"
)

const (
	SysmapDepsStart = "<!-- reviewbot:sysmap-deps -->"
	SysmapDepsEnd   = "<!-- /reviewbot:sysmap-deps -->"
)

// The title hint lives between these markers in the summary note so that a
// title-only change can replace it without a new review.
const (
	TitleHintStart = "<!-- reviewbot:title-hint -->"
	TitleHintEnd   = "<!-- /reviewbot:title-hint -->"
)

var (
	TitleHintRe = regexp.MustCompile(`(?s)<!--\s*reviewbot:title-hint\s*-->.*?<!--\s*/reviewbot:title-hint\s*-->`)
	// TitleSuggestionRe holds Claude's last title suggestion (JSON) inside
	// the hint block, for refreshes without Claude.
	TitleSuggestionRe = regexp.MustCompile(`<!--\s*reviewbot:title-suggestion\s+(\{.*?\})\s*-->`)
)

const (
	TooBigStart = "<!-- reviewbot:too-big -->"
	TooBigEnd   = "<!-- /reviewbot:too-big -->"
)

const (
	VerifyStart = "<!-- reviewbot:verify -->"
	VerifyEnd   = "<!-- /reviewbot:verify -->"
)
