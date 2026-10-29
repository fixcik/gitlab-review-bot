package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type echoArgs struct {
	Text string `json:"text"`
}

func testTools() []Tool {
	return []Tool{
		{Name: "echo", Description: "Echo text.", ReadOnly: true,
			InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"additionalProperties":false}`),
			Handler: func(_ context.Context, args json.RawMessage) (string, error) {
				var a echoArgs
				if err := DecodeArgs(args, &a); err != nil {
					return "", err
				}
				return "echo: " + a.Text, nil
			}},
		{Name: "fail", Handler: func(context.Context, json.RawMessage) (string, error) { return "", errors.New("nope: fix the path") }},
		{Name: "boom", Handler: func(context.Context, json.RawMessage) (string, error) { panic("secret detail") }},
		{Name: "big", Handler: func(context.Context, json.RawMessage) (string, error) { return strings.Repeat("я", 5000), nil }},
	}
}

type fixture struct {
	srv   *Server
	ts    *httptest.Server
	clk   *clock
	ss    *Session
	token string
	calls []CallInfo
	mu    sync.Mutex
}

func newFixture(t *testing.T, o Options) *fixture {
	t.Helper()
	f := &fixture{clk: &clock{t: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}}
	o.Now = f.clk.now
	o.OnCall = func(_ context.Context, c CallInfo) {
		f.mu.Lock()
		f.calls = append(f.calls, c)
		f.mu.Unlock()
	}
	f.srv = New(o)
	f.ts = httptest.NewServer(f.srv.Handler())
	t.Cleanup(f.ts.Close)
	var err error
	f.ss, f.token, err = f.srv.Open(Spec{ProjectID: 42, MRIID: 7, HeadSHA: "abc", RunID: "run1", Tools: testTools(), TTL: time.Hour,
		Instructions: "Use submit_finding."})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) post(t *testing.T, token, body string, hdr ...string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.ts.URL+Path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

type rpcOut struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func (f *fixture) rpc(t *testing.T, body string) rpcOut {
	t.Helper()
	resp, b := f.post(t, f.token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type %q", ct)
	}
	var out rpcOut
	if err := json.Unmarshal([]byte(b), &out); err != nil {
		t.Fatalf("response %q: %v", b, err)
	}
	return out
}

func (f *fixture) call(t *testing.T, name, args string) callResult {
	t.Helper()
	out := f.rpc(t, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
	if out.Error != nil {
		t.Fatalf("rpc error: %+v", out.Error)
	}
	var cr callResult
	if err := json.Unmarshal(out.Result, &cr); err != nil {
		t.Fatal(err)
	}
	return cr
}

func TestInitializeVersionNegotiation(t *testing.T) {
	f := newFixture(t, Options{Version: "1.2.3"})
	for _, tc := range []struct{ asked, want string }{
		{"2025-06-18", "2025-06-18"},
		{"2025-03-26", "2025-03-26"},
		{"2024-11-05", "2024-11-05"},
		{"2099-01-01", LatestProtocolVersion},
		{"", LatestProtocolVersion},
	} {
		out := f.rpc(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+tc.asked+`","capabilities":{},"clientInfo":{"name":"claude-code","version":"2.1"}}}`)
		var res initializeResult
		if out.Error != nil || json.Unmarshal(out.Result, &res) != nil {
			t.Fatalf("initialize: %+v %s", out.Error, out.Result)
		}
		if res.ProtocolVersion != tc.want {
			t.Errorf("asked %q: got %q, want %q", tc.asked, res.ProtocolVersion, tc.want)
		}
		if res.ServerInfo.Name != ServerName || res.ServerInfo.Version != "1.2.3" || res.Instructions != "Use submit_finding." {
			t.Errorf("server info %+v", res)
		}
		if _, ok := res.Capabilities["tools"]; !ok {
			t.Errorf("capabilities %v", res.Capabilities)
		}
		if string(out.ID) != "1" {
			t.Errorf("id %s", out.ID)
		}
	}
}

