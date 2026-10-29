package diagweb

import "testing"

func TestTraceLink(t *testing.T) {
	id := "4bf92f3577b34da6a3ce929d0e0e4736"
	for _, c := range []struct{ tmpl, id, want string }{
		{"https://grafana.example/explore?traceId={trace_id}", id, "https://grafana.example/explore?traceId=" + id},
		{"", id, ""},
		{"https://grafana.example/{trace_id}", "", ""},
		{"https://grafana.example/{trace_id}", "<script>", ""},
		{"javascript:alert('{trace_id}')", id, ""},
	} {
		if got := TraceLink(c.tmpl, c.id); got != c.want {
			t.Errorf("TraceLink(%q, %q) = %q, want %q", c.tmpl, c.id, got, c.want)
		}
	}
}
