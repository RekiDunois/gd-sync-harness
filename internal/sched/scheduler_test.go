package sched

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPriorityOrdering(t *testing.T) {
	s := New(1)
	ctx := context.Background()
	var order []string
	var mu sync.Mutex

	// Hold the only slot while the jobs are submitted. Submit starts work
	// asynchronously, so without this barrier the first job could run before
	// the later, higher-priority jobs reach the queue.
	started := make(chan struct{})
	release := make(chan struct{})
	blockerDone := s.Submit(ctx, Job{Remote: "r", Priority: 100, Run: func(context.Context) error {
		close(started)
		<-release
		return nil
	}})
	<-started

	// Once all jobs are queued, higher priority should be dequeued first.
	done := make([]chan error, 4)
	done[0] = s.Submit(ctx, Job{Remote: "r", Priority: 1, Run: func(ctx context.Context) error {
		mu.Lock()
		order = append(order, "low")
		mu.Unlock()
		return nil
	}})
	done[1] = s.Submit(ctx, Job{Remote: "r", Priority: 10, Run: func(ctx context.Context) error {
		mu.Lock()
		order = append(order, "high1")
		mu.Unlock()
		return nil
	}})
	done[2] = s.Submit(ctx, Job{Remote: "r", Priority: 5, Run: func(ctx context.Context) error {
		mu.Lock()
		order = append(order, "mid")
		mu.Unlock()
		return nil
	}})
	done[3] = s.Submit(ctx, Job{Remote: "r", Priority: 10, Run: func(ctx context.Context) error {
		mu.Lock()
		order = append(order, "high2")
		mu.Unlock()
		return nil
	}})

	close(release)
	<-blockerDone
	for _, d := range done {
		<-d
	}
	time.Sleep(30 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 4 {
		t.Fatalf("order = %v", order)
	}
	want := []string{"high1", "high2", "mid", "low"}
	for i, w := range want {
		if order[i] != w {
			t.Errorf("order[%d] = %v, want %v (full: %v)", i, order[i], w, order)
		}
	}
}

func TestDifferentRemotesConcurrent(t *testing.T) {
	s := New(1) // per-remote cap of 1
	ctx := context.Background()
	var active int32
	var maxActive int32
	var mu sync.Mutex

	run := func() {
		a := atomic.AddInt32(&active, 1)
		mu.Lock()
		if a > maxActive {
			maxActive = a
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&active, -1)
	}

	// Two remotes should run concurrently even with per-remote cap 1.
	d1 := s.Submit(ctx, Job{Remote: "a", Priority: 1, Run: func(ctx context.Context) error { run(); return nil }})
	d2 := s.Submit(ctx, Job{Remote: "b", Priority: 1, Run: func(ctx context.Context) error { run(); return nil }})
	<-d1
	<-d2

	if maxActive < 2 {
		t.Errorf("maxActive = %d, want >= 2 (independent remotes)", maxActive)
	}
}

func TestBoundedConcurrencyPerRemote(t *testing.T) {
	s := New(2) // cap 2 per remote
	ctx := context.Background()
	var active int32
	var maxActive int32
	var mu sync.Mutex

	run := func() {
		a := atomic.AddInt32(&active, 1)
		mu.Lock()
		if a > maxActive {
			maxActive = a
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&active, -1)
	}

	done := make([]chan error, 6)
	for i := 0; i < 6; i++ {
		done[i] = s.Submit(ctx, Job{Remote: "same", Priority: 1, Run: func(ctx context.Context) error { run(); return nil }})
	}
	for _, d := range done {
		<-d
	}
	if maxActive > 2 {
		t.Errorf("maxActive = %d, want <= 2", maxActive)
	}
}
