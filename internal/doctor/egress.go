package doctor

import (
	"bufio"
	"context"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"
)

// egressTarget is what the sandbox must reach through the proxy.
const egressTarget = "api.anthropic.com:443"

// EgressInput is what the egress checks need.
type EgressInput struct {
	// Proxy: sandbox.egress_proxy; Allow: sandbox.egress_allow.
	Proxy string
	Allow []string
	// Dial sends CONNECT api.anthropic.com:443 to the proxy at host:port
	// and returns the status line (nil = a real TCP connection).
	Dial func(ctx context.Context, hostport string) (string, error)
}

func dialConnect(ctx context.Context, hostport string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", hostport)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	// A failed write shows up as a failed read below.
	_, _ = c.Write([]byte("CONNECT " + egressTarget + " HTTP/1.1\r\nHost: " + egressTarget + "\r\n\r\n"))
	line, err := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line), err
}

// Egress checks the egress allowlist and the proxy.
func Egress(in EgressInput) Provider {
	return ProviderFunc(func(ctx context.Context) []Result {
		if in.Proxy == "" {
			return []Result{{ID: "egress.proxy", Group: "Egress", Title: "прокси песочницы", Status: Skip, Detail: "sandbox.egress_proxy не задан"}}
		}
		if in.Dial == nil {
			in.Dial = dialConnect
		}
		allow := Result{ID: "egress.allow", Group: "Egress", Title: "разрешённые хосты"}
		if slices.Contains(in.Allow, "api.anthropic.com") {
			allow.Status, allow.Detail = OK, "api.anthropic.com в sandbox.egress_allow"
		} else {
			allow.Status, allow.Detail, allow.Action = Fail, "api.anthropic.com не в sandbox.egress_allow", "добавьте api.anthropic.com в sandbox.egress_allow"
		}
		px := Result{ID: "egress.proxy", Group: "Egress", Title: "прокси песочницы"}
		host := in.Proxy
		if u, err := url.Parse(in.Proxy); err == nil && u.Host != "" {
			host = u.Host
		}
		line, err := in.Dial(ctx, host)
		switch {
		case err != nil:
			px.Status, px.Detail = Warn, host+" недоступен отсюда: "+oneLine(err.Error())
			px.Action = "прокси виден только из сети compose: docker compose up -d и docker compose exec reviewbot reviewbot doctor -config /etc/reviewbot/config.yaml"
		case strings.Contains(line, " 200"):
			px.Status, px.Detail = OK, "CONNECT "+egressTarget+" через "+host
		default:
			px.Status, px.Detail = Fail, "прокси отказал: "+oneLine(line)
			px.Action = "проверьте sandbox.egress_allow в конфиге, смонтированном в egress, и перезапустите egress"
		}
		return []Result{allow, px}
	})
}
