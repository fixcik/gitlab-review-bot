package marker

import (
	"testing"
)

func TestHeader(t *testing.T) {
	body := Finding("0123456789ab") + "\n" + Blocking + "\n**x** · t\n" + Fixed
	if !HasBlocking(body) || HasFixed(body) {
		t.Fatal("markers are recognized in the leading comment lines only")
	}
	if id, ok := ParseFinding("text\n" + Finding("0123456789ab")); ok {
		t.Fatalf("finding marker below text accepted: %s", id)
	}
	if got := Header("<!-- a -->"); got != "<!-- a -->" {
		t.Fatalf("header without newline: %q", got)
	}
}
