package config

import "testing"

func TestCIIgnoredJobParallel(t *testing.T) {
	e := EffCI{IgnoreJobs: []string{"e2e", "deploy-*"}}
	for name, want := range map[string]bool{
		"e2e": true, "e2e 1/4": true, "e2e 4/4": true, "e2e 5/4": false, "e2e x": false,
		"deploy-prod": true, "deploy-prod 2/3": true, "unit": false, "e2e-smoke": false,
	} {
		if got := e.IgnoredJob(name); got != want {
			t.Errorf("IgnoredJob(%q) = %v, want %v", name, got, want)
		}
	}
}
