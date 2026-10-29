package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Handler serves the MCP endpoint at Path.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(Path, s.serveMCP)
	return mux
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return ""
	}
	return strings.TrimSpace(h[len(p):])
}

func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request) {
	// Browsers send Origin; the CLI does not. Refusing it rules out DNS
	// rebinding from a page (the transport specification asks servers to
	// validate Origin).
	if r.Header.Get("Origin") != "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ss := s.lookup(bearer(r))
	if ss == nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="reviewbot-mcp"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ss.seen.Store(true)
	if r.Method != http.MethodPost {
		// No server-initiated SSE stream (GET) and no client session
		// termination (DELETE): the token's lifetime is the run's.
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.Contains(strings.ToLower(ct), "application/json") {
		http.Error(w, "content type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.o.MaxRequestBytes))
	if err != nil {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	if strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		writeJSON(w, errorResponse(nil, codeInvalidRequest, "batch requests are not supported"))
		return
	}
	var msg rpcMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		writeJSON(w, errorResponse(nil, codeParseError, "parse error"))
		return
	}
	if msg.JSONRPC != "2.0" {
		writeJSON(w, errorResponse(msg.ID, codeInvalidRequest, `jsonrpc must be "2.0"`))
		return
	}
	if !msg.isRequest() {
		// Notifications (notifications/initialized, cancelled, ...) and
		// responses need no answer.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, s.dispatch(r.Context(), ss, &msg))
}

// errorResponse is a JSON-RPC error answer to id (null when there is none).
func errorResponse(id json.RawMessage, code int, message string) rpcResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
}

func (s *Server) dispatch(ctx context.Context, ss *Session, msg *rpcMessage) rpcResponse {
	resp := rpcResponse{JSONRPC: "2.0", ID: msg.ID}
	switch msg.Method {
	case "initialize":
		var p initializeParams
		if len(msg.Params) > 0 {
			if err := json.Unmarshal(msg.Params, &p); err != nil {
				return errorResponse(msg.ID, codeInvalidParams, "invalid initialize params")
			}
		}
		resp.Result = initializeResult{
			ProtocolVersion: negotiateVersion(p.ProtocolVersion),
			Capabilities:    map[string]any{"tools": map[string]any{"listChanged": false}},
			ServerInfo:      serverInfo{Name: ServerName, Version: s.o.Version},
			Instructions:    ss.spec.Instructions,
		}
	case "ping":
		resp.Result = struct{}{}
	case "tools/list":
		resp.Result = ss.list()
	case "tools/call":
		var p callParams
		if err := json.Unmarshal(msg.Params, &p); err != nil || p.Name == "" {
			return errorResponse(msg.ID, codeInvalidParams, "tools/call needs a tool name")
		}
		if len(p.Arguments) > 0 && !strings.HasPrefix(strings.TrimSpace(string(p.Arguments)), "{") && string(p.Arguments) != "null" {
			return errorResponse(msg.ID, codeInvalidParams, "arguments must be an object")
		}
		res, known := ss.Call(ctx, p.Name, p.Arguments)
		if !known {
			return errorResponse(msg.ID, codeInvalidParams, "unknown tool: "+preview(json.RawMessage(p.Name), 64))
		}
		resp.Result = callResult{Content: []textContent{{Type: "text", Text: res.Text}}, IsError: res.IsError}
	default:
		return errorResponse(msg.ID, codeMethodNotFound, "method not found")
	}
	return resp
}

func (ss *Session) list() toolsListResult {
	out := toolsListResult{Tools: []toolDescriptor{}}
	for _, n := range ss.order { // registration order
		t := ss.tools[n]
		out.Tools = append(out.Tools, toolDescriptor{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema,
			Annotations: &toolAnnotation{ReadOnlyHint: t.ReadOnly, IdempotentHint: t.ReadOnly}})
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(errorResponse(nil, codeInternalError, "internal error"))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}
