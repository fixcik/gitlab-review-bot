package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Type errors name the argument and the expected JSON kind.
func TestDecodeArgsKinds(t *testing.T) {
	var a struct {
		B bool              `json:"b"`
		L []string          `json:"l"`
		O struct{ X int }   `json:"o"`
		M map[string]string `json:"m"`
		P *struct{}         `json:"p"`
		F float64           `json:"f"`
		S string            `json:"s"`
	}
	for args, want := range map[string]string{
		`{"b":"yes"}`: `argument "b" must be a boolean`,
		`{"l":"x"}`:   `argument "l" must be an array`,
		`{"o":1}`:     `argument "o" must be an object`,
		`{"m":[]}`:    `argument "m" must be an object`,
		`{"p":true}`:  `argument "p" must be an object`,
		`{"f":"1"}`:   `argument "f" must be of another type`,
		`{"s":1}`:     `argument "s" must be a string`,
		`{"zz":1}`:    `invalid arguments: unknown field "zz"`,
	} {
		if err := DecodeArgs(json.RawMessage(args), &a); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", args, err, want)
		}
	}
}

// Malformed initialize and tools/call params are invalid-params errors.
func TestDispatchInvalidParams(t *testing.T) {
	f := newFixture(t, Options{})
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":[1]}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"echo"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"arguments":{}}}`,
	} {
		out := f.rpc(t, body)
		if out.Error == nil || out.Error.Code != codeInvalidParams || string(out.ID) != "1" {
			t.Errorf("%s: %+v", body, out)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls = %+v", f.calls)
	}
}

// A response that cannot be encoded (a tool registered with a broken input
// schema) is an internal error, not a broken body.
func TestUnencodableResponse(t *testing.T) {
	f := newFixture(t, Options{})
	_, token, err := f.srv.Open(Spec{TTL: time.Hour, Tools: []Tool{{Name: "bad", InputSchema: json.RawMessage(`{`),
		Handler: func(context.Context, json.RawMessage) (string, error) { return "", nil }}}})
	if err != nil {
		t.Fatal(err)
	}
	f.token = token
	out := f.rpc(t, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	if out.Error == nil || out.Error.Code != codeInternalError || string(out.ID) != "null" {
		t.Fatalf("out = %+v", out)
	}
}

// Expired sessions are dropped when the next one opens; closing a nil
// session is a no-op.
func TestExpiredSessionsPruned(t *testing.T) {
	f := newFixture(t, Options{})
	if f.srv.Sessions() != 1 {
		t.Fatalf("sessions = %d", f.srv.Sessions())
	}
	f.clk.add(2 * time.Hour)
	if _, _, err := f.srv.Open(Spec{TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if f.srv.Sessions() != 1 {
		t.Fatalf("expired session kept: %d", f.srv.Sessions())
	}
	var ss *Session
	ss.Close()
}

// An unknown tool name is echoed capped at 64 bytes, cut on a rune
// boundary; ToolNames lists the tools in registration order.
func TestUnknownToolNamePreview(t *testing.T) {
	f := newFixture(t, Options{})
	name := "x" + strings.Repeat("я", 40)
	out := f.rpc(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+name+`"}}`)
	want := "unknown tool: " + name[:63] + "…"
	if out.Error == nil || out.Error.Message != want || !utf8.ValidString(out.Error.Message) {
		t.Fatalf("error = %+v, want %q", out.Error, want)
	}
	if got := strings.Join(f.ss.ToolNames(), ","); got != "echo,fail,boom,big" {
		t.Fatalf("ToolNames = %s", got)
	}
}

// initialize works without params and announces a fixed tool list;
// arguments that are not an object are refused; answers are never cached.
func TestInitializeWithoutParamsAndArgumentShape(t *testing.T) {
	f := newFixture(t, Options{})
	resp, body := f.post(t, f.token, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	var out struct {
		Result struct {
			Capabilities struct {
				Tools struct {
					ListChanged *bool `json:"listChanged"`
				} `json:"tools"`
			} `json:"capabilities"`
		} `json:"result"`
		Error *rpcError `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Error != nil {
		t.Fatalf("%s: %v", body, err)
	}
	if lc := out.Result.Capabilities.Tools.ListChanged; lc == nil || *lc {
		t.Errorf("listChanged = %v", lc)
	}
	for _, args := range []string{`1`, `"x"`, `[1]`} {
		out := f.rpc(t, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":`+args+`}}`)
		if out.Error == nil || out.Error.Message != "arguments must be an object" {
			t.Errorf("arguments %s: %+v", args, out.Error)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls = %+v", f.calls)
	}
}
