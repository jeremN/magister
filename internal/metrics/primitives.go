// Package metrics is a tiny, dependency-free metrics registry rendered in the
// Prometheus text-exposition format. Primitives are safe for concurrent use via
// sync/atomic; labeled vectors guard their series map with an RWMutex and mutate
// the per-series primitive lock-free.
package metrics

import (
	"math"
	"strings"
	"sync"
	"sync/atomic"
)

// keySep joins label values into a map key; the unit separator does not appear
// in any label value we produce (statuses, HTTP methods, route templates, ints).
const keySep = "\x1f"

func joinKey(values []string) string { return strings.Join(values, keySep) }

// Counter is a monotonically increasing float64, safe for concurrent use.
type Counter struct{ bits atomic.Uint64 }

func (c *Counter) Add(delta float64) {
	for {
		old := c.bits.Load()
		nv := math.Float64frombits(old) + delta
		if c.bits.CompareAndSwap(old, math.Float64bits(nv)) {
			return
		}
	}
}

func (c *Counter) value() float64 { return math.Float64frombits(c.bits.Load()) }

// Gauge is a settable float64, safe for concurrent use.
type Gauge struct{ bits atomic.Uint64 }

func (g *Gauge) Set(v float64)  { g.bits.Store(math.Float64bits(v)) }
func (g *Gauge) value() float64 { return math.Float64frombits(g.bits.Load()) }

// Histogram observes samples into fixed ascending buckets. buckets[i] holds the
// raw (non-cumulative) count of samples in (bounds[i-1], bounds[i]]; samples above
// the top bound contribute only to count/sum (the implicit +Inf bucket). The
// renderer cumulates buckets at write time.
type Histogram struct {
	bounds  []float64
	buckets []atomic.Uint64
	sumBits atomic.Uint64
	count   atomic.Uint64
}

func newHistogram(bounds []float64) *Histogram {
	return &Histogram{bounds: bounds, buckets: make([]atomic.Uint64, len(bounds))}
}

func (h *Histogram) Observe(v float64) {
	i := 0
	for i < len(h.bounds) && v > h.bounds[i] {
		i++
	}
	if i < len(h.buckets) {
		h.buckets[i].Add(1)
	}
	h.count.Add(1)
	for {
		old := h.sumBits.Load()
		nv := math.Float64frombits(old) + v
		if h.sumBits.CompareAndSwap(old, math.Float64bits(nv)) {
			return
		}
	}
}

// CounterVec is a set of Counters partitioned by label values.
type CounterVec struct {
	mu     sync.RWMutex
	series map[string]*counterSeries
}

type counterSeries struct {
	labels []string
	c      Counter
}

func newCounterVec() *CounterVec { return &CounterVec{series: map[string]*counterSeries{}} }

func (v *CounterVec) Add(delta float64, labelValues ...string) {
	key := joinKey(labelValues)
	v.mu.RLock()
	s := v.series[key]
	v.mu.RUnlock()
	if s == nil {
		v.mu.Lock()
		if s = v.series[key]; s == nil {
			s = &counterSeries{labels: append([]string(nil), labelValues...)}
			v.series[key] = s
		}
		v.mu.Unlock()
	}
	s.c.Add(delta)
}

// HistogramVec is a set of Histograms partitioned by label values; all share bounds.
type HistogramVec struct {
	mu     sync.RWMutex
	bounds []float64
	series map[string]*histogramSeries
}

type histogramSeries struct {
	labels []string
	h      *Histogram
}

func newHistogramVec(bounds []float64) *HistogramVec {
	return &HistogramVec{bounds: bounds, series: map[string]*histogramSeries{}}
}

func (v *HistogramVec) Observe(val float64, labelValues ...string) {
	key := joinKey(labelValues)
	v.mu.RLock()
	s := v.series[key]
	v.mu.RUnlock()
	if s == nil {
		v.mu.Lock()
		if s = v.series[key]; s == nil {
			s = &histogramSeries{labels: append([]string(nil), labelValues...), h: newHistogram(v.bounds)}
			v.series[key] = s
		}
		v.mu.Unlock()
	}
	s.h.Observe(val)
}