func TestNotificationsAndPing(t *testing.T) {
	f := newFixture(t, Options{})
	resp, body := f.post(t, f.token, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if resp.StatusCode != http.StatusAccepted || body != "" {
		t.Errorf("notification: %d %q", resp.StatusCode, body)
	}
	out := f.rpc(t, `{"jsonrpc":"2.0","id":"p-1","method":"ping"}`)
	if out.Error != nil || string(out.Result) != "{}" || string(out.ID) != `"p-1"` {
		t.Errorf("ping: %+v %s %s", out.Error, out.Result, out.ID)
	}
	out = f.rpc(t, `{"jsonrpc":"2.0","id":2,"method":"resources/list"}`)
	if out.Error == nil || out.Error.Code != codeMethodNotFound {
		t.Errorf("unknown method: %+v", out.Error)
	}
}

func TestToolsList(t *testing.T) {
	f := newFixture(t, Options{})
	out := f.rpc(t, `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`)
	var res toolsListResult
	if err := json.Unmarshal(out.Result, &res); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "echo,fail,boom,big" {
		t.Errorf("tools %v", names)
	}
	if !res.Tools[0].Annotations.ReadOnlyHint || res.Tools[1].Annotations.ReadOnlyHint {
		t.Errorf("annotations %+v %+v", res.Tools[0].Annotations, res.Tools[1].Annotations)
	}
	if !strings.Contains(string(res.Tools[1].InputSchema), `"type":"object"`) {
		t.Errorf("default schema %s", res.Tools[1].InputSchema)
	}
}

func TestToolsCall(t *testing.T) {
	f := newFixture(t, Options{MaxResponseBytes: 2048})
	if cr := f.call(t, "echo", `{"text":"hi"}`); cr.IsError || cr.Content[0].Text != "echo: hi" || cr.Content[0].Type != "text" {
		t.Errorf("echo: %+v", cr)
	}
	if cr := f.call(t, "echo", `{"text":1}`); !cr.IsError || !strings.Contains(cr.Content[0].Text, `"text" must be a string`) {
		t.Errorf("bad type: %+v", cr)
	}
	if cr := f.call(t, "echo", `{"other":"x"}`); !cr.IsError || !strings.Contains(cr.Content[0].Text, "unknown field") {
		t.Errorf("unknown field: %+v", cr)
	}
	if cr := f.call(t, "fail", `{}`); !cr.IsError || cr.Content[0].Text != "nope: fix the path" {
		t.Errorf("fail: %+v", cr)
	}
	if cr := f.call(t, "boom", `{}`); !cr.IsError || strings.Contains(cr.Content[0].Text, "secret") {
		t.Errorf("panic must become a generic error: %+v", cr)
	}
	cr := f.call(t, "big", `null`)
	if len(cr.Content[0].Text) > 2048 || !strings.Contains(cr.Content[0].Text, "truncated") || !json.Valid([]byte(`"`+strings.ReplaceAll(cr.Content[0].Text, "\n", `\n`)+`"`)) {
		t.Errorf("big: %d bytes", len(cr.Content[0].Text))
	}
	out := f.rpc(t, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"nosuch","arguments":{}}}`)
	if out.Error == nil || out.Error.Code != codeInvalidParams {
		t.Errorf("unknown tool: %+v", out.Error)
	}
	out = f.rpc(t, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"echo","arguments":[1]}}`)
	if out.Error == nil || out.Error.Code != codeInvalidParams {
		t.Errorf("array arguments: %+v", out.Error)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 6 { // unknown tool and bad params are not tool calls
		t.Fatalf("observed %d calls: %+v", len(f.calls), f.calls)
	}
	if c := f.calls[0]; c.Tool != "echo" || c.Status != StatusOK || c.ProjectID != 42 || c.MRIID != 7 || c.RunID != "run1" || c.ArgsPreview != `{"text":"hi"}` {
		t.Errorf("call info %+v", c)
	}
	if f.calls[3].Status != StatusError {
		t.Errorf("fail status %+v", f.calls[3])
	}
}

