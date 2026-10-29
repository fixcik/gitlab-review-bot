// Package metrics is a small, dependency-free implementation of Prometheus
// metrics: counter, gauge and histogram vectors with fixed label names,
// rendered in the text exposition format version 0.0.4.
//
// It exists because the Prometheus client library is not a dependency of
// this module. Only what the bot needs is implemented:
//
//   - families are registered once (duplicate or invalid names panic: that is
//     a programming error);
//   - every family caps its label combinations (DefaultMaxSeries); a new
//     combination beyond the cap is folded into one series whose label values
//     are all OverflowValue, so a bug or hostile input cannot blow up memory
//     or the scrape size;
//   - output is stable: families sorted by name, series by label values;
//   - everything is safe for concurrent use; counters and gauges are
//     lock-free on the hot path once a series exists.
package metrics

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// DefaultMaxSeries is the default cap of label combinations per family.
const DefaultMaxSeries = 1000

// OverflowValue replaces every label value of a combination over the cap.
const OverflowValue = "other"

// ContentType is the Content-Type of the text exposition format 0.0.4.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

var (
	metricNameRe = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNameRe  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// Registry holds metric families and renders them.
type Registry struct {
	mu        sync.Mutex
	families  map[string]family
	hooks     []func()
	maxSeries int
}

type family interface {
	write(b *bytes.Buffer)
}

// NewRegistry returns an empty registry with DefaultMaxSeries.
func NewRegistry() *Registry {
	return &Registry{families: map[string]family{}, maxSeries: DefaultMaxSeries}
}

// SetMaxSeries changes the label-combination cap of families registered
// afterwards (n < 1 means 1).
func (r *Registry) SetMaxSeries(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.maxSeries = max(n, 1)
}

// OnCollect adds a hook run at the start of every WriteText, e.g. to sample
// gauges (queue length, runtime stats) at scrape time.
func (r *Registry) OnCollect(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks = append(r.hooks, f)
}

func (r *Registry) register(name string, labels []string, f func(d desc) family) family {
	if !metricNameRe.MatchString(name) {
		panic(fmt.Sprintf("metrics: invalid metric name %q", name))
	}
	seen := map[string]bool{}
	for _, l := range labels {
		if !labelNameRe.MatchString(l) || strings.HasPrefix(l, "__") || seen[l] {
			panic(fmt.Sprintf("metrics: invalid or duplicate label %q of %s", l, name))
		}
		seen[l] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.families[name]; dup {
		panic(fmt.Sprintf("metrics: %s registered twice", name))
	}
	fam := f(desc{name: name, labels: slices.Clone(labels), max: r.maxSeries})
	r.families[name] = fam
	return fam
}

// WriteText runs the collect hooks and writes every family in the text
// exposition format, families sorted by name.
func (r *Registry) WriteText(w io.Writer) error {
	r.mu.Lock()
	hooks := slices.Clone(r.hooks)
	r.mu.Unlock()
	for _, h := range hooks {
		h()
	}
	r.mu.Lock()
	names := make([]string, 0, len(r.families))
	for n := range r.families {
		names = append(names, n)
	}
	fams := make([]family, 0, len(names))
	sort.Strings(names)
	for _, n := range names {
		fams = append(fams, r.families[n])
	}
	r.mu.Unlock()
	var b bytes.Buffer
	for _, f := range fams {
		f.write(&b)
	}
	_, err := w.Write(b.Bytes())
	return err
}

// ---------------------------------------------------------------------------
// Series storage shared by all vector kinds
// ---------------------------------------------------------------------------

type desc struct {
	name, help string
	labels     []string
	max        int
}

type series[T any] struct {
	values []string
	v      *T
}

type vec[T any] struct {
	desc
	mu     sync.RWMutex
	series map[string]*series[T]
	newT   func() *T
}

func newVec[T any](d desc, newT func() *T) *vec[T] {
	v := &vec[T]{desc: d, series: map[string]*series[T]{}, newT: newT}
	if len(d.labels) == 0 {
		v.get(nil) // a label-less metric is exported (as 0) from the start
	}
	return v
}

func (v *vec[T]) get(values []string) *T {
	if len(values) != len(v.labels) {
		panic(fmt.Sprintf("metrics: %s wants %d label values, got %d", v.name, len(v.labels), len(values)))
	}
	key := strings.Join(values, "\xff")
	v.mu.RLock()
	s := v.series[key]
	v.mu.RUnlock()
	if s != nil {
		return s.v
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if s = v.series[key]; s != nil {
		return s.v
	}
	if len(v.series) >= v.max {
		values = make([]string, len(v.labels))
		for i := range values {
			values[i] = OverflowValue
		}
		key = strings.Join(values, "\xff")
		if s = v.series[key]; s != nil {
			return s.v
		}
	} else {
		values = slices.Clone(values)
	}
	s = &series[T]{values: values, v: v.newT()}
	v.series[key] = s
	return s.v
}

// sorted returns the series ordered by label values.
func (v *vec[T]) sorted() []*series[T] {
	v.mu.RLock()
	out := make([]*series[T], 0, len(v.series))
	for _, s := range v.series {
		out = append(out, s)
	}
	v.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return slices.Compare(out[i].values, out[j].values) < 0 })
	return out
}

func (v *vec[T]) header(b *bytes.Buffer, typ string) {
	if v.help != "" {
		fmt.Fprintf(b, "# HELP %s %s\n", v.name, escapeHelp(v.help))
	}
	fmt.Fprintf(b, "# TYPE %s %s\n", v.name, typ)
}

// ---------------------------------------------------------------------------
// Counter
// ---------------------------------------------------------------------------

// Counter is a monotonically increasing value.
type Counter struct{ bits atomic.Uint64 }

// Add increases the counter; negative, NaN and infinite values are ignored.
func (c *Counter) Add(d float64) {
	if d <= 0 || math.IsNaN(d) || math.IsInf(d, 0) {
		return
	}
	addFloat(&c.bits, d)
}

// Inc adds 1.
func (c *Counter) Inc() { addFloat(&c.bits, 1) }

// Value returns the current value.
func (c *Counter) Value() float64 { return math.Float64frombits(c.bits.Load()) }

// CounterVec is a family of counters with fixed label names.
type CounterVec struct{ v *vec[Counter] }

// NewCounterVec registers a counter family.
func (r *Registry) NewCounterVec(name, help string, labels ...string) *CounterVec {
	return r.register(name, labels, func(d desc) family {
		d.help = help
		return &CounterVec{v: newVec(d, func() *Counter { return &Counter{} })}
	}).(*CounterVec)
}

// NewCounter registers a counter without labels.
func (r *Registry) NewCounter(name, help string) *Counter {
	return r.NewCounterVec(name, help).With()
}

// With returns the counter of the label values (in label-name order).
func (c *CounterVec) With(values ...string) *Counter { return c.v.get(values) }

func (c *CounterVec) write(b *bytes.Buffer) {
	c.v.header(b, "counter")
	for _, s := range c.v.sorted() {
		writeSample(b, c.v.name, c.v.labels, s.values, "", "", s.v.Value())
	}
}

// ---------------------------------------------------------------------------
// Gauge
// ---------------------------------------------------------------------------

// Gauge is a value that can go up and down.
type Gauge struct{ bits atomic.Uint64 }

// Set sets the value.
func (g *Gauge) Set(v float64) { g.bits.Store(math.Float64bits(v)) }

// Add adds d (may be negative).
func (g *Gauge) Add(d float64) { addFloat(&g.bits, d) }

// Value returns the current value.
func (g *Gauge) Value() float64 { return math.Float64frombits(g.bits.Load()) }

// GaugeVec is a family of gauges with fixed label names.
type GaugeVec struct{ v *vec[Gauge] }

// NewGaugeVec registers a gauge family.
func (r *Registry) NewGaugeVec(name, help string, labels ...string) *GaugeVec {
	return r.register(name, labels, func(d desc) family {
		d.help = help
		return &GaugeVec{v: newVec(d, func() *Gauge { return &Gauge{} })}
	}).(*GaugeVec)
}

// NewGauge registers a gauge without labels.
func (r *Registry) NewGauge(name, help string) *Gauge {
	return r.NewGaugeVec(name, help).With()
}

// With returns the gauge of the label values (in label-name order).
func (g *GaugeVec) With(values ...string) *Gauge { return g.v.get(values) }

func (g *GaugeVec) write(b *bytes.Buffer) {
	g.v.header(b, "gauge")
	for _, s := range g.v.sorted() {
		writeSample(b, g.v.name, g.v.labels, s.values, "", "", s.v.Value())
	}
}

// ---------------------------------------------------------------------------
// Histogram
// ---------------------------------------------------------------------------

// Histogram counts observations into cumulative buckets.
type Histogram struct {
	upper []float64 // shared, sorted, without +Inf

	mu     sync.Mutex
	counts []uint64 // per bucket, non-cumulative; last = +Inf
	sum    float64
	count  uint64
}

// Observe adds one observation (NaN is ignored).
func (h *Histogram) Observe(v float64) {
	if math.IsNaN(v) {
		return
	}
	i := sort.SearchFloat64s(h.upper, v) // first bucket with upper >= v
	h.mu.Lock()
	h.counts[i]++
	h.sum += v
	h.count++
	h.mu.Unlock()
}

func (h *Histogram) snapshot() (cum []uint64, sum float64, count uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cum = make([]uint64, len(h.counts))
	var acc uint64
	for i, c := range h.counts {
		acc += c
		cum[i] = acc
	}
	return cum, h.sum, h.count
}

// HistogramVec is a family of histograms with fixed label names and buckets.
type HistogramVec struct {
	v     *vec[Histogram]
	upper []float64
}

// NewHistogramVec registers a histogram family. buckets are upper bounds
// (sorted and de-duplicated here; +Inf is implicit).
func (r *Registry) NewHistogramVec(name, help string, buckets []float64, labels ...string) *HistogramVec {
	if slices.Contains(labels, "le") {
		panic("metrics: histogram label \"le\" is reserved")
	}
	upper := slices.Clone(buckets)
	sort.Float64s(upper)
	upper = slices.Compact(upper)
	upper = slices.DeleteFunc(upper, func(f float64) bool { return math.IsInf(f, +1) || math.IsNaN(f) })
	return r.register(name, labels, func(d desc) family {
		d.help = help
		hv := &HistogramVec{upper: upper}
		hv.v = newVec(d, func() *Histogram {
			return &Histogram{upper: upper, counts: make([]uint64, len(upper)+1)}
		})
		return hv
	}).(*HistogramVec)
}

// With returns the histogram of the label values (in label-name order).
func (h *HistogramVec) With(values ...string) *Histogram { return h.v.get(values) }

func (h *HistogramVec) write(b *bytes.Buffer) {
	h.v.header(b, "histogram")
	for _, s := range h.v.sorted() {
		cum, sum, count := s.v.snapshot()
		for i, up := range h.upper {
			writeSample(b, h.v.name+"_bucket", h.v.labels, s.values, "le", formatFloat(up), float64(cum[i]))
		}
		writeSample(b, h.v.name+"_bucket", h.v.labels, s.values, "le", "+Inf", float64(cum[len(cum)-1]))
		writeSample(b, h.v.name+"_sum", h.v.labels, s.values, "", "", sum)
		writeSample(b, h.v.name+"_count", h.v.labels, s.values, "", "", float64(count))
	}
}

// ---------------------------------------------------------------------------
// Formatting
// ---------------------------------------------------------------------------

func addFloat(bits *atomic.Uint64, d float64) {
	for {
		old := bits.Load()
		if bits.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+d)) {
			return
		}
	}
}

func writeSample(b *bytes.Buffer, name string, labels, values []string, extraName, extraValue string, v float64) {
	b.WriteString(name)
	if len(labels) > 0 || extraName != "" {
		b.WriteByte('{')
		for i, l := range labels {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(l)
			b.WriteString(`="`)
			b.WriteString(EscapeLabelValue(values[i]))
			b.WriteByte('"')
		}
		if extraName != "" {
			if len(labels) > 0 {
				b.WriteByte(',')
			}
			b.WriteString(extraName)
			b.WriteString(`="`)
			b.WriteString(extraValue)
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(formatFloat(v))
	b.WriteByte('\n')
}

// EscapeLabelValue escapes a label value for the text format: backslash,
// double quote and line feed. Invalid UTF-8 is replaced with U+FFFD.
func EscapeLabelValue(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	return labelEscaper.Replace(s)
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

func escapeHelp(s string) string { return helpEscaper.Replace(strings.ToValidUTF8(s, "�")) }

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, +1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	case v == math.Trunc(v) && math.Abs(v) < 1e15:
		return strconv.FormatInt(int64(v), 10)
	default:
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
}
