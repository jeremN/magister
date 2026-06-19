package metrics

import (
	"sync"
	"testing"
)

func TestCounterAdd(t *testing.T) {
	var c Counter
	c.Add(1)
	c.Add(2.5)
	if got := c.value(); got != 3.5 {
		t.Errorf("value = %v, want 3.5", got)
	}
}

func TestGaugeSet(t *testing.T) {
	var g Gauge
	g.Set(7)
	g.Set(4)
	if got := g.value(); got != 4 {
		t.Errorf("value = %v, want 4", got)
	}
}

func TestHistogramObserve(t *testing.T) {
	h := newHistogram([]float64{1, 5, 10})
	for _, v := range []float64{0.5, 1, 5, 7, 20} { // buckets: <=1:{0.5,1}, <=5:{5}, <=10:{7}, +Inf:{20}
		h.Observe(v)
	}
	if h.count.Load() != 5 {
		t.Errorf("count = %d, want 5", h.count.Load())
	}
	// raw (non-cumulative) bucket tallies
	if got := []uint64{h.buckets[0].Load(), h.buckets[1].Load(), h.buckets[2].Load()}; got[0] != 2 || got[1] != 1 || got[2] != 1 {
		t.Errorf("buckets = %v, want [2 1 1]", got)
	}
}

func TestCounterVecSeries(t *testing.T) {
	v := newCounterVec()
	v.Add(1, "succeeded")
	v.Add(1, "succeeded")
	v.Add(1, "failed")
	if len(v.series) != 2 {
		t.Fatalf("series = %d, want 2", len(v.series))
	}
	if got := v.series[joinKey([]string{"succeeded"})].c.value(); got != 2 {
		t.Errorf("succeeded = %v, want 2", got)
	}
}

func TestHistogramVecSeries(t *testing.T) {
	v := newHistogramVec([]float64{1, 5})
	v.Observe(0.5, "GET", "/x")
	v.Observe(2, "GET", "/x")
	v.Observe(0.5, "POST", "/y")
	if len(v.series) != 2 {
		t.Fatalf("series = %d, want 2", len(v.series))
	}
	if got := v.series[joinKey([]string{"GET", "/x"})].h.count.Load(); got != 2 {
		t.Errorf("GET /x count = %d, want 2", got)
	}
}

func TestCounterConcurrent(t *testing.T) {
	var c Counter
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := c.value(); got != 10000 {
		t.Errorf("value = %v, want 10000", got)
	}
}

func TestCounterVecConcurrent(t *testing.T) {
	v := newCounterVec()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				v.Add(1, "x")
			}
		}()
	}
	wg.Wait()
	if got := v.series[joinKey([]string{"x"})].c.value(); got != 5000 {
		t.Errorf("value = %v, want 5000", got)
	}
}