func TestMalformedRequests(t *testing.T) {
	f := newFixture(t, Options{MaxRequestBytes: 512})
	out := f.rpc(t, `{not json`)
	if out.Error == nil || out.Error.Code != codeParseError {
		t.Errorf("parse error: %+v", out.Error)
	}
	out = f.rpc(t, `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`)
	if out.Error == nil || out.Error.Code != codeInvalidRequest {
		t.Errorf("batch: %+v", out.Error)
	}
	out = f.rpc(t, `{"jsonrpc":"1.0","id":1,"method":"ping"}`)
	if out.Error == nil || out.Error.Code != codeInvalidRequest {
		t.Errorf("version: %+v", out.Error)
	}
	resp, _ := f.post(t, f.token, `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"`+strings.Repeat("a", 1024)+`"}}`)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("too large: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, f.ts.URL+Path, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "text/plain")
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("content type: %d", r2.StatusCode)
	}
	resp, _ = f.post(t, f.token, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "Origin", "http://evil.example")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("origin: %d", resp.StatusCode)
	}
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		req, _ := http.NewRequest(m, f.ts.URL+Path, nil)
		req.Header.Set("Authorization", "Bearer "+f.token)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusMethodNotAllowed || r.Header.Get("Allow") != http.MethodPost {
			t.Errorf("%s: %d", m, r.StatusCode)
		}
	}
}

func TestAuth(t *testing.T) {
	f := newFixture(t, Options{})
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	for name, tok := range map[string]string{
		"missing": "", "wrong": strings.Repeat("0", 64), "prefix": f.token[:63], "longer": f.token + "0",
	} {
		resp, body := f.post(t, tok, ping)
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s token: %d %q", name, resp.StatusCode, body)
		}
		if strings.Contains(body, f.token) {
			t.Error("token echoed")
		}
	}
	req, _ := http.NewRequest(http.MethodPost, f.ts.URL+Path, strings.NewReader(ping))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+f.token)
	r, _ := http.DefaultClient.Do(req)
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("basic auth: %d", r.StatusCode)
	}
	if resp, _ := f.post(t, f.token, ping); resp.StatusCode != http.StatusOK {
		t.Errorf("valid token: %d", resp.StatusCode)
	}
	// Unauthenticated requests never reach the GET/405 or body handling.
	req, _ = http.NewRequest(http.MethodGet, f.ts.URL+Path, nil)
	r, _ = http.DefaultClient.Do(req)
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET without token: %d", r.StatusCode)
	}
	if len(f.token) != 64 {
		t.Errorf("token length %d", len(f.token))
	}
	if _, tok2, _ := f.srv.Open(Spec{TTL: time.Minute}); tok2 == f.token {
		t.Error("tokens repeat")
	}
}

func TestSessionCloseAndExpiry(t *testing.T) {
	f := newFixture(t, Options{})
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	f.ss.Close()
	f.ss.Close() // idempotent
	if resp, _ := f.post(t, f.token, ping); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("closed session: %d", resp.StatusCode)
	}
	if res, _ := f.ss.Call(context.Background(), "echo", nil); !res.IsError {
		t.Errorf("direct call after close: %+v", res)
	}
	ss, tok, err := f.srv.Open(Spec{Tools: testTools(), TTL: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	f.clk.add(9 * time.Minute)
	if resp, _ := f.post(t, tok, ping); resp.StatusCode != http.StatusOK {
		t.Errorf("before expiry: %d", resp.StatusCode)
	}
	f.clk.add(time.Minute)
	if resp, _ := f.post(t, tok, ping); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expired: %d", resp.StatusCode)
	}
	if f.srv.Sessions() != 0 {
		t.Errorf("sessions left: %d", f.srv.Sessions())
	}
	_ = ss
	if _, _, err := f.srv.Open(Spec{}); err == nil {
		t.Error("session without TTL accepted")
	}
	if _, _, err := f.srv.Open(Spec{TTL: time.Minute, Tools: []Tool{{Name: "Bad-Name", Handler: testTools()[0].Handler}}}); err == nil {
		t.Error("invalid tool name accepted")
	}
	if _, _, err := f.srv.Open(Spec{TTL: time.Minute, Tools: []Tool{testTools()[0], testTools()[0]}}); err == nil {
		t.Error("duplicate tool accepted")
	}
}

