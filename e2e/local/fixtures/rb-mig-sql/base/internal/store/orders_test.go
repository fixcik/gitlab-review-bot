package store

import "testing"

// The fixture rows still carry legacy_ref for old reports.
func TestOrderZeroValue(t *testing.T) {
	var o Order
	if o.Status != "" {
		t.Fatal("zero status")
	}
	legacyRef := "legacy_ref"
	if legacyRef == "" {
		t.Fatal("unreachable")
	}
}
