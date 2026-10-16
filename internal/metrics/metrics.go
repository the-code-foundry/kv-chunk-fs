// Package metrics is a minimal Prometheus registry: counters and histograms
// with labels, gauges from callbacks, rendered in the text exposition format.
// (The official client would add a large dependency for a dozen series.)
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
)

// Registry holds every metric, rendered in registration order.
type Registry struct {
	mu      sync.Mutex
	metrics []renderer
}

type renderer interface{ render(w io.Writer) }

func (r *Registry) add(m renderer) {
	r.mu.Lock()
	r.metrics = append(r.metrics, m)
	r.mu.Unlock()
}

// Render writes all metrics in the Prometheus text format.
func (r *Registry) Render(w io.Writer) {
	r.mu.Lock()
	ms := append([]renderer(nil), r.metrics...)
	r.mu.Unlock()
	for _, m := range ms {
		m.render(w)
	}
}

type series struct {
	labels []string
	value  float64
}

// CounterVec is a counter per label combination.
type CounterVec struct {
	name, help string
	labelNames []string
	mu         sync.Mutex
	series     map[string]*series
}

// Counter registers a counter with these label names.
func (r *Registry) Counter(name, help string, labelNames ...string) *CounterVec {
	c := &CounterVec{name: name, help: help, labelNames: labelNames, series: map[string]*series{}}
	r.add(c)
	return c
}

// Add adds v for these label values.
func (c *CounterVec) Add(v float64, labels ...string) {
	key := strings.Join(labels, "\x00")
	c.mu.Lock()
	s, ok := c.series[key]
	if !ok {
		s = &series{labels: labels}
		c.series[key] = s
	}
	s.value += v
	c.mu.Unlock()
}

// Inc adds 1.
func (c *CounterVec) Inc(labels ...string) { c.Add(1, labels...) }

func (c *CounterVec) render(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.series) == 0 && len(c.labelNames) == 0 {
		fmt.Fprintf(w, "%s 0\n", c.name)
	}
	for _, k := range sortedKeys(c.series) {
		s := c.series[k]
		fmt.Fprintf(w, "%s%s %s\n", c.name, labelString(c.labelNames, s.labels, "", ""), num(s.value))
	}
}

// HistogramVec is a histogram per label combination.
type HistogramVec struct {
	name, help string
	labelNames []string
	buckets    []float64
	mu         sync.Mutex
	series     map[string]*hseries
}

type hseries struct {
	labels []string
	counts []uint64 // per bucket, cumulative at render
	sum    float64
	count  uint64
}

// Histogram registers a histogram with these upper bounds.
func (r *Registry) Histogram(name, help string, buckets []float64, labelNames ...string) *HistogramVec {
	h := &HistogramVec{name: name, help: help, labelNames: labelNames, buckets: buckets, series: map[string]*hseries{}}
	r.add(h)
	return h
}

// Observe records v for these label values.
func (h *HistogramVec) Observe(v float64, labels ...string) {
	key := strings.Join(labels, "\x00")
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.series[key]
	if !ok {
		s = &hseries{labels: labels, counts: make([]uint64, len(h.buckets))}
		h.series[key] = s
	}
	for i, b := range h.buckets {
		if v <= b {
			s.counts[i]++
			break
		}
	}
	s.sum += v
	s.count++
}

func (h *HistogramVec) render(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", h.name, h.help, h.name)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, k := range sortedKeys(h.series) {
		s := h.series[k]
		var cum uint64
		for i, b := range h.buckets {
			cum += s.counts[i]
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelString(h.labelNames, s.labels, "le", num(b)), cum)
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelString(h.labelNames, s.labels, "le", "+Inf"), s.count)
		fmt.Fprintf(w, "%s_sum%s %s\n", h.name, labelString(h.labelNames, s.labels, "", ""), num(s.sum))
		fmt.Fprintf(w, "%s_count%s %d\n", h.name, labelString(h.labelNames, s.labels, "", ""), s.count)
	}
}

// GaugeFunc is a gauge whose value is read when rendered.
type GaugeFunc struct {
	name, help string
	fn         func() float64
}

// Gauge registers a gauge read from fn.
func (r *Registry) Gauge(name, help string, fn func() float64) {
	r.add(&GaugeFunc{name: name, help: help, fn: fn})
}

func (g *GaugeFunc) render(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %s\n", g.name, g.help, g.name, g.name, num(g.fn()))
}

// Info registers a constant gauge 1 with labels (build info).
func (r *Registry) Info(name, help string, labels map[string]string) {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	vals := make([]string, len(keys))
	for i, k := range keys {
		vals[i] = labels[k]
	}
	r.add(rendererFunc(func(w io.Writer) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s%s 1\n", name, help, name, name, labelString(keys, vals, "", ""))
	}))
}

type rendererFunc func(io.Writer)

func (f rendererFunc) render(w io.Writer) { f(w) }

func labelString(names, values []string, extraName, extraValue string) string {
	var parts []string
	for i, n := range names {
		v := ""
		if i < len(values) {
			v = values[i]
		}
		parts = append(parts, fmt.Sprintf("%s=%q", n, v))
	}
	if extraName != "" {
		parts = append(parts, fmt.Sprintf("%s=%q", extraName, extraValue))
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func num(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return fmt.Sprintf("%g", v)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
