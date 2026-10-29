package config

// Learn configures learning team rules from people's replies in bot threads
// (review.learn). Rules are stored in SQLite and added to every later review
// and chat prompt of the repositories they apply to.
type Learn struct {
	// Enabled turns learning and applying rules on for the whole service.
	Enabled bool `yaml:"enabled"`
	// Model overrides claude.light_model for the extraction run. Empty uses
	// claude.light_model (and the CLI default when that is empty too).
	Model string `yaml:"model"`
	// MaxPerHour caps extraction runs per hour (they also count against
	// limits.global_runs_per_hour). <= 0 = no separate cap.
	MaxPerHour int `yaml:"max_per_hour"`
	// MaxInPrompt caps how many rules go into one prompt (newest first);
	// <= 0 uses the default.
	MaxInPrompt int `yaml:"max_in_prompt"`
	// FromActions tracks what happens to each finding (fixed / closed
	// without changes) and learns rules from repeated dismissals.
	FromActions bool `yaml:"from_actions"`
	// DismissThreshold: unused dismissed findings of a project that trigger
	// rule synthesis; <= 0 uses the default.
	DismissThreshold int `yaml:"dismiss_threshold"`
}

// DefaultDismissThreshold is used when review.learn.dismiss_threshold is
// not set.
const DefaultDismissThreshold = 3

// DefaultLearnMaxInPrompt is used when review.learn.max_in_prompt is not set.
const DefaultLearnMaxInPrompt = 80

func defaultLearn() Learn {
	return Learn{Enabled: true, MaxPerHour: 30, MaxInPrompt: DefaultLearnMaxInPrompt,
		FromActions: true, DismissThreshold: DefaultDismissThreshold}
}

// ExtractModel is the model for the rule extraction run: review.learn.model,
// else claude.light_model, else "" (CLI default).
func (c *Service) ExtractModel() string {
	if c.Review.Learn.Model != "" {
		return c.Review.Learn.Model
	}
	return c.Claude.LightModel
}

// RepoLearn is the learn section of .reviewbot.yaml.
type RepoLearn struct {
	Enabled *bool `yaml:"enabled"`
}

// learnEnabled reports the repo's own switch (true when not set). Safe on a
// nil *Repo.
func (r *Repo) learnEnabled() bool {
	return r == nil || r.Learn == nil || defaultTrue(r.Learn.Enabled)
}
