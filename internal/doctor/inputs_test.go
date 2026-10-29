package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestInputsProviders(t *testing.T) {
	ex := fakeExec{}
	in := Inputs{
		ConfigPath: "config.yaml",
		ConfigErr:  errors.New("GITLAB_TOKEN is required\ngitlab.events must be x"),
		Docker:     DockerInput{Image: "img", Network: "net", Exec: ex.run},
		Egress:     EgressInput{Proxy: "http://egress:3128", Allow: []string{"api.anthropic.com"}, Dial: func(context.Context, string) (string, error) { return "", errors.New("dial: refused") }},
	}
	r := Run(context.Background(), time.Unix(0, 0), in.Providers()...)
	got := byID(r)
	c := got["config.valid"]
	if c.Status != Fail || !strings.Contains(c.Detail, "GITLAB_TOKEN is required; gitlab.events") || c.Action == "" {
		t.Fatalf("config = %+v", c)
	}
	for _, id := range []string{"gitlab.version", "docker.access", "egress.proxy", "claude.token"} {
		if _, ok := got[id]; !ok {
			t.Errorf("%s missing in %v", id, r.Results)
		}
	}
	if r.Results[0].ID != "config.valid" {
		t.Fatalf("config must come first: %v", r.Results[0])
	}
	// Valid config; no path = no config check.
	if res := Config("c.yaml", nil).Check(context.Background()); res[0].Status != OK {
		t.Fatalf("valid = %+v", res)
	}
	in.ConfigPath = ""
	for _, x := range Run(context.Background(), time.Unix(0, 0), in.Providers()...).Results {
		if x.ID == "config.valid" {
			t.Fatal("no path: no config check")
		}
	}
}
