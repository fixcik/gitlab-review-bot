package config

import "time"

// CheckoutCache configures the cache of review checkouts
// (review.checkout_cache, internal/repo/treecache.go): read-only trees of
// commits (the MR head, the merge base for /base) checked out from the
// project mirror and shared by the jobs that need the same commit; kept
// after use for TTL, evicted least recently used first above MaxGB.
type CheckoutCache struct {
	// Enabled: false = a fresh clone per job and a git archive export of the
	// merge base per run (the old behaviour).
	Enabled bool `yaml:"enabled"`
	// TTL: how long an unused tree is kept after its last use.
	TTL Duration `yaml:"ttl"`
	// MaxGB: disk budget of the cached trees (file sizes); 0 = no budget.
	MaxGB float64 `yaml:"max_gb"`
}

func defaultCheckoutCache() CheckoutCache {
	return CheckoutCache{Enabled: true, TTL: Duration(2 * time.Hour), MaxGB: 10}
}

func (c CheckoutCache) validate() []string {
	if !c.Enabled {
		return nil
	}
	var errs []string
	if c.TTL.D() <= 0 {
		errs = append(errs, "review.checkout_cache.ttl must be positive (or set review.checkout_cache.enabled: false)")
	}
	if c.MaxGB < 0 {
		errs = append(errs, "review.checkout_cache.max_gb must not be negative")
	}
	return errs
}

// CacheTTL is the TTL of the checkout cache; 0 = off.
func (c CheckoutCache) CacheTTL() time.Duration {
	if !c.Enabled {
		return 0
	}
	return c.TTL.D()
}

// MaxBytes is MaxGB in bytes (0 = no budget).
func (c CheckoutCache) MaxBytes() int64 { return int64(c.MaxGB * (1 << 30)) }
