package claude

import (
	"errors"
	"testing"
)

// Only the CLI's advisor refusals switch the advisor off; a failed run whose
// text merely mentions an advisor (e.g. the model's prose) does not.
func TestIsAdvisorErrorKnownMessagesOnly(t *testing.T) {
	for _, msg := range []string{
		`API Error: 400 Error: The model "claude-sonnet" does not support the advisor tool.`,
		`"haiku" is not a valid advisor model`,
		`The configured advisor model is not compatible with the main model`,
		`the advisor tool is not available for this account`,
		`error: unknown option '--advisor'`,
		`Opus cannot advise on this request`,
	} {
		if !isAdvisorError(errors.New(msg)) {
			t.Errorf("not recognised: %q", msg)
		}
	}
	for _, msg := range []string{
		`claude run failed: result: I asked the advisor and the migration is unsafe`,
		`invalid output: {"summary":"advisor says..."}`,
		`advisor`,
	} {
		if isAdvisorError(errors.New(msg)) {
			t.Errorf("false positive: %q", msg)
		}
	}
}
