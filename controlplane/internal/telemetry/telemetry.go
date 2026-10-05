// Package telemetry is a minimal Prometheus-compatible metrics registry.
//
// Hand-rolled rather than imported so the daemon builds with no external
// dependencies: a PoP box should need nothing but a Go toolchain to produce a
// binary. The text exposition format is stable and simple enough that this is a
// smaller liability than a dependency tree.
package telemetry

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Kind is a metric type as Prometheus names them.
type Kind string

const (
	Counter Kind = "counter"
	Gauge   Kind = "gauge"
)

// Labels is an unordered label set. Rendering sorts by key so output is stable.
type Labels map[string]string

func (l Labels) key() string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString("=\"")
		b.WriteString(escape(l[k]))
		b.WriteString("\"")
	}
	return b.String()
}

type series struct {
	labels string
	value  float64
}

type metric struct {
	name   string
	kind   Kind
	help   string
	series map[string]*series
}

// Registry holds every metric the daemon exports.
type Registry struct {
	mu      sync.RWMutex
	metrics map[string]*metric
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{metrics: make(map[string]*metric)}
}

// Define declares a metric. Calling it twice with the same name is harmless;
// the first help text and kind win.
func (r *Registry) Define(name string, kind Kind, help string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.metrics[name]; ok {
		return
	}
	r.metrics[name] = &metric{
		name:   name,
		kind:   kind,
		help:   help,
		series: make(map[string]*series),
	}
}

// Set replaces a gauge value.
func (r *Registry) Set(name string, labels Labels, v float64) {
	r.withSeries(name, labels, func(s *series) { s.value = v })
}

// Add increments a counter or gauge.
func (r *Registry) Add(name string, labels Labels, delta float64) {
	r.withSeries(name, labels, func(s *series) { s.value += delta })
}

// Inc adds one.
func (r *Registry) Inc(name string, labels Labels) { r.Add(name, labels, 1) }

// Value reads a single series back, mainly for tests.
func (r *Registry) Value(name string, labels Labels) (float64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.metrics[name]
	if !ok {
		return 0, false
	}
	s, ok := m.series[labels.key()]
	if !ok {
		return 0, false
	}
	return s.value, true
}

// DropMatching deletes series whose labels match every pair in match. Needed
// because per-terminal gauges would otherwise accumulate forever as
// subscribers churn, which is the classic way a metrics endpoint turns into a
// memory leak.
func (r *Registry) DropMatching(name string, match Labels) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.metrics[name]
	if !ok {
		return
	}
	want := match.key()
	for k := range m.series {
		if want == "" || strings.Contains(k, want) {
			delete(m.series, k)
		}
	}
}

func (r *Registry) withSeries(name string, labels Labels, fn func(*series)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.metrics[name]
	if !ok {
		// An undefined metric is a programming slip, not a runtime error worth
		// dropping data over. Register it untyped and carry on.
		m = &metric{name: name, kind: Gauge, series: make(map[string]*series)}
		r.metrics[name] = m
	}
	k := labels.key()
	s, ok := m.series[k]
	if !ok {
		s = &series{labels: k}
		m.series[k] = s
	}
	fn(s)
}

// Render writes the registry in Prometheus text exposition format.
func (r *Registry) Render(w io.Writer) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.metrics))
	for n := range r.metrics {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, n := range names {
		m := r.metrics[n]
		if len(m.series) == 0 {
			continue
		}
		if m.help != "" {
			fmt.Fprintf(&b, "# HELP %s %s\n", m.name, m.help)
		}
		fmt.Fprintf(&b, "# TYPE %s %s\n", m.name, m.kind)

		keys := make([]string, 0, len(m.series))
		for k := range m.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for _, k := range keys {
			s := m.series[k]
			if k == "" {
				fmt.Fprintf(&b, "%s %s\n", m.name, format(s.value))
			} else {
				fmt.Fprintf(&b, "%s{%s} %s\n", m.name, k, format(s.value))
			}
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func format(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

func escape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}
