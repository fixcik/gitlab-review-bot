package config

import (
	"fmt"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// PathInstruction is one entry of path_instructions: extra review
// instructions for the files matching a doublestar glob. Service entries
// (review.path_instructions) apply to every repository; repository entries
// (.reviewbot.yaml, read from the target branch) are added after them.
type PathInstruction struct {
	Path         string `yaml:"path"`         // doublestar glob, e.g. "migrations/**"
	Instructions string `yaml:"instructions"` // free text for the reviewer
}

// Limits of path_instructions (per list: service and repository each).
const (
	MaxPathInstructions     = 50
	MaxPathInstructionRunes = 2000
)

// cleanGlob trims spaces and a leading "./" or "/" (paths in the diff are
// relative to the repository root).
func cleanGlob(g string) string {
	g = strings.TrimSpace(g)
	g = strings.TrimPrefix(g, "./")
	return strings.TrimLeft(g, "/")
}

// normalizePathInstructions validates and cleans a path_instructions list.
// where names the list in error messages ("path_instructions",
// "review.path_instructions").
func normalizePathInstructions(list []PathInstruction, where string) ([]PathInstruction, error) {
	if len(list) > MaxPathInstructions {
		return nil, fmt.Errorf("%s: at most %d entries allowed, got %d", where, MaxPathInstructions, len(list))
	}
	out := make([]PathInstruction, 0, len(list))
	for i, pi := range list {
		g := cleanGlob(pi.Path)
		text := strings.TrimSpace(pi.Instructions)
		switch {
		case g == "":
			return nil, fmt.Errorf("%s[%d]: path is required", where, i)
		case !doublestar.ValidatePattern(g):
			return nil, fmt.Errorf("%s[%d]: invalid glob %q", where, i, pi.Path)
		case text == "":
			return nil, fmt.Errorf("%s[%d] (%s): instructions are required", where, i, g)
		case len([]rune(text)) > MaxPathInstructionRunes:
			return nil, fmt.Errorf("%s[%d] (%s): instructions longer than %d characters", where, i, g, MaxPathInstructionRunes)
		}
		out = append(out, PathInstruction{Path: g, Instructions: text})
	}
	return out, nil
}

// validatePathInstructions reports problems of review.path_instructions.
func (r Review) validatePathInstructions() []string {
	if _, err := normalizePathInstructions(r.PathInstructions, "review.path_instructions"); err != nil {
		return []string{err.Error()}
	}
	return nil
}

// mergePathInstructions returns the service entries followed by the repo
// entries (both cleaned). Invalid service entries are dropped (Validate
// reports them at startup); repo entries were validated by ParseRepo.
// repo may be nil.
func mergePathInstructions(svc *Service, repo *Repo) []PathInstruction {
	var out []PathInstruction
	for _, pi := range svc.Review.PathInstructions {
		if one, err := normalizePathInstructions([]PathInstruction{pi}, "review.path_instructions"); err == nil {
			out = append(out, one...)
		}
	}
	if repo != nil {
		out = append(out, repo.PathInstructions...)
	}
	return out
}

// MatchPath reports whether file matches the instruction's glob.
func (p PathInstruction) MatchPath(file string) bool {
	ok, err := doublestar.Match(p.Path, file)
	return err == nil && ok
}