func TestCallCap(t *testing.T) {
	f := newFixture(t, Options{MaxCalls: 3})
	for i := 0; i < 3; i++ {
		if cr := f.call(t, "echo", `{"text":"x"}`); cr.IsError {
			t.Fatalf("call %d: %+v", i, cr)
		}
	}
	cr := f.call(t, "echo", `{"text":"x"}`)
	if !cr.IsError || !strings.Contains(cr.Content[0].Text, "limit") || !strings.Contains(cr.Content[0].Text, "final JSON") {
		t.Errorf("over the cap: %+v", cr)
	}
	if f.ss.Calls() != 4 {
		t.Errorf("calls %d", f.ss.Calls())
	}
	f.mu.Lock()
	last := f.calls[len(f.calls)-1]
	f.mu.Unlock()
	if last.Status != StatusLimit {
		t.Errorf("status %q", last.Status)
	}
}

func TestCallContext(t *testing.T) {
	type key struct{}
	srv := New(Options{CallTimeout: 50 * time.Millisecond})
	jobCtx := context.WithValue(context.Background(), key{}, "job")
	var sawJob bool
	ss, _, err := srv.Open(Spec{Ctx: jobCtx, TTL: time.Minute, Tools: []Tool{{Name: "slow", Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
		sawJob = ctx.Value(key{}) == "job"
		<-ctx.Done()
		return "", ctx.Err()
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := ss.Call(context.Background(), "slow", nil)
	if !res.IsError || !sawJob {
		t.Errorf("timeout: %+v, job ctx %v", res, sawJob)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	ss.Call(ctx, "slow", nil)
	if time.Since(start) > 40*time.Millisecond {
		t.Error("request cancellation does not stop the call")
	}
}

func TestDecodeArgs(t *testing.T) {
	var a struct {
		N int    `json:"n"`
		S string `json:"s"`
	}
	if err := DecodeArgs(nil, &a); err != nil {
		t.Errorf("empty: %v", err)
	}
	if err := DecodeArgs(json.RawMessage(`{"n":"x"}`), &a); err == nil || !strings.Contains(err.Error(), `"n" must be an integer`) {
		t.Errorf("type: %v", err)
	}
	if err := DecodeArgs(json.RawMessage(`{"n":1}{"n":2}`), &a); err == nil {
		t.Error("trailing data accepted")
	}
}

// Connected turns true with the first authenticated request only; a wrong
// token or a browser request does not count.
func TestSessionConnected(t *testing.T) {
	f := newFixture(t, Options{})
	if f.ss.Connected() {
		t.Fatal("connected before any request")
	}
	f.post(t, "wrong", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	f.post(t, f.token, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, "Origin", "http://evil")
	if f.ss.Connected() {
		t.Fatal("connected after refused requests")
	}
	f.post(t, f.token, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if !f.ss.Connected() {
		t.Fatal("not connected after initialize")
	}
	var nilSess *Session
	if nilSess.Connected() {
		t.Error("nil session connected")
	}
}

// Fallback opens the breaker for FallbackCooldown and reports the reason.
func TestFallbackCooldown(t *testing.T) {
	var reasons []string
	f := newFixture(t, Options{FallbackCooldown: 10 * time.Minute, OnFallback: func(r string) { reasons = append(reasons, r) }})
	if on, _ := f.srv.Degraded(); on {
		t.Fatal("degraded at start")
	}
	f.srv.Fallback("not_connected")
	on, until := f.srv.Degraded()
	if !on || !until.Equal(f.clk.now().Add(10*time.Minute)) {
		t.Fatalf("after fallback: %v %v", on, until)
	}
	f.clk.add(10 * time.Minute)
	if on, _ := f.srv.Degraded(); on {
		t.Error("still degraded after the cooldown")
	}
	if strings.Join(reasons, ",") != "not_connected" {
		t.Errorf("reasons %v", reasons)
	}
	if New(Options{}).o.FallbackCooldown != DefaultFallbackCooldown {
		t.Error("default cooldown")
	}
	var nilSrv *Server
	if on, _ := nilSrv.Degraded(); on {
		t.Error("nil server degraded")
	}
}
