package metrics

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func render(t *testing.T, r *Registry) string {
	t.Helper()
	var b strings.Builder
	if err := r.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestFormatSortedWithHelpAndType(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounterVec("b_total", "Things.\nSecond line \\ here.", "kind", "status")
	g := r.NewGauge("a_gauge", "A gauge.")
	c.With("review", "ok").Add(2)
	c.With("chat", "failed").Inc()
	c.With("review", "failed").Add(0.5)
	c.With("review", "failed").Add(-3) // ignored
	g.Set(-1.25)

	want := `# HELP a_gauge A gauge.
# TYPE a_gauge gauge
a_gauge -1.25
# HELP b_total Things.\nSecond line \\ here.
# TYPE b_total counter
b_total{kind="chat",status="failed"} 1
b_total{kind="review",status="failed"} 0.5
b_total{kind="review",status="ok"} 2
`
	if got := render(t, r); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestLabelEscaping(t *testing.T) {
	r := NewRegistry()
	r.NewGaugeVec("x", "", "v").With("a\"b\\c\nd\xff").Set(1)
	got := render(t, r)
	want := "# TYPE x gauge\nx{v=\"a\\\"b\\\\c\\nd\uFFFD\"} 1\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestHistogram(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogramVec("dur_seconds", "Duration.", []float64{5, 1, 1}, "kind")
	for _, v := range []float64{0.5, 1, 3, 100} {
		h.With("review").Observe(v)
	}
	want := `# HELP dur_seconds Duration.
# TYPE dur_seconds histogram
dur_seconds_bucket{kind="review",le="1"} 2
dur_seconds_bucket{kind="review",le="5"} 3
dur_seconds_bucket{kind="review",le="+Inf"} 4
dur_seconds_sum{kind="review"} 104.5
dur_seconds_count{kind="review"} 4
`
	if got := render(t, r); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestLabelLessCounterExportedAtZero(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("c_total", "C.")
	if got := render(t, r); !strings.Contains(got, "\nc_total 0\n") {
		t.Fatalf("got %q", got)
	}
}

func TestCardinalityCap(t *testing.T) {
	r := NewRegistry()
	r.SetMaxSeries(3)
	c := r.NewCounterVec("c_total", "", "a", "b")
	for i := 0; i < 10; i++ {
		c.With(fmt.Sprint(i), "x").Inc()
	}
	c.With("0", "x").Inc() // existing series still updated
	out := render(t, r)
	if n := strings.Count(out, "c_total{"); n != 4 {
		t.Fatalf("want 3 series + overflow, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, `c_total{a="other",b="other"} 7`) || !strings.Contains(out, `c_total{a="0",b="x"} 2`) {
		t.Fatalf("overflow folding:\n%s", out)
	}
}

func TestRegisterPanics(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("ok_total", "")
	for name, f := range map[string]func(){
		"dup":       func() { r.NewCounter("ok_total", "") },
		"bad name":  func() { r.NewCounter("1bad", "") },
		"bad label": func() { r.NewCounterVec("x_total", "", "a-b") },
		"le":        func() { r.NewHistogramVec("h", "", []float64{1}, "le") },
		"arity":     func() { r.NewCounterVec("y_total", "", "a").With() },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			f()
		}()
	}
}

func TestCollectHook(t *testing.T) {
	r := NewRegistry()
	g := r.NewGauge("g", "")
	n := 0
	r.OnCollect(func() { n++; g.Set(float64(n)) })
	render(t, r)
	if out := render(t, r); !strings.Contains(out, "\ng 2\n") {
		t.Fatalf("hook not run per scrape: %q", out)
	}
}

func TestConcurrent(t *testing.T) {
	r := NewRegistry()
	r.SetMaxSeries(50)
	c := r.NewCounterVec("c_total", "", "k")
	h := r.NewHistogramVec("h", "", []float64{1, 10}, "k")
	g := r.NewGauge("g", "")
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				k := fmt.Sprint(i % 100)
				c.With(k).Inc()
				h.With(k).Observe(float64(i % 20))
				g.Add(1)
				if i%100 == 0 {
					var b strings.Builder
					_ = r.WriteText(&b)
				}
			}
		}()
	}
	wg.Wait()
	// Which keys got a slot varies; check the grand total and the cap.
	out := render(t, r)
	if n := strings.Count(out, "c_total{"); n > 51 {
		t.Fatalf("cap exceeded: %d series", n)
	}
	var sum float64
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "c_total{") {
			var v float64
			fmt.Sscanf(line[strings.LastIndexByte(line, ' ')+1:], "%g", &v)
			sum += v
		}
	}
	if sum != 8000 || g.Value() != 8000 {
		t.Fatalf("lost updates: counters %v gauge %v", sum, g.Value())
	}
}
