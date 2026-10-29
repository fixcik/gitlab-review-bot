package jsonschema

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

func TestUnmarshalStrict(t *testing.T) {
	var v output.ReviewOutput
	for _, ok := range []string{`{"summary":"a"}`, " \n```json\n{\"summary\":\"a\"}\n```\n", "```\n{\"summary\":\"a\"}\n```"} {
		if err := UnmarshalStrict(ok, &v); err != nil || v.Summary != "a" {
			t.Errorf("%q: err=%v summary=%q", ok, err, v.Summary)
		}
	}
	for _, bad := range []string{"", "   ", "```json\n```", `Here it is: {"summary":"a"}`, `{"summary":`, `[]`, `null`} {
		if err := UnmarshalStrict(bad, &v); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}
