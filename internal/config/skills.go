package config

import (
	"os"
	"strings"
)

// Skills configures team skills (review.skills): a directory on the bot's
// host with skills in the Claude Code SKILL.md format, offered to Claude in
// reviews and thread answers (see internal/skills).
type Skills struct {
	// Dir is the skills directory: <dir>/<name>/SKILL.md. Empty = no skills.
	// Re-read on every run, so new skills need no restart.
	Dir string `yaml:"dir"`
}

// On reports whether skills are configured.
func (s Skills) On() bool { return strings.TrimSpace(s.Dir) != "" }

func (s Skills) validate() []string {
	if !s.On() {
		return nil
	}
	fi, err := os.Stat(s.Dir)
	if err != nil || !fi.IsDir() {
		return []string{"review.skills.dir must be an existing directory: " + s.Dir}
	}
	return nil
}

// RepoSkills is the skills section of .reviewbot.yaml.
type RepoSkills struct {
	// Enabled=false offers no skills in this repository's reviews.
	Enabled *bool `yaml:"enabled"`
	// Disable lists skill names not offered in this repository.
	Disable []string `yaml:"disable"`
}

func (r *Repo) skillsEnabled() bool {
	return r == nil || r.Skills == nil || defaultTrue(r.Skills.Enabled)
}

func (r *Repo) skillsDisable() []string {
	if r == nil || r.Skills == nil {
		return nil
	}
	var out []string
	for _, d := range r.Skills.Disable {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			out = append(out, d)
		}
	}
	return out
}
