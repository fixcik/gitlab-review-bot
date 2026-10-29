package review

import (
	"testing"
)

// The jsonschema.UnmarshalStrict cases are in the jsonschema package.
func TestFormatMutChatOutputOnlyMarker(t *testing.T) {
	if _, err := parseChatOutput(`{"reply":" <!-- reviewbot:fixed --> "}`); err == nil {
		t.Error("a reply that is only a marker is empty")
	}
}
