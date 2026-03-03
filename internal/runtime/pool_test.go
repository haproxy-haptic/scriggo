// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"runtime"
	"sync"
	"testing"
)

func TestChanPool_GetPut(t *testing.T) {
	p := newChanPool(4, func() *int {
		v := 42
		return &v
	})

	// Put an object, then get it back.
	val := 100
	p.put(&val)

	got := p.get()
	if got != &val {
		t.Errorf("expected same pointer back from pool, got different object")
	}
	if *got != 100 {
		t.Errorf("expected value 100, got %d", *got)
	}
}

func TestChanPool_NewOnEmpty(t *testing.T) {
	calls := 0
	p := newChanPool(4, func() int {
		calls++
		return calls
	})

	// Pool is empty, should call newF.
	v1 := p.get()
	v2 := p.get()
	if v1 != 1 || v2 != 2 {
		t.Errorf("expected 1, 2 from newF, got %d, %d", v1, v2)
	}
	if calls != 2 {
		t.Errorf("expected 2 newF calls, got %d", calls)
	}
}

func TestChanPool_DiscardOnFull(t *testing.T) {
	p := newChanPool(2, func() int { return 0 })

	// Fill the pool.
	p.put(1)
	p.put(2)
	// This should not block — excess is discarded.
	p.put(3)

	// Should get back 1 and 2 (FIFO from channel).
	v1 := p.get()
	v2 := p.get()
	if v1 != 1 || v2 != 2 {
		t.Errorf("expected 1, 2 from full pool, got %d, %d", v1, v2)
	}
}

func TestChanPool_Drain(t *testing.T) {
	p := newChanPool(10, func() int { return 0 })

	for i := 0; i < 5; i++ {
		p.put(i)
	}
	p.drain()

	// After drain, pool should be empty — next get uses newF.
	calls := 0
	p2 := newChanPool(10, func() int {
		calls++
		return -1
	})
	// Drain an already-empty pool (should not panic).
	p2.drain()

	// Verify original pool is empty by checking channel length.
	if len(p.ch) != 0 {
		t.Errorf("expected empty pool after drain, got %d items", len(p.ch))
	}
}

func TestChanPool_Concurrent(t *testing.T) {
	p := newChanPool(64, func() *int {
		v := 0
		return &v
	})

	const goroutines = 100
	const opsPerGoroutine = 1000

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				v := p.get()
				*v = j
				p.put(v)
			}
		}()
	}
	wg.Wait()
}

// BenchmarkPool_ChanPool benchmarks chanPool get/put throughput.
func BenchmarkPool_ChanPool(b *testing.B) {
	p := newChanPool(128, func() *int {
		v := 0
		return &v
	})

	// Pre-fill so we measure channel hit path, not allocation.
	for i := 0; i < 128; i++ {
		v := 0
		p.put(&v)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		v := p.get()
		p.put(v)
	}
}

// BenchmarkPool_SyncPool benchmarks sync.Pool get/put throughput for comparison.
func BenchmarkPool_SyncPool(b *testing.B) {
	p := &sync.Pool{
		New: func() interface{} {
			v := 0
			return &v
		},
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		v := p.Get().(*int)
		p.Put(v)
	}
}

// BenchmarkPool_ChanPool_Parallel benchmarks chanPool under parallel load.
func BenchmarkPool_ChanPool_Parallel(b *testing.B) {
	p := newChanPool(runtime.GOMAXPROCS(0)*2, func() *int {
		v := 0
		return &v
	})

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			v := p.get()
			p.put(v)
		}
	})
}

// BenchmarkPool_SyncPool_Parallel benchmarks sync.Pool under parallel load.
func BenchmarkPool_SyncPool_Parallel(b *testing.B) {
	p := &sync.Pool{
		New: func() interface{} {
			v := 0
			return &v
		},
	}

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			v := p.Get().(*int)
			p.Put(v)
		}
	})
}

// BenchmarkPool_ChanPool_AfterGC benchmarks chanPool after a GC cycle.
// This is the key scenario: chanPool should retain objects while sync.Pool loses them.
func BenchmarkPool_ChanPool_AfterGC(b *testing.B) {
	p := newChanPool(128, func() *int {
		v := 0
		return &v
	})

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		// Pre-fill pool.
		for j := 0; j < 64; j++ {
			v := 0
			p.put(&v)
		}
		// Force GC — chanPool should retain objects.
		runtime.GC()
		// Get should return pooled objects (no allocation).
		for j := 0; j < 64; j++ {
			v := p.get()
			_ = v
		}
	}
}

// BenchmarkPool_SyncPool_AfterGC benchmarks sync.Pool after a GC cycle.
// sync.Pool loses objects after GC, so every get() allocates.
func BenchmarkPool_SyncPool_AfterGC(b *testing.B) {
	p := &sync.Pool{
		New: func() interface{} {
			v := 0
			return &v
		},
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		// Pre-fill pool.
		for j := 0; j < 64; j++ {
			v := 0
			p.Put(&v)
		}
		// Force GC — sync.Pool evicts objects after 2 cycles.
		runtime.GC()
		runtime.GC() // Second GC to clear victim cache.
		// Get will allocate new objects.
		for j := 0; j < 64; j++ {
			v := p.Get().(*int)
			_ = v
		}
	}
}
