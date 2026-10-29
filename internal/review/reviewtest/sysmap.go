package reviewtest

import (
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
)

type FakeSysmap struct {
	Repo      string
	Changed   map[string][]sysmap.LineRange
	endpoints []sysmap.EndpointRef
	Imp       *sysmap.Impact
	Panics    bool
	calls     int
}

func (f *FakeSysmap) Impact(repo string, changed map[string][]sysmap.LineRange, endpoints []sysmap.EndpointRef) *sysmap.Impact {
	f.calls++
	if f.Panics {
		panic("boom")
	}
	f.Repo, f.Changed, f.endpoints = repo, changed, endpoints
	return f.Imp
}

func OrdersImpact() *sysmap.Impact {
	return &sysmap.Impact{SHA: "abcdef1234567890", Branch: "main", Contracts: []sysmap.Touched{{
		Protocol: sysmap.HTTP, Method: "POST", Path: "/orders", File: "app/routes.py", Line: 10,
		Usages: []sysmap.Usage{
			{Repo: "group/web", File: "src/api/orders.ts", Line: 12, Protocol: sysmap.HTTP, Method: "POST", Path: "/orders"},
			{Repo: "group/web", File: "src/x.ts", Line: 4, Protocol: sysmap.HTTP, Method: "POST", Path: "/orders"},
		},
		Possible: []sysmap.Usage{{Repo: "group/admin", File: "a.ts", Line: 1, Protocol: sysmap.HTTP, Method: "POST", Path: "/orders"}},
	}, {
		Protocol: sysmap.Webhook, Method: "POST", Path: "/hooks/orders", Event: "order.paid", Sender: true, File: "app/notify.py", Line: 70,
		Usages: []sysmap.Usage{{Repo: "group/notifier", File: "hooks.py", Line: 3, Protocol: sysmap.Webhook, Receiver: true}},
	}}}
}

func WithSysmap(h *Harness, f *FakeSysmap) {
	h.Cfg.GitLab.Groups = []string{"group"}
	h.Deps.SystemMap = f
}
