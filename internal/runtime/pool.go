// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "sync"

// retainedPool is an object pool whose contents the garbage collector never
// evicts.
//
// It exists because sync.Pool does not fit this workload, which was measured
// rather than assumed: sync.Pool's poolCleanup runs at every GC STW pause and
// drops every entry, so with long idle gaps between renders (30-120s) the pool
// is always empty when it is next needed. A production profile of the haptic
// controller showed sync.Pool.pinSlow at 51% of all allocations and 79% of CPU
// in GC. sync.Pool remains ~3x cheaper per operation in a microbenchmark and is
// still the wrong choice here — do not re-litigate it on a sequential
// benchmark alone; BenchmarkPool_SyncPool_AfterGC is the one that matters.
//
// The pool holds its own references, so nothing is ever evicted. A mutex and a
// slice back it. That was a buffered channel until closure calls started
// borrowing a VM per call: a channel send/recv is ~33ns against ~9ns for the
// mutex, and under a load shaped like a real render — work between borrow and
// return, rather than a hammer loop — the mutex also wins in parallel
// (144ns vs 177ns). A hammer loop reverses that result, which is why the
// benchmarks put work between the two calls.
//
// Sharding the mutex was tried and is much worse: get and put pick shards
// independently, so gets land on empty shards and allocate. Per-shard affinity
// would need per-P caching, which is sync.Pool.
type retainedPool[T any] struct {
	mu   sync.Mutex
	buf  []T
	max  int
	newF func() T
}

// newRetainedPool creates a pool retaining at most capacity idle objects.
func newRetainedPool[T any](capacity int, newFunc func() T) *retainedPool[T] {
	return &retainedPool[T]{
		buf:  make([]T, 0, capacity),
		max:  capacity,
		newF: newFunc,
	}
}

// get returns a pooled object, or a new one when the pool is empty.
func (p *retainedPool[T]) get() T {
	p.mu.Lock()
	if n := len(p.buf); n > 0 {
		v := p.buf[n-1]
		var zero T
		// Drop the pool's own reference so a taken object is not kept alive
		// by the slice's backing array after the pool shrinks past it.
		p.buf[n-1] = zero
		p.buf = p.buf[:n-1]
		p.mu.Unlock()
		return v
	}
	p.mu.Unlock()
	return p.newF()
}

// put returns an object to the pool, discarding it when the pool is full.
func (p *retainedPool[T]) put(v T) {
	p.mu.Lock()
	if len(p.buf) < p.max {
		p.buf = append(p.buf, v)
	}
	p.mu.Unlock()
}

// drain removes all objects from the pool, allowing them to be garbage
// collected. Used by ClearVMPool.
func (p *retainedPool[T]) drain() {
	p.mu.Lock()
	clear(p.buf)
	p.buf = p.buf[:0]
	p.mu.Unlock()
}
