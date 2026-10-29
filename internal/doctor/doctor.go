// Package doctor checks a reviewbot installation before the first merge
// request: GitLab (address, the bot's token, scope and role, the service
// token, how events arrive), Docker (daemon access, the sandbox image and
// network), the egress proxy and the Claude token. `reviewbot doctor`,
// `reviewbot init` and the /setup page show the same results.
//
// Checks are read-only: GitLab GETs only, `docker ... inspect`, a CONNECT
// through the egress proxy. Secrets are never put into a result: a token is
// "принят" or "нет", never shown. Every problem comes with an action.
package doctor

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// Status of one check.
type Status string

// Statuses.
const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "fail"
	Skip Status = "skip" // not checked (an earlier check failed or nothing to check)
)

// Result is one check: what, the outcome, why and what to do.
type Result struct {
	ID     string `json:"id"`
	Group  string `json:"group"`
	Title  string `json:"title"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
	Action string `json:"action,omitempty"`
}

// Provider is a set of checks (GitLab, Docker, egress, Claude).
type Provider interface {
	Check(ctx context.Context) []Result
}

// ProviderFunc adapts a function to Provider.
type ProviderFunc func(ctx context.Context) []Result

// Check implements Provider.
func (f ProviderFunc) Check(ctx context.Context) []Result { return f(ctx) }

// Report is the outcome of a run.
type Report struct {
	At      time.Time `json:"at"`
	Results []Result  `json:"results"`
	Fails   int       `json:"fails"`
	Warns   int       `json:"warns"`
}

// Run runs the providers in order.
func Run(ctx context.Context, now time.Time, ps ...Provider) Report {
	r := Report{At: now, Results: []Result{}}
	for _, p := range ps {
		for _, x := range p.Check(ctx) {
			r.Results = append(r.Results, x)
			switch x.Status {
			case Fail:
				r.Fails++
			case Warn:
				r.Warns++
			}
		}
	}
	return r
}

// ExitCode is 1 when a check failed, else 0.
func (r Report) ExitCode() int {
	if r.Fails > 0 {
		return 1
	}
	return 0
}

var marks = map[Status]string{OK: "✓", Warn: "!", Fail: "✗", Skip: "·"}

// WriteText prints the report for a terminal.
func (r Report) WriteText(w io.Writer) {
	for _, x := range r.Results {
		line := marks[x.Status] + " " + x.Group + ": " + x.Title
		if x.Detail != "" {
			line += " — " + x.Detail
		}
		fmt.Fprintln(w, line)
		if x.Action != "" && x.Status != OK {
			fmt.Fprintln(w, "    → "+x.Action)
		}
	}
	fmt.Fprintf(w, "\nИтог: %d %s, %d %s\n", r.Fails, plural(r.Fails, "ошибка", "ошибки", "ошибок"), r.Warns, plural(r.Warns, "предупреждение", "предупреждения", "предупреждений"))
}

func plural(n int, one, few, many string) string {
	n10, n100 := n%10, n%100
	switch {
	case n10 == 1 && n100 != 11:
		return one
	case n10 >= 2 && n10 <= 4 && (n100 < 10 || n100 >= 20):
		return few
	}
	return many
}

// oneLine shortens an error or a command output for a result.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}
