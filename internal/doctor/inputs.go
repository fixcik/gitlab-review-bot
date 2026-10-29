package doctor

import (
	"context"
	"strings"
)

// Inputs is everything the standard checks need; `reviewbot doctor`, `init`
// and /setup fill it from the config (or the answers) and the environment.
type Inputs struct {
	// ConfigPath and ConfigErr: the config file and its validation error
	// (nil = valid); ConfigPath "" skips the config check (init, before
	// anything is written).
	ConfigPath string
	ConfigErr  error
	GitLab     GitLabInput
	Docker     DockerInput
	Egress     EgressInput
	Claude     ClaudeInput
}

// Providers are the standard checks in display order.
func (in Inputs) Providers() []Provider {
	ps := []Provider{}
	if in.ConfigPath != "" {
		ps = append(ps, Config(in.ConfigPath, in.ConfigErr))
	}
	return append(ps, GitLab(in.GitLab), Docker(in.Docker), Egress(in.Egress), Claude(in.Claude))
}

// Config reports the validation of the service config (the same rules as
// `reviewbot serve`).
func Config(path string, err error) Provider {
	return ProviderFunc(func(context.Context) []Result {
		r := Result{ID: "config.valid", Group: "Конфигурация", Title: path}
		if err == nil {
			r.Status, r.Detail = OK, "проходит проверку serve"
			return r.list()
		}
		// Validation errors are "a; b; c" or one per line: one short line.
		r.Status, r.Detail = Fail, oneLine(strings.ReplaceAll(err.Error(), "\n", "; "))
		r.Action = "исправьте ключи (docs/configuration.md) или перезапустите `reviewbot init`"
		return r.list()
	})
}

func (r Result) list() []Result { return []Result{r} }
