package sysmap

import (
	"strings"
	"testing"
)

func TestNormalizePath(t *testing.T) {
	cases := map[string]string{
		"/orders/{id}":                         "/orders/{id}",
		"/orders/:id":                          "/orders/{id}",
		"/orders/<int:id>":                     "/orders/{id}",
		"/orders/<id>/items/":                  "/orders/{id}/items",
		"`${ORDERS_URL}/orders/${id}`":         "/orders/{id}",
		"${process.env.API}/orders/${o.id}":    "/orders/{id}",
		"/orders/%s":                           "/orders/{}",
		"/orders/%(order_id)s":                 "/orders/{order_id}",
		"/orders/{order_id}?expand=items":      "/orders/{order_id}",
		"/orders/{0}":                          "/orders/{}",
		"/users/{id:int}":                      "/users/{id}",
		"https://orders.internal/api/v1/x/":    "/api/v1/x",
		"http://orders.svc:8080":               "/",
		"^orders/(?P<pk>[0-9]+)/$":             "/orders/{pk}",
		"^orders/?$":                           "/orders",
		"orders/{id}":                          "/orders/{id}",
		"/pages/[slug]":                        "/pages/{slug}",
		"/orders/' + id + '":                   "/orders/{}",
		"/files/*":                             "/files/{}",
		"/orders/{{id}}":                       "/orders/{id}",
		"ORDERS_URL/orders":                    "/orders",
		"/report-{id}.pdf":                     "/report-{id}.pdf",
		"":                                     "",
		"  /a//b/  ":                           "/a/b",
		"config('services.orders.url').'/x/1'": "/x/1",
	}
	for in, want := range cases {
		if got := NormalizePath(in); got != want {
			t.Errorf("NormalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPathKeyAndPrefix(t *testing.T) {
	if k := PathKey("/Orders/{id}/items/{item_id}"); k != "/orders/{}/items/{}" {
		t.Errorf("PathKey = %q", k)
	}
	if k := PathKey("/report-{id}.pdf"); k != "/{}" {
		t.Errorf("PathKey partial = %q", k)
	}
	for in, want := range map[string]string{
		"/api/orders":      "/orders",
		"/api/v1/orders":   "/orders",
		"/v2/orders/{}":    "/orders/{}",
		"/orders":          "/orders",
		"/apis/orders":     "/apis/orders",
		"/api":             "/",
		"/version/1":       "/version/1",
		"/api/v10/x":       "/x",
		"/api/internal/v1": "/internal/v1",
	} {
		if got := StripPrefix(in); got != want {
			t.Errorf("StripPrefix(%q) = %q, want %q", in, got, want)
		}
	}
	if !genericKey("/") || !genericKey("/{}/{}") || genericKey("/orders/{}") {
		t.Error("genericKey")
	}
}

func TestNormalizeMethodAndEvent(t *testing.T) {
	if NormalizeMethod(" post ") != "POST" || NormalizeMethod("ANY") != "" || NormalizeMethod("*") != "" {
		t.Error("NormalizeMethod")
	}
	if NormalizeEvent("'order.paid'") != "order.paid" || NormalizeEvent("bad event") != "" || NormalizeEvent("<x>") != "" {
		t.Error("NormalizeEvent")
	}
}

func TestContainsToken(t *testing.T) {
	if !containsToken("process.env.ORDERS_URL", "ORDERS_URL") {
		t.Error("token after dot")
	}
	if containsToken("ORDERS_URL_V2", "ORDERS_URL") {
		t.Error("glued token matched")
	}
	if !containsToken("http://orders.internal/x", "orders.internal") {
		t.Error("host")
	}
	if !containsToken("${orders_url}", "ORDERS_URL") {
		t.Error("case-insensitive")
	}
}

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"oauth.yandex.ru":                       "oauth.yandex.ru",
		" https://OAuth.Yandex.ru/token?x=1 ":   "oauth.yandex.ru",
		"wss://ws.pusher.com:443/app":           "ws.pusher.com",
		"https://user:secret@s3.amazonaws.com/": "s3.amazonaws.com",
		"`https://b24.example-corp.ru/rest/`":   "b24.example-corp.ru",
		"api.bitrix24.ru.":                      "api.bitrix24.ru",
		"xn--80ak6aa92e.xn--p1ai":               "xn--80ak6aa92e.xn--p1ai",
		"":                                      "",
		"localhost":                             "",
		"http://localhost:8080/x":               "",
		"orders":                                "",
		"10.0.0.1":                              "",
		"http://[::1]/x":                        "",
		"orders.internal":                       "",
		"orders.default.svc.cluster.local":      "",
		"app.local":                             "",
		"nas.lan":                               "",
		"https://${HOST}/x":                     "",
		"config('services.orders.url')":         "",
		"process.env.API_URL":                   "",
		"-bad.example.com":                      "",
		"a..b.com":                              "",
		"host.c0m":                              "",
		strings.Repeat("a", 250) + ".com":       "",
	} {
		if got := NormalizeHost(in); got != want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestURLHost(t *testing.T) {
	for _, c := range []struct{ raw, base, want string }{
		{"https://oauth.yandex.ru/token", "", "oauth.yandex.ru"},
		{"`${BASE}/x`", "'https://api.telegram.org/bot'", "api.telegram.org"},
		{"fetch(\"wss://ws.pusher.com/app\")", "", "ws.pusher.com"},
		{"http://localhost:3000/a https://api.example.org/b", "", "api.example.org"},
		{"`${ORDERS_URL}/orders`", "ORDERS_URL", ""},
		{"/orders", "oauth.yandex.ru", ""}, // a bare base is not trusted
		{"http://orders/api", "", ""},
		{"ftp://files.example.org/x", "", ""},
	} {
		if got := urlHost(c.raw, c.base); got != c.want {
			t.Errorf("urlHost(%q, %q) = %q, want %q", c.raw, c.base, got, c.want)
		}
	}
}

func TestOwnDomain(t *testing.T) {
	for in, want := range map[string]string{
		"https://gitlab.example.com/":        "example.com",
		"https://git.dev.example.co.uk:8443": "dev.example.co.uk",
		"https://gitlab.com":                 "gitlab.com", // two labels: the host itself
		"http://localhost:8080":              "",
		"http://10.0.0.5":                    "",
		"":                                   "",
	} {
		if got := OwnDomain(in); got != want {
			t.Errorf("OwnDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEventKey(t *testing.T) {
	same := [][]string{
		{"chat.update", "chat_update", "Chat-Update", "chat update", "chat..update"},
		{"git-index.done", "git_index_done", "GIT.INDEX.DONE"},
		{"presale-run", "presale_run"},
	}
	for _, g := range same {
		for _, e := range g[1:] {
			if EventKey(e) != EventKey(g[0]) {
				t.Errorf("%q and %q must match: %q %q", g[0], e, EventKey(g[0]), EventKey(e))
			}
		}
	}
	for _, d := range [][2]string{{"chat.update", "chat.updated"}, {"llm.usage", "usage"}, {"order:paid", "order.paid"}, {"oauth_exchange", "bitrix.oauth.exchange"}} {
		if EventKey(d[0]) == EventKey(d[1]) {
			t.Errorf("%q and %q must stay different", d[0], d[1])
		}
	}
	if EventKey("") != "" || EventKey("._-") != "" {
		t.Error("empty key")
	}
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"usage", "llm.usage", true}, {"llm_usage", "usage", true}, {"oauth.exchange", "bitrix.oauth.exchange", true},
		{"done", "git-index.done", true}, {"usage", "usage", false}, {"sage", "llm.usage", false}, {"llm", "llm.usage", false}, {"", "x", false},
	} {
		if got := eventSuffix(c.a, c.b); got != c.want {
			t.Errorf("eventSuffix(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
