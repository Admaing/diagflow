// Package metrics is a dependency-free Prometheus text-format registry
// (counters and duration histograms) sufficient for DiagFlow's runtime KPIs.
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Counter is a monotonically increasing counter with optional labels.
type Counter struct {
	name       string
	help       string
	labelNames []string
	mu         sync.Mutex
	vals       map[string]*atomic.Int64
	order      []string
}

var (
	registryMu sync.Mutex
	registry   = map[string]any{}
)

// NewCounter registers a counter (returns the existing one on duplicate name).
func NewCounter(name, help string, labelNames ...string) *Counter {
	registryMu.Lock()
	defer registryMu.Unlock()
	if c, ok := registry[name]; ok {
		return c.(*Counter)
	}
	c := &Counter{name: name, help: help, labelNames: labelNames, vals: map[string]*atomic.Int64{}}
	registry[name] = c
	return c
}

// Inc increments the counter for the given label values.
func (c *Counter) Inc(labelValues ...string) { c.Add(1, labelValues...) }

// Add adds n to the counter for the given label values.
func (c *Counter) Add(n int64, labelValues ...string) {
	key := strings.Join(labelValues, "\x00")
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.vals[key]
	if !ok {
		v = &atomic.Int64{}
		c.vals[key] = v
		c.order = append(c.order, key)
	}
	v.Add(n)
}

// DurationBuckets are millisecond buckets for latency histograms.
var DurationBuckets = []float64{100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 120000, 300000}

// Histogram is a fixed-bucket cumulative histogram (no labels).
type Histogram struct {
	name   string
	help   string
	counts []atomic.Int64
	sum    atomic.Int64 // milliseconds
	total  atomic.Int64
}

// NewHistogram registers a histogram with DurationBuckets.
func NewHistogram(name, help string) *Histogram {
	registryMu.Lock()
	defer registryMu.Unlock()
	if h, ok := registry[name]; ok {
		return h.(*Histogram)
	}
	h := &Histogram{name: name, help: help, counts: make([]atomic.Int64, len(DurationBuckets))}
	registry[name] = h
	return h
}

// Observe records a millisecond duration.
func (h *Histogram) Observe(ms float64) {
	h.sum.Add(int64(ms))
	h.total.Add(1)
	for i, bucket := range DurationBuckets {
		if ms <= bucket {
			h.counts[i].Add(1)
		}
	}
}

// Render produces Prometheus text exposition format.
func Render() string {
	registryMu.Lock()
	defer registryMu.Unlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		switch v := registry[n].(type) {
		case *Counter:
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", v.name, v.help, v.name)
			v.mu.Lock()
			for _, key := range v.order {
				val := v.vals[key].Load()
				if key == "" {
					fmt.Fprintf(&b, "%s %d\n", v.name, val)
				} else {
					fmt.Fprintf(&b, "%s%s %d\n", v.name, labelsLine(v.labelNames, key), val)
				}
			}
			v.mu.Unlock()
		case *Histogram:
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s histogram\n", v.name, v.help, v.name)
			cum := int64(0)
			for i, bucket := range DurationBuckets {
				cum += v.counts[i].Load()
				fmt.Fprintf(&b, "%s_bucket{le=\"%g\"} %d\n", v.name, bucket, cum)
			}
			fmt.Fprintf(&b, "%s_bucket{le=\"+Inf\"} %d\n", v.name, v.total.Load())
			fmt.Fprintf(&b, "%s_sum %d\n", v.name, v.sum.Load())
			fmt.Fprintf(&b, "%s_count %d\n", v.name, v.total.Load())
		}
	}
	return b.String()
}

func labelsLine(names []string, key string) string {
	vals := strings.Split(key, "\x00")
	parts := make([]string, len(vals))
	for i, val := range vals {
		name := "label"
		if i < len(names) {
			name = names[i]
		}
		parts[i] = fmt.Sprintf("%s=%q", name, val)
	}
	return "{" + strings.Join(parts, ",") + "}"
}
