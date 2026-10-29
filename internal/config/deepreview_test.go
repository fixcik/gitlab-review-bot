package config

import (
	"testing"

	"github.com/goccy/go-yaml"
)

func TestDeepReviewModel(t *testing.T) {
	if d := Defaults(); d.Claude.DeepReviewModel != "opus" {
		t.Fatalf("default deep_review_model = %q", d.Claude.DeepReviewModel)
	}
	s := mutSvc()
	wantValid(t, "default", s)
	s.Claude.DeepReviewModel = ""
	wantValid(t, "off", s)
	s.Claude.DeepReviewModel = "claude-opus-4-1[1m]"
	wantValid(t, "full model id", s)
	for _, bad := range []string{"opus 4", "op\"us", "-opus", " opus"} {
		s.Claude.DeepReviewModel = bad
		wantInvalid(t, bad, s, "claude.deep_review_model")
	}
	var c Service
	if err := yaml.Unmarshal([]byte("claude:\n  deep_review_model: sonnet\n"), &c); err != nil || c.Claude.DeepReviewModel != "sonnet" {
		t.Errorf("yaml: %v %+v", err, c.Claude)
	}
}
