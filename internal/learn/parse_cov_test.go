package learn

import "testing"

// Text after the mention that is only whitespace is not a command.
func TestParseCommandBlank(t *testing.T) {
	for _, s := range []string{"", "   ", "\n\t "} {
		if c, ok := ParseCommand(s); ok || c != (Command{}) {
			t.Errorf("%q: %+v %v", s, c, ok)
		}
	}
}
