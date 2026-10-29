package main

import "testing"

func TestKnownClaudeModel(t *testing.T) {
	for m, want := range map[string]bool{
		"sonnet": true, "claude-opus-4-1": true, "HAIKU": true,
		"no-such-model-xyz": false, "gpt-4": false, "": false,
	} {
		if got := knownClaudeModel(m); got != want {
			t.Errorf("knownClaudeModel(%q) = %v, want %v", m, got, want)
		}
	}
}
