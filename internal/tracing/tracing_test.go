package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

// memExporter collects finished spans.
type memExporter struct {
	mu    sync.Mutex
	spans []*SpanData
}

func (m *memExporter) Enqueue(s *SpanData) {
	m.mu.Lock()
	m.spans = append(m.spans, s)
	m.mu.Unlock()
}

func (m *memExporter) all() []*SpanData {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*SpanData(nil), m.spans...)
}

func ratio(f float64) *float64 { return &f }

func TestTraceParent(t *testing.T) {
	good := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	sc, err := ParseTraceParent(good)
	if err != nil {
		t.Fatal(err)
	}
	if sc.TraceID.String() != "4bf92f3577b34da6a3ce929d0e0e4736" || sc.SpanID.String() != "00f067aa0ba902b7" || !sc.Sampled || !sc.Remote {
		t.Fatalf("parsed %+v", sc)
	}
	if got := sc.TraceParent(); got != good {
		t.Fatalf("format = %q", got)
	}
	unsampled, err := ParseTraceParent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00")
	if err != nil || unsampled.Sampled {
		t.Fatalf("unsampled: %+v %v", unsampled, err)
	}
	// A future version may carry more fields.
	if _, err := ParseTraceParent("01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra"); err != nil {
		t.Fatalf("future version: %v", err)
	}
	for _, bad := range []string{
		"", "garbage",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7",      // 3 fields
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-x", // v00 with extra field
		"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",   // forbidden version
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01",   // zero trace id
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",   // zero span id
		"00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01",   // upper case
		"00-4bf92f3577b34da6a3ce929d0e0e473-00f067aa0ba902b7-01",    // short trace id
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902bz-01",   // non-hex
		"0-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",    // short version
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-1",    // short flags
	} {
		if _, err := ParseTraceParent(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if (SpanContext{}).TraceParent() != "" {
		t.Error("invalid context formats")
	}
}

func TestNoopWhenDisabled(t *testing.T) {
	var tr *Tracer
	ctx, sp := tr.Start(context.Background(), "x")
	if sp != nil || SpanFromContext(ctx) != nil {
		t.Fatal("nil tracer created a span")
	}
	// Every span method is nil-safe.
	sp.SetAttr(String("a", "b"))
	sp.SetAttrIfAbsent(Int("n", 1))
	sp.AddEvent("e")
	sp.RecordError(errors.New("x"), "")
	sp.SetStatus(StatusError, "x")
	sp.SetName("y")
	sp.End()
	if sp.IsRecording() || sp.TraceID() != "" || sp.SpanContext().IsValid() {
		t.Fatal("nil span reports data")
	}
	// The package-level Start never starts a trace by itself.
	ctx2, child := Start(context.Background(), "child")
	if child != nil || ctx2 != context.Background() {
		t.Fatal("Start without a parent created a span")
	}
	if TraceParent(context.Background()) != "" {
		t.Fatal("traceparent without a span")
	}
	if tr.Handler("/x", http.NotFoundHandler(), nil) == nil {
		t.Fatal("nil handler")
	}
}

func TestParentChildAndLinks(t *testing.T) {
	exp := &memExporter{}
	tr := New(Options{Exporter: exp, Resource: Resource{ServiceName: "svc"}})
	ctx, root := tr.Start(context.Background(), "root", WithKind(KindConsumer))
	cctx, child := Start(ctx, "child", WithKind(KindClient))
	_, grand := Start(cctx, "grand")
	grand.End()
	child.End()
	link := Link{SpanContext: SpanContext{TraceID: TraceID{1}, SpanID: SpanID{2}, Sampled: true}, Attrs: []Attr{String("event", "webhook")}}
	_, other := tr.Start(ctx, "new root", WithNewRoot(), WithLinks(link, Link{}))
	other.End()
	root.End()
	root.End() // second End is ignored

	spans := exp.all()
	if len(spans) != 4 {
		t.Fatalf("got %d spans", len(spans))
	}
	byName := map[string]*SpanData{}
	for _, s := range spans {
		byName[s.Name] = s
	}
	r, c, g, o := byName["root"], byName["child"], byName["grand"], byName["new root"]
	if r.Parent.IsValid() || r.Kind != KindConsumer {
		t.Fatalf("root: %+v", r)
	}
	if c.SpanContext.TraceID != r.SpanContext.TraceID || c.Parent != r.SpanContext.SpanID || c.Kind != KindClient {
		t.Fatalf("child not under root")
	}
	if g.Parent != c.SpanContext.SpanID || g.Kind != KindInternal {
		t.Fatalf("grandchild not under child")
	}
	if o.SpanContext.TraceID == r.SpanContext.TraceID || o.Parent.IsValid() {
		t.Fatalf("WithNewRoot kept the parent")
	}
	if len(o.Links) != 1 || o.Links[0].SpanContext.SpanID != (SpanID{2}) {
		t.Fatalf("links = %+v", o.Links)
	}
	if got := TraceParent(cctx); got != child.SpanContext().TraceParent() || !strings.HasSuffix(got, "-01") {
		t.Fatalf("TraceParent(ctx) = %q", got)
	}
}

func TestRemoteParent(t *testing.T) {
	exp := &memExporter{}
	tr := New(Options{Exporter: exp})
	sc, _ := ParseTraceParent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	_, sp := tr.Start(ContextWithRemote(context.Background(), sc), "server")
	sp.End()
	got := exp.all()[0]
	if got.SpanContext.TraceID != sc.TraceID || got.Parent != sc.SpanID {
		t.Fatalf("remote parent ignored: %+v", got)
	}
}

func TestSampling(t *testing.T) {
	exp := &memExporter{}
	none := New(Options{Exporter: exp, SampleRatio: ratio(0)})
	ctx, sp := none.Start(context.Background(), "x")
	_, child := Start(ctx, "child")
	if sp.IsRecording() || child.IsRecording() {
		t.Fatal("ratio 0 records")
	}
	sp.SetAttr(String("a", "b"))
	child.End()
	sp.End()
	if len(exp.all()) != 0 {
		t.Fatal("unsampled spans exported")
	}
	if !sp.SpanContext().IsValid() || TraceParent(ctx) != "" {
		t.Fatal("unsampled span must have ids but no traceparent to link")
	}

	half := New(Options{Exporter: exp, SampleRatio: ratio(0.5)})
	kept := 0
	for i := 0; i < 2000; i++ {
		ctx, sp := half.Start(context.Background(), "x")
		_, c := Start(ctx, "c")
		if c.SpanContext().Sampled != sp.SpanContext().Sampled {
			t.Fatal("child decided on its own")
		}
		if sp.SpanContext().Sampled {
			kept++
		}
	}
	if kept < 800 || kept > 1200 {
		t.Fatalf("ratio 0.5 kept %d of 2000", kept)
	}
}

func TestScrubCapAndAttrs(t *testing.T) {
	exp := &memExporter{}
	tr := New(Options{Exporter: exp, Scrub: func(s string) string { return strings.ReplaceAll(s, "glpat-SECRET", "[REDACTED]") }})
	_, sp := tr.Start(context.Background(), "x", WithAttrs(String("token", "glpat-SECRET"), String("", "no key")))
	sp.SetAttr(String("long", strings.Repeat("я", 400)), Float64("nan", nanValue()), Int("n", 1))
	sp.SetAttr(Int("n", 2)) // replaces
	sp.SetAttrIfAbsent(Int("n", 3), Bool("b", true))
	sp.RecordError(errors.New("POST https://x/?private_token=glpat-SECRET failed"), "network")
	sp.SetStatus(StatusOK, "") // never downgrades an error
	for i := 0; i < MaxSpanAttrs+5; i++ {
		sp.SetAttr(Int("k"+strings.Repeat("x", i), i))
	}
	sp.End()
	sp.SetAttr(String("after", "end")) // ignored
	d := exp.all()[0]
	if v, _ := d.AttrValue("token"); v.Str != "[REDACTED]" {
		t.Fatalf("token = %q", v.Str)
	}
	if v, _ := d.AttrValue("long"); len(v.Str) > MaxAttrLen || !strings.HasSuffix(v.Str, "…") {
		t.Fatalf("long not capped: %d", len(v.Str))
	}
	if _, ok := d.AttrValue("nan"); ok {
		t.Fatal("NaN kept")
	}
	if v, _ := d.AttrValue("n"); v.Int != 2 {
		t.Fatalf("n = %d", v.Int)
	}
	if _, ok := d.AttrValue("after"); ok {
		t.Fatal("attribute after End")
	}
	if d.Status != StatusError || strings.Contains(d.StatusMsg, "glpat-SECRET") || strings.Contains(d.StatusMsg, "private_token") ||
		!strings.Contains(d.StatusMsg, "https://x/ failed") {
		t.Fatalf("status %v %q", d.Status, d.StatusMsg)
	}
	if len(d.Events) != 1 || d.Events[0].Name != "exception" {
		t.Fatalf("events %+v", d.Events)
	}
	for _, a := range d.Events[0].Attrs {
		if strings.Contains(a.Value.Str, "glpat-SECRET") {
			t.Fatal("secret in exception event")
		}
	}
	if len(d.Attrs) != MaxSpanAttrs || d.DroppedAttrs == 0 {
		t.Fatalf("attrs %d dropped %d", len(d.Attrs), d.DroppedAttrs)
	}
}

func nanValue() float64 {
	var z float64
	return z / z
}

// goldenSpans is a fixed request covering every field the encoder writes.
func goldenSpans() []*SpanData {
	res := Resource{ServiceName: "gitlab-review-bot", ServiceVersion: "1.2.3", ServiceInstanceID: "bot-1", Environment: "prod"}
	tid := TraceID{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36}
	root := SpanID{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7}
	child := SpanID{0x53, 0x99, 0x5c, 0x3f, 0x42, 0xcd, 0x8a, 0xd8}
	t0 := time.Unix(1700000000, 123456789).UTC()
	return []*SpanData{
		{
			Resource: res, SpanContext: SpanContext{TraceID: tid, SpanID: root, Sampled: true}, Name: "review.job", Kind: KindConsumer,
			Start: t0, End: t0.Add(90 * time.Second),
			Attrs:  []Attr{String("gitlab.project", "group/app"), Int("gitlab.mr.iid", 7), Bool("job.manual", false), Float64("reviewbot.claude.cost_usd", 0.25)},
			Links:  []Link{{SpanContext: SpanContext{TraceID: TraceID{1}, SpanID: SpanID{2}, Sampled: true}, Attrs: []Attr{String("reviewbot.event.source", "webhook")}}},
			Status: StatusOK,
		},
		{
			Resource: res, SpanContext: SpanContext{TraceID: tid, SpanID: child, Sampled: true}, Parent: root, Name: "GET", Kind: KindClient,
			Start: t0.Add(time.Second), End: t0.Add(1500 * time.Millisecond),
			Attrs:  []Attr{String("http.request.method", "GET"), Int("http.response.status_code", 503)},
			Events: []Event{{Name: "exception", Time: t0.Add(1400 * time.Millisecond), Attrs: []Attr{String("exception.type", "http_5xx")}}},
			Status: StatusError, StatusMsg: "HTTP 503", DroppedAttrs: 1,
		},
	}
}

func TestOTLPJSONGolden(t *testing.T) {
	b, err := EncodeJSON(goldenSpans())
	if err != nil {
		t.Fatal(err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, b, "", "  "); err != nil {
		t.Fatal(err)
	}
	pretty.WriteByte('\n')
	golden := filepath.Join("testdata", "otlp_traces.golden.json")
	if *update {
		if err := os.WriteFile(golden, pretty.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/tracing -update)", err)
	}
	if !bytes.Equal(pretty.Bytes(), want) {
		t.Fatalf("OTLP JSON differs from %s:\n%s", golden, pretty.String())
	}
}

// TestOTLPJSONShape checks the field names and JSON types against the
// OTLP/JSON mapping independently of the golden file.
func TestOTLPJSONShape(t *testing.T) {
	b, _ := EncodeJSON(goldenSpans())
	var req map[string]any
	if err := json.Unmarshal(b, &req); err != nil {
		t.Fatal(err)
	}
	rs := req["resourceSpans"].([]any)[0].(map[string]any)
	res := attrMap(t, rs["resource"].(map[string]any)["attributes"])
	for k, want := range map[string]string{"service.name": "gitlab-review-bot", "service.version": "1.2.3",
		"service.instance.id": "bot-1", "deployment.environment": "prod"} {
		if res[k] != want {
			t.Errorf("resource %s = %v", k, res[k])
		}
	}
	ss := rs["scopeSpans"].([]any)[0].(map[string]any)
	if ss["scope"].(map[string]any)["name"] != ScopeName {
		t.Error("scope name")
	}
	spans := ss["spans"].([]any)
	root, child := spans[0].(map[string]any), spans[1].(map[string]any)
	hexRe := func(s any, n int) bool {
		str, ok := s.(string)
		return ok && len(str) == n && lowerHex(str)
	}
	if !hexRe(root["traceId"], 32) || !hexRe(root["spanId"], 16) || !hexRe(child["parentSpanId"], 16) {
		t.Errorf("ids must be lowercase hex strings: %v %v %v", root["traceId"], root["spanId"], child["parentSpanId"])
	}
	if _, ok := root["parentSpanId"]; ok {
		t.Error("root has parentSpanId")
	}
	if s, ok := root["startTimeUnixNano"].(string); !ok || s != "1700000000123456789" {
		t.Errorf("startTimeUnixNano must be a decimal string: %#v", root["startTimeUnixNano"])
	}
	if _, ok := root["endTimeUnixNano"].(string); !ok {
		t.Error("endTimeUnixNano must be a string")
	}
	if k, ok := root["kind"].(float64); !ok || k != 5 {
		t.Errorf("kind = %#v (consumer = 5)", root["kind"])
	}
	if k := child["kind"].(float64); k != 3 {
		t.Errorf("client kind = %v", k)
	}
	if st := child["status"].(map[string]any); st["code"].(float64) != 2 || st["message"] != "HTTP 503" {
		t.Errorf("status = %v", st)
	}
	if st := root["status"].(map[string]any); st["code"].(float64) != 1 {
		t.Errorf("ok status = %v", st)
	}
	vals := map[string]map[string]any{}
	for _, a := range root["attributes"].([]any) {
		kv := a.(map[string]any)
		vals[kv["key"].(string)] = kv["value"].(map[string]any)
	}
	if _, ok := vals["gitlab.project"]["stringValue"].(string); !ok {
		t.Error("stringValue")
	}
	if v, ok := vals["gitlab.mr.iid"]["intValue"].(string); !ok || v != "7" {
		t.Errorf("intValue must be a decimal string: %#v", vals["gitlab.mr.iid"])
	}
	if v, ok := vals["job.manual"]["boolValue"].(bool); !ok || v {
		t.Errorf("boolValue: %#v", vals["job.manual"])
	}
	if v, ok := vals["reviewbot.claude.cost_usd"]["doubleValue"].(float64); !ok || v != 0.25 {
		t.Errorf("doubleValue: %#v", vals["reviewbot.claude.cost_usd"])
	}
	for _, v := range vals {
		if len(v) != 1 {
			t.Errorf("AnyValue must have exactly one field: %v", v)
		}
	}
	ev := child["events"].([]any)[0].(map[string]any)
	if _, ok := ev["timeUnixNano"].(string); !ok || ev["name"] != "exception" {
		t.Errorf("event = %v", ev)
	}
	ln := root["links"].([]any)[0].(map[string]any)
	if !hexRe(ln["traceId"], 32) || !hexRe(ln["spanId"], 16) {
		t.Errorf("link = %v", ln)
	}
	if child["droppedAttributesCount"].(float64) != 1 {
		t.Error("droppedAttributesCount")
	}
}

func attrMap(t *testing.T, v any) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, a := range v.([]any) {
		kv := a.(map[string]any)
		val := kv["value"].(map[string]any)
		for _, x := range val {
			out[kv["key"].(string)] = x
		}
	}
	return out
}

func TestLogHandler(t *testing.T) {
	var buf bytes.Buffer
	active := &Active{}
	log := slog.New(LogHandler(slog.NewJSONHandler(&buf, nil), active, "mr"))
	tr := New(Options{Exporter: &memExporter{}})
	ctx, sp := tr.Start(context.Background(), "job")
	unregister := active.Register("42!7", sp.SpanContext())

	line := func(f func()) map[string]any {
		buf.Reset()
		f()
		var m map[string]any
		if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
			t.Fatalf("%v: %s", err, buf.String())
		}
		return m
	}
	want := sp.SpanContext()
	m := line(func() { log.InfoContext(ctx, "with ctx") })
	if m["trace_id"] != want.TraceID.String() || m["span_id"] != want.SpanID.String() {
		t.Fatalf("ctx line: %v", m)
	}
	m = line(func() { log.Info("by mr attr", "mr", "42!7") })
	if m["trace_id"] != want.TraceID.String() {
		t.Fatalf("mr attr line: %v", m)
	}
	m = line(func() { log.With("mr", "42!7", "kind", "review").Info("by With") })
	if m["trace_id"] != want.TraceID.String() {
		t.Fatalf("With line: %v", m)
	}
	m = line(func() { log.Info("other mr", "mr", "1!1") })
	if _, ok := m["trace_id"]; ok {
		t.Fatalf("unrelated line got a trace: %v", m)
	}
	unregister()
	m = line(func() { log.Info("after the job", "mr", "42!7") })
	if _, ok := m["trace_id"]; ok {
		t.Fatalf("finished job still correlated: %v", m)
	}
	// Unsampled traces are not referenced.
	off := New(Options{SampleRatio: ratio(0)})
	octx, _ := off.Start(context.Background(), "job")
	m = line(func() { log.InfoContext(octx, "unsampled") })
	if _, ok := m["trace_id"]; ok {
		t.Fatalf("unsampled trace referenced: %v", m)
	}
}

func TestHTTPHandler(t *testing.T) {
	exp := &memExporter{}
	tr := New(Options{Exporter: exp})
	var inner context.Context
	h := tr.Handler("/webhook", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner = r.Context()
		w.WriteHeader(http.StatusBadGateway)
	}), func(r *http.Request) []Attr { return []Attr{String("gitlab.event", r.Header.Get("X-Gitlab-Event"))} })
	req := httptest.NewRequest(http.MethodPost, "/webhook?token=secret", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	req.Header.Set("X-Gitlab-Event", "Note Hook")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if SpanFromContext(inner) == nil {
		t.Fatal("handler context has no span")
	}
	d := exp.all()[0]
	if d.Kind != KindServer || d.Name != "POST /webhook" {
		t.Fatalf("span %+v", d)
	}
	// The untrusted traceparent is a link, not the parent: a new trace.
	if d.SpanContext.TraceID.String() == "4bf92f3577b34da6a3ce929d0e0e4736" || d.Parent.IsValid() {
		t.Fatalf("remote traceparent became the parent: %+v", d)
	}
	if len(d.Links) != 1 || d.Links[0].SpanContext.TraceID.String() != "4bf92f3577b34da6a3ce929d0e0e4736" ||
		d.Links[0].SpanContext.SpanID.String() != "00f067aa0ba902b7" {
		t.Fatalf("links %+v", d.Links)
	}
	if v, _ := d.AttrValue("url.path"); v.Str != "/webhook" {
		t.Fatalf("url.path = %q (no query)", v.Str)
	}
	if v, _ := d.AttrValue("http.response.status_code"); v.Int != 502 || d.Status != StatusError {
		t.Fatalf("status %v %v", v, d.Status)
	}
	if v, _ := d.AttrValue("gitlab.event"); v.Str != "Note Hook" {
		t.Fatalf("extra attr %v", v)
	}
}

// An inbound traceparent comes before any authentication (the webhook
// secret is checked inside the handler): its sampled flag and trace id must
// not decide sampling, or anyone could force every request into the export
// queue past sample_ratio.
func TestHTTPHandlerIgnoresRemoteSampling(t *testing.T) {
	exp := &memExporter{}
	tr := New(Options{Exporter: exp, SampleRatio: ratio(0)})
	h := tr.Handler("/webhook", http.NotFoundHandler(), nil)
	for _, tp := range []string{
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da60000000000000000-00f067aa0ba902b7-01", // low bytes 0: below any ratio threshold
	} {
		req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
		req.Header.Set("traceparent", tp)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if n := len(exp.all()); n != 0 {
		t.Fatalf("sample_ratio 0 but %d spans exported for remote sampled traceparents", n)
	}
}
