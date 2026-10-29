package tracing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveTempo exports a small trace to a real collector and, when a Tempo
// query URL is given, reads it back. Skipped unless the stand is up:
//
//	REVIEWBOT_OTLP_ENDPOINT=http://localhost:4318 REVIEWBOT_TEMPO_URL=http://localhost:3200 \
//	  go test ./internal/tracing -run TestLiveTempo -v
func TestLiveTempo(t *testing.T) {
	endpoint := os.Getenv("REVIEWBOT_OTLP_ENDPOINT")
	if endpoint == "" {
		t.Skip("REVIEWBOT_OTLP_ENDPOINT not set (needs a running collector, e.g. the stand's Tempo)")
	}
	var results []string
	exp, err := NewExporter(ExporterOptions{Endpoint: endpoint, Interval: time.Hour,
		Observer: func(r string, n int) { results = append(results, fmt.Sprintf("%s=%d", r, n)) },
		OnError:  func(r string) { results = append(results, "error="+r) }})
	if err != nil {
		t.Fatal(err)
	}
	tr := New(Options{Exporter: exp, Resource: Resource{ServiceName: "reviewbot-live-test", ServiceVersion: "test",
		ServiceInstanceID: "live", Environment: "test"}})
	link := SpanContext{TraceID: newTraceID(), SpanID: newSpanID(), Sampled: true}
	ctx, root := tr.Start(context.Background(), "review.job", WithKind(KindConsumer),
		WithAttrs(String("gitlab.project", "group/app"), Int("gitlab.mr.iid", 7), Bool("job.manual", false)),
		WithLinks(Link{SpanContext: link, Attrs: []Attr{String("reviewbot.link", "event")}}))
	_, child := Start(ctx, "claude review", WithKind(KindClient), WithAttrs(String("gen_ai.system", "anthropic"),
		Int64("gen_ai.usage.input_tokens", 1234), Float64("reviewbot.claude.cost_usd", 0.42)))
	child.AddEvent("note", Int("n", 1))
	child.RecordError(fmt.Errorf("boom"), "test")
	child.End()
	root.SetStatus(StatusOK, "")
	root.End()
	if err := exp.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(results, ",") != "exported=2" {
		t.Fatalf("export results: %v", results)
	}
	tempo := os.Getenv("REVIEWBOT_TEMPO_URL")
	if tempo == "" {
		return
	}
	id := root.TraceID()
	var body []byte
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(strings.TrimRight(tempo, "/") + "/api/v2/traces/" + id)
		if err == nil {
			body, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("trace %s not found in Tempo: %v %s", id, err, body)
		}
		time.Sleep(time.Second)
	}
	s := string(body)
	for _, want := range []string{"review.job", "claude review", "reviewbot-live-test", "gen_ai.usage.input_tokens", "group/app"} {
		if !strings.Contains(s, want) {
			t.Errorf("Tempo's trace lacks %q", want)
		}
	}
	var anyJSON map[string]any
	if err := json.Unmarshal(body, &anyJSON); err != nil {
		t.Fatalf("Tempo answer: %v", err)
	}
	t.Logf("trace %s read back from Tempo (%d bytes)", id, len(body))
}
