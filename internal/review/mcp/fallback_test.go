package mcp

import (
	"testing"
)

func TestMCPRunFailure(t *testing.T) {
	for _, tc := range []struct {
		dropped   bool
		status    string
		connected bool
		want      string
	}{
		{false, "connected", false, ""},
		{false, "connected", true, ""},
		{false, "failed", true, fallbackNotConnected},
		{false, "needs-auth", false, fallbackNotConnected},
		{false, "pending", true, ""},
		{false, "pending", false, fallbackNoRequest},
		{false, "", true, ""},
		{false, "", false, fallbackNoRequest},
		{true, "", false, fallbackDropped},
	} {
		if got := runFailure(tc.dropped, tc.status, tc.connected); got != tc.want {
			t.Errorf("%+v: %q", tc, got)
		}
	}
}
