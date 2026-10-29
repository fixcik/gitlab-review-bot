package config

import (
	"fmt"
	"math"
	"os"
	"strings"
	"time"
)

// Usage configures the token consumption page (/usage).
type Usage struct {
	// Enabled turns the page on (default true when unset).
	Enabled *bool `yaml:"enabled"`
	// Token (env USAGE_TOKEN preferred), when set, is required to open the
	// page: "Authorization: Bearer <token>" or "?token=<token>" once (it is
	// then kept in an HttpOnly cookie). Empty = the page is open to anyone
	// who can reach the bot's port.
	Token string `yaml:"token"`
	// Retention is how long run records are kept. Default 8760h (365 days).
	Retention Duration `yaml:"retention"`
	// BudgetUSD is the monthly budget in USD shown on /usage with a
	// month-end forecast. 0 (default) hides the budget block. Informational
	// only: the bot never limits or blocks anything by it.
	BudgetUSD float64 `yaml:"budget_usd"`
}

// MaxUsageBudgetUSD bounds usage.budget_usd (a typo guard, not a policy).
const MaxUsageBudgetUSD = 1_000_000

// DefaultUsageRetention is the default for usage.retention.
const DefaultUsageRetention = 365 * 24 * time.Hour

// On reports whether the usage page is enabled.
func (u Usage) On() bool { return defaultTrue(u.Enabled) }

// RetentionD returns the retention, defaulting to 365 days.
func (u Usage) RetentionD() time.Duration { return positiveOr(u.Retention.D(), DefaultUsageRetention) }

func (u Usage) validate() []string {
	b := u.BudgetUSD
	if math.IsNaN(b) || math.IsInf(b, 0) || b < 0 || b > MaxUsageBudgetUSD {
		return []string{fmt.Sprintf("usage.budget_usd: want a number from 0 (no budget) to %d, got %v", MaxUsageBudgetUSD, b)}
	}
	return nil
}

func applyUsageEnv(u *Usage) {
	if v := os.Getenv("USAGE_TOKEN"); v != "" {
		u.Token = v
	}
	u.Token = strings.TrimSpace(u.Token)
}
