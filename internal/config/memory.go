package config

// Memory configures the per-project operational memory (review.memory):
// what the bot learned about building, testing and linting each repository,
// kept in the bot's database and given to Claude at the start of every run
// that uses the repository.
type Memory struct {
	// Enabled turns memory on for the whole service (default true). A nil
	// value (section absent) means true.
	Enabled *bool `yaml:"enabled"`
	// MaxChars caps the total memory of one project in characters (default
	// 6000). Each section is capped at min(1500, MaxChars).
	MaxChars int `yaml:"max_chars"`
}

// DefaultMemoryMaxChars is the default review.memory.max_chars.
const DefaultMemoryMaxChars = 6000

// On reports whether memory is enabled for the service.
func (m Memory) On() bool { return defaultTrue(m.Enabled) }

// TotalCap is the effective total cap.
func (m Memory) TotalCap() int { return positiveOr(m.MaxChars, DefaultMemoryMaxChars) }

// SectionCap is the effective per-section cap.
func (m Memory) SectionCap() int { return min(m.TotalCap(), 1500) }

// RepoMemory is the memory section of .reviewbot.yaml.
type RepoMemory struct {
	Enabled *bool `yaml:"enabled"`
}

// RepoMemoryEnabled reports the repository's own switch (true when not
// set). Safe on a nil *Repo.
func (r *Repo) RepoMemoryEnabled() bool {
	return r == nil || r.Memory == nil || defaultTrue(r.Memory.Enabled)
}
