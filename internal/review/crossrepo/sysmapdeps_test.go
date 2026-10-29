package crossrepo

import (
	"reflect"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/jira"
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
)

func TestMergeOrderWording(t *testing.T) {
	ref := "g/p!1"
	cases := []struct {
		role sysmap.DepRole
		act  otherAct
		rel  string
		ord  mergeOrder
	}{
		{sysmap.RoleProvidesNew, actAdds, "начинает использовать X, который добавляет этот MR", orderThisFirst},
		{roleProvidesChanged, actRemoves, "перестаёт использовать X, который меняет этот MR", orderOtherFirst},
		{roleProvidesChanged, actChanges, "меняет использование X, который меняет этот MR", orderExpand},
		{sysmap.RoleConsumesNew, actAdds, "добавляет X, который начинает использовать этот MR", orderOtherFirst},
		{sysmap.RoleConsumesNew, actRemoves, "убирает X, который начинает использовать этот MR", orderConflict},
		{sysmap.RoleConsumesDropped, actRemoves, "убирает X, который перестаёт использовать этот MR", orderThisFirst},
		{sysmap.RoleConsumesDropped, actAdds, "добавляет X, который перестаёт использовать этот MR", orderNone},
	}
	for _, c := range cases {
		rel, ord := relation(c.role, c.act, "X")
		if rel != c.rel || ord != c.ord {
			t.Errorf("%s/%d: %q %d", c.role, c.act, rel, ord)
		}
	}
	if s := orderText(orderThisFirst, ref); s != "Порядок: сначала этот MR, потом g/p!1." {
		t.Error(s)
	}
	if s := orderText(orderOtherFirst, ref); s != "Порядок: сначала g/p!1, потом этот MR." {
		t.Error(s)
	}
	if s := orderText(orderExpand, ref); !strings.Contains(s, "expand/contract") {
		t.Error(s)
	}
	if s := orderText(orderConflict, ref); !strings.Contains(s, "Конфликт") {
		t.Error(s)
	}
	mk := func(role sysmap.DepRole, act otherAct) depLink {
		return depLink{check: &depCheck{contract: sysmap.DepContract{Role: role}}, act: act}
	}
	if o := combineOrder([]depLink{mk(sysmap.RoleConsumesNew, actAdds), mk(sysmap.RoleConsumesDropped, actAdds)}); o != orderOtherFirst {
		t.Errorf("one clear order: %d", o)
	}
	if o := combineOrder([]depLink{mk(sysmap.RoleConsumesNew, actAdds), mk(sysmap.RoleProvidesNew, actAdds)}); o != orderExpand {
		t.Errorf("both directions: %d", o)
	}
	if o := combineOrder([]depLink{mk(sysmap.RoleConsumesNew, actAdds), mk(sysmap.RoleConsumesNew, actRemoves)}); o != orderConflict {
		t.Errorf("conflict: %d", o)
	}
}

func TestSharedTask(t *testing.T) {
	is := func(key, parent, epic string) *jira.Issue {
		i := &jira.Issue{Key: key, EpicKey: epic}
		if parent != "" {
			i.Parent = &jira.IssueBrief{Key: parent}
		}
		return i
	}
	for _, c := range []struct {
		a, b      *jira.Issue
		kind, key string
	}{
		{is("CTX-1", "CTX-10", "CTX-100"), is("CTX-2", "CTX-10", "CTX-100"), "общая стори", "CTX-10"},
		{is("CTX-1", "CTX-10", "CTX-100"), is("CTX-2", "CTX-11", "CTX-100"), "общий эпик", "CTX-100"},
		{is("CTX-1", "CTX-100", ""), is("CTX-2", "CTX-100", "CTX-100"), "общий эпик", "CTX-100"}, // next-gen: the parent is the epic
		{is("CTX-1", "", ""), is("CTX-1", "", ""), "общая задача", "CTX-1"},
		{is("CTX-1", "CTX-10", ""), is("CTX-2", "CTX-11", ""), "", ""},
	} {
		kind, key := sharedTask([]*jira.Issue{c.a}, []*jira.Issue{c.b})
		if kind != c.kind || key != c.key {
			t.Errorf("%+v / %+v: %q %q", c.a, c.b, kind, key)
		}
	}
}

func TestContractRegexp(t *testing.T) {
	re := contractRegexp(sysmap.DepContract{Protocol: sysmap.HTTP, Method: "GET", Path: "/api/v1/orders/{id}/history"})
	for line, want := range map[string]bool{
		"fetch(`${API}/orders/${id}/history`)":             true,
		"  /orders/{orderId}/history:":                     true,
		"router.get('/api/v1/orders/:id/history/', h)":     true,
		`requests.get(f"{BASE}/orders/{oid}/history?x=1")`: true,
		"get('/orders/42/history')":                        true,
		"get('/orders/history/x')":                         false,
		"get('/orders/{id}/historyx')":                     false,
		"get('/orders/abc/history')":                       false,
	} {
		if re.MatchString(line) != want {
			t.Errorf("%q: want %v", line, want)
		}
	}
	ev := contractRegexp(sysmap.DepContract{Protocol: sysmap.SSE, Event: "order.updated"})
	if !ev.MatchString(`es.addEventListener("order.updated", f)`) || ev.MatchString(`"order.updatedx"`) || ev.MatchString("order_updated") {
		t.Error("event regexp")
	}
	if contractRegexp(sysmap.DepContract{Protocol: sysmap.HTTP, Path: "/{id}"}) != nil {
		t.Error("a generic path must not be matched")
	}
}

func TestParseDiffLines(t *testing.T) {
	diff := "diff --git a/a.ts b/a.ts\n--- a/a.ts\n+++ b/a.ts\n@@ -10,4 +10,5 @@\n x\n-old1\n-old2\n+new1\n+new2\n+new3\n y\n" +
		"@@ -30 +31,2 @@\n+n4\n z\n" +
		"diff --git a/gone.ts b/gone.ts\n--- a/gone.ts\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-a\n-b\n" +
		"diff --git a/new.ts b/new.ts\n--- /dev/null\n+++ b/new.ts\n@@ -0,0 +1 @@\n+--- not a header\n"
	got := parseDiffLines(diff)
	if !reflect.DeepEqual(got.added, map[string][]sysmap.LineRange{"a.ts": {{From: 11, To: 13}, {From: 31, To: 31}}, "new.ts": {{From: 1, To: 1}}}) {
		t.Errorf("added %v", got.added)
	}
	if !reflect.DeepEqual(got.removed, map[string][]sysmap.LineRange{"a.ts": {{From: 11, To: 12}}, "gone.ts": {{From: 1, To: 2}}}) {
		t.Errorf("removed %v", got.removed)
	}
	if !reflect.DeepEqual(got.changed, []string{"a.ts", "new.ts"}) || !reflect.DeepEqual(got.deleted, []string{"gone.ts"}) {
		t.Errorf("changed %v deleted %v", got.changed, got.deleted)
	}
}
