package config

// diagramEnabled reports the repo's own diagram switch (true when not set).
// Safe on a nil *Repo.
func (r *Repo) diagramEnabled() bool {
	return r == nil || defaultTrue(r.Diagram)
}
