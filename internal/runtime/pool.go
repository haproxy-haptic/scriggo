// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// chanPool is a GC-resistant object pool backed by a buffered channel.
//
// Unlike sync.Pool, objects in a chanPool are never evicted by the garbage
// collector. sync.Pool's poolCleanup runs at every GC STW pause and evicts
// all entries after 2 cycles. For workloads with long idle gaps between
// pool access (e.g. template renders every 30-120s), this means the pool
// is always empty when needed, causing expensive re-allocation.
//
// A buffered channel holds references that the GC scans but never evicts,
// providing stable object reuse across GC cycles. The trade-off is a single
// lock (channel mutex) instead of sync.Pool's per-P lock-free fast path,
// which is acceptable when pool access frequency is low (hundreds of ops
// per render, not millions per second).
type chanPool[T any] struct {
	ch   chan T
	newF func() T
}

// newChanPool creates a pool with the given capacity and constructor.
// Capacity determines the maximum number of idle objects retained.
func newChanPool[T any](capacity int, newFunc func() T) *chanPool[T] {
	return &chanPool[T]{
		ch:   make(chan T, capacity),
		newF: newFunc,
	}
}

// get returns a pooled object or creates a new one if the pool is empty.
func (p *chanPool[T]) get() T {
	select {
	case v := <-p.ch:
		return v
	default:
		return p.newF()
	}
}

// put returns an object to the pool. If the pool is full, the object is
// discarded (eligible for GC).
func (p *chanPool[T]) put(v T) {
	select {
	case p.ch <- v:
	default:
		// Pool full, discard.
	}
}

// drain removes all objects from the pool, allowing them to be garbage
// collected. Used by ClearVMPool.
func (p *chanPool[T]) drain() {
	for {
		select {
		case <-p.ch:
		default:
			return
		}
	}
}
