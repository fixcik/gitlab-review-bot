package config

import (
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

func TestSysmapTopology(t *testing.T) {
	if d := Defaults(); len(d.Review.Sysmap.Topology) != 0 {
		t.Fatalf("topology must be empty by default: %+v", d.Review.Sysmap.Topology)
	}
	var s Service
	y := "review:\n  sysmap:\n    topology:\n      - {from: team/shop-frontend, to: team/shop-backend, via: [http, ws]}\n      - {from: /team/shop-backend/, to: team/shop-provider, via: [HTTP]}\n"
	if err := yaml.Unmarshal([]byte(y), &s); err != nil || len(s.Review.Sysmap.Topology) != 2 || s.Review.Sysmap.Topology[0].Via[1] != "ws" {
		t.Fatalf("yaml: %v %+v", err, s.Review.Sysmap.Topology)
	}
	ok := mutSvc()
	ok.Review.Sysmap.Topology = s.Review.Sysmap.Topology
	wantValid(t, "rules", ok)
	for _, c := range []struct {
		rule SysmapTopologyRule
		want string
	}{
		{SysmapTopologyRule{From: "frontend", To: "g/b", Via: []string{"http"}}, `"frontend" is not a full project path`},
		{SysmapTopologyRule{From: "g/a", To: "g/../b", Via: []string{"http"}}, `"g/../b" is not a full project path`},
		{SysmapTopologyRule{From: "g/a", To: "g/b"}, "via: at least one protocol"},
		{SysmapTopologyRule{From: "g/a", To: "g/b", Via: []string{"grpc"}}, `unknown protocol "grpc" (http, webhook, ws, sse)`},
		{SysmapTopologyRule{From: "g/a", To: "G/A/", Via: []string{"http"}}, "from and to are the same project"},
	} {
		s := mutSvc()
		s.Review.Sysmap.Topology = []SysmapTopologyRule{c.rule}
		wantInvalid(t, c.want, s, "review.sysmap.topology[0]: ")
		wantInvalid(t, c.want, s, c.want)
	}
	many := mutSvc()
	for i := 0; i <= MaxSysmapTopology; i++ {
		many.Review.Sysmap.Topology = append(many.Review.Sysmap.Topology, SysmapTopologyRule{From: "g/a", To: "g/b", Via: []string{"http"}})
	}
	wantInvalid(t, "too many", many, "review.sysmap.topology: at most")
	// Service config only: a repository cannot declare the topology.
	if _, err := ParseRepo([]byte("sysmap:\n  topology: [{from: g/a, to: g/b, via: [http]}]\n")); err == nil || !strings.Contains(err.Error(), "sysmap.topology: unknown key") {
		t.Errorf("repo topology: %v", err)
	}
}
