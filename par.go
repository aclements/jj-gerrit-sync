// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"container/heap"
	"context"
	"iter"
	"sync"
)

// parApply calls fn on each element of in in parallel and yields (input,
// result) pairs in the same order as in.
//
// If the returned iterator is interrupted before it ends, it will cancel the
// context passed to fn and wait until all invocations of fn have completed.
func parApply[T, U any](ctx context.Context, fn func(context.Context, T) U, in iter.Seq[T], limit int) iter.Seq2[T, U] {
	// Define worker.
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(ctx)
	work := make(chan parApplyItem[T, U])
	results := make(chan parApplyItem[T, U], 1)
	worker := func() {
		var item parApplyItem[T, U]
		defer func() {
			p := recover()
			if p != nil {
				// Forward panic to the output loop
				item.panic = p
				select {
				case results <- item:
				case <-ctx.Done():
				}
			}
		}()
		for {
			var ok bool
			select {
			case item, ok = <-work:
				if !ok {
					return
				}
				item.value = fn(ctx, item.input)
				select {
				case results <- item:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}
	// Create and feed the worker pool.
	inCount := 0
	wg.Go(func() {
		defer close(work)
		n := 0
		for item := range in {
			if n < limit {
				// Start another worker. We do this lazily in case in < limit.
				wg.Go(worker)
				n++
			}
			select {
			case work <- parApplyItem[T, U]{index: inCount, input: item}:
				inCount++
			case <-ctx.Done():
				return
			}
		}
		// Signal completion of the input stream. After this, the output loop
		// owns inCount.
		select {
		case results <- parApplyItem[T, U]{index: -1}:
		case <-ctx.Done():
		}
	})

	// Feed results back to the caller.
	return func(yield func(T, U) bool) {
		defer func() {
			cancel()
			wg.Wait()
		}()
		// Keep a priority queue of results that have been returned but not yet
		// yielded.
		var pendingResults parApplyHeap[T, U]
		outCount := -1
		for i := 0; outCount == -1 || i < outCount; {
			select {
			case result := <-results:
				if result.index == -1 {
					// Signal from the input loop that we now know the output
					// count.
					outCount = inCount
				} else if result.panic != nil {
					panic(result.panic)
				} else {
					heap.Push(&pendingResults, result)
				}
			case <-ctx.Done():
				// While this is the only goroutine that can cancel the context
				// we created, our caller could cancel its context.
				return
			}

			// Pull results off the pending list.
			for len(pendingResults) > 0 && pendingResults[0].index == i {
				if !yield(pendingResults[0].input, pendingResults[0].value) {
					// Cancel remaining work and wait for everything to finish.
					return
				}
				heap.Pop(&pendingResults)
				i++
			}
		}
	}
}

type parApplyItem[T, U any] struct {
	index int
	input T
	value U
	panic any
}

type parApplyHeap[T, U any] []parApplyItem[T, U]

func (h parApplyHeap[T, U]) Len() int           { return len(h) }
func (h parApplyHeap[T, U]) Less(i, j int) bool { return h[i].index < h[j].index }
func (h parApplyHeap[T, U]) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *parApplyHeap[T, U]) Push(x any) {
	*h = append(*h, x.(parApplyItem[T, U]))
}

func (h *parApplyHeap[T, U]) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	var zero parApplyItem[T, U]
	old[n-1] = zero
	*h = old[0 : n-1]
	return x
}
