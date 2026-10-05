// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"container/heap"
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestParApply_InOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const count = 50
		in := make([]int, count)
		for i := range in {
			in[i] = i
		}

		// fn introduces delays inversely proportional to x so results arrive in reverse order.
		fn := func(ctx context.Context, x int) int {
			time.Sleep(time.Duration(count-x) * time.Second)
			return x * 2
		}

		var gotIn, gotOut []int
		for k, v := range parApply(t.Context(), fn, slices.Values(in), 4) {
			gotIn = append(gotIn, k)
			gotOut = append(gotOut, v)
		}

		if len(gotIn) != count || len(gotOut) != count {
			t.Fatalf("got %d in, %d out, want %d", len(gotIn), len(gotOut), count)
		}
		for i := 0; i < count; i++ {
			if gotIn[i] != i {
				t.Fatalf("gotIn[%d] = %d, want %d", i, gotIn[i], i)
			}
			if want := i * 2; gotOut[i] != want {
				t.Fatalf("gotOut[%d] = %d, want %d", i, gotOut[i], want)
			}
		}
	})
}

func TestParApply_Empty(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := slices.Values([]int{})
		called := false
		for range parApply(t.Context(), func(ctx context.Context, x int) int { return x }, in, 4) {
			called = true
		}
		if called {
			t.Fatal("expected no yields for empty input")
		}
	})
}

func TestParApply_FewerThanLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := []int{10, 20, 30}
		var gotIn, gotOut []int
		for k, v := range parApply(t.Context(), func(ctx context.Context, x int) int { return x + 1 }, slices.Values(in), 10) {
			gotIn = append(gotIn, k)
			gotOut = append(gotOut, v)
		}
		if !slices.Equal(gotIn, in) {
			t.Fatalf("got inputs %v, want %v", gotIn, in)
		}
		wantOut := []int{11, 21, 31}
		if !slices.Equal(gotOut, wantOut) {
			t.Fatalf("got outputs %v, want %v", gotOut, wantOut)
		}
	})
}

func TestParApply_EarlyBreak(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const count = 100
		in := make([]int, count)
		for i := range in {
			in[i] = i
		}

		var processed atomic.Int32
		var gotIn, gotOut []int
		for k, v := range parApply(t.Context(), func(ctx context.Context, x int) int {
			processed.Add(1)
			time.Sleep(time.Second)
			return x * 10
		}, slices.Values(in), 4) {
			gotIn = append(gotIn, k)
			gotOut = append(gotOut, v)
			if len(gotIn) == 5 {
				break
			}
		}

		if len(gotIn) != 5 {
			t.Fatalf("got %d results, want 5", len(gotIn))
		}
		wantIn := []int{0, 1, 2, 3, 4}
		wantOut := []int{0, 10, 20, 30, 40}
		if !slices.Equal(gotIn, wantIn) {
			t.Fatalf("gotIn %v, want %v", gotIn, wantIn)
		}
		if !slices.Equal(gotOut, wantOut) {
			t.Fatalf("gotOut %v, want %v", gotOut, wantOut)
		}
	})
}

func TestParApply_Panic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := []int{1, 2, 3, 4, 5}

		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected panic, got nil")
			}
			if s, ok := r.(string); !ok || s != "panic on 3" {
				t.Fatalf("unexpected panic value: %v", r)
			}
		}()

		for range parApply(t.Context(), func(ctx context.Context, x int) int {
			if x == 3 {
				panic("panic on 3")
			}
			return x
		}, slices.Values(in), 2) {
		}
	})
}

func TestParApply_PanicContextDone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		worker1Sent := make(chan struct{})
		in := []int{0, 1, 2}
		for k := range parApply(ctx, func(c context.Context, x int) int {
			switch x {
			case 0:
				return 0
			case 1:
				defer close(worker1Sent)
				return 1
			case 2:
				<-worker1Sent
				// Let worker 1 send its result into the 1-slot results channel first.
				time.Sleep(time.Second)
				panic("boom")
			}
			return x
		}, slices.Values(in), 3) {
			if k == 0 {
				// While inside yield for item 0, the output loop is paused.
				// Worker 1 filled the 1-slot results channel.
				// Worker 2 panicked and is durably blocked in select trying to send to results.
				synctest.Wait()
				// Cancel context while Worker 2 is blocked on results <-.
				cancel()
				break
			}
		}
	})
}

func TestParApply_CallerContextCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())

		started := make(chan struct{})
		go func() {
			<-started
			cancel()
		}()

		in := []int{1, 2, 3}
		for range parApply(ctx, func(c context.Context, x int) int {
			if x == 1 {
				close(started)
				<-c.Done()
			}
			return x
		}, slices.Values(in), 2) {
		}
	})
}

func TestParApply_FeederContextDone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())

		// Generator that never stops yielding until cancelled.
		infiniteSeq := func(yield func(int) bool) {
			for i := 0; ; i++ {
				if !yield(i) {
					return
				}
			}
		}

		started := make(chan struct{})
		go func() {
			<-started
			// Block until the feeder is durably blocked on sending to work.
			synctest.Wait()
			cancel()
		}()

		for range parApply(ctx, func(c context.Context, x int) int {
			if x == 0 {
				close(started)
			}
			<-c.Done()
			return x
		}, infiniteSeq, 1) {
		}
	})
}

func TestParApply_WorkerContextDone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())

		// Sequence with 2 items. We start 2 workers.
		in := func(yield func(int) bool) {
			if !yield(0) {
				return
			}
			if !yield(1) {
				return
			}
			// Now block before closing work, so idle worker waits on <-work.
			<-ctx.Done()
		}

		worker0Done := make(chan struct{})
		go func() {
			<-worker0Done
			// Wait until worker 0 is durably blocked on <-work.
			synctest.Wait()
			cancel()
		}()

		for range parApply(ctx, func(c context.Context, x int) int {
			if x == 0 {
				close(worker0Done)
				return 0
			}
			<-c.Done()
			return 1
		}, in, 2) {
		}
	})
}

func TestParApplyHeap(t *testing.T) {
	var h parApplyHeap[int, string]
	if h.Len() != 0 {
		t.Fatalf("expected Len 0, got %d", h.Len())
	}

	packets := []parApplyItem[int, string]{
		{index: 3, input: 30, value: "three"},
		{index: 1, input: 10, value: "one"},
		{index: 4, input: 40, value: "four"},
		{index: 2, input: 20, value: "two"},
	}

	for _, p := range packets {
		heap.Push(&h, p)
	}

	if h.Len() != 4 {
		t.Fatalf("expected Len 4, got %d", h.Len())
	}

	h.Swap(0, 1)
	if !h.Less(1, 0) && !h.Less(0, 1) {
		t.Fatal("expected elements to be ordered")
	}
	heap.Init(&h)

	var result []int
	var inputs []int
	for h.Len() > 0 {
		p := heap.Pop(&h).(parApplyItem[int, string])
		result = append(result, p.index)
		inputs = append(inputs, p.input)
	}

	wantResult := []int{1, 2, 3, 4}
	wantInputs := []int{10, 20, 30, 40}
	if !slices.Equal(result, wantResult) {
		t.Fatalf("popped indices = %v, want %v", result, wantResult)
	}
	if !slices.Equal(inputs, wantInputs) {
		t.Fatalf("popped inputs = %v, want %v", inputs, wantInputs)
	}
}
