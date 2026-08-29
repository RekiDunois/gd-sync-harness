package sched

import (
	"context"
	"sync"
)

// Job is a unit of work queued for a remote.
type Job struct {
	Remote   string
	Priority int // higher = scheduled first
	Run      func(ctx context.Context) error
}

// Scheduler enforces bounded concurrency per remote (§20.2).
type Scheduler struct {
	MaxConcurrent int
	mu            sync.Mutex
	queues        map[string]*remoteQueue
}

type remoteQueue struct {
	mu   sync.Mutex
	cond *sync.Cond
	jobs []Job
	// slots is a counting semaphore for concurrent executions on this remote.
	slots chan struct{}
}

// New builds a scheduler. MaxConcurrent is per-remote.
func New(maxConcurrent int) *Scheduler {
	if maxConcurrent <= 0 {
		maxConcurrent = 2
	}
	return &Scheduler{MaxConcurrent: maxConcurrent, queues: make(map[string]*remoteQueue)}
}

// Submit queues a job for a remote and returns a channel that completes when
// the job is done. Priority sorts within the remote queue (higher first).
func (s *Scheduler) Submit(ctx context.Context, job Job) (done chan error) {
	s.mu.Lock()
	q, ok := s.queues[job.Remote]
	if !ok {
		q = &remoteQueue{slots: make(chan struct{}, s.MaxConcurrent)}
		q.cond = sync.NewCond(&q.mu)
		s.queues[job.Remote] = q
	}
	s.mu.Unlock()

	q.mu.Lock()
	inserted := false
	for i, j := range q.jobs {
		if job.Priority > j.Priority {
			q.jobs = append(q.jobs, Job{})
			copy(q.jobs[i+1:], q.jobs[i:])
			q.jobs[i] = job
			inserted = true
			break
		}
	}
	if !inserted {
		q.jobs = append(q.jobs, job)
	}
	q.cond.Signal()
	q.mu.Unlock()

	done = make(chan error, 1)
	go func() {
		err := s.run(ctx, q)
		done <- err
	}()
	return done
}

// run pops and executes a single job, acquiring a per-remote slot. The caller
// (per Submit) runs exactly one job; the queue coordinates the rest.
func (s *Scheduler) run(ctx context.Context, q *remoteQueue) error {
	q.mu.Lock()
	for len(q.jobs) == 0 {
		q.cond.Wait()
		if ctx.Err() != nil {
			q.mu.Unlock()
			return ctx.Err()
		}
	}
	job := q.jobs[0]
	q.jobs = q.jobs[1:]
	q.mu.Unlock()

	select {
	case q.slots <- struct{}{}:
	case <-ctx.Done():
		// Requeue at front and return.
		q.mu.Lock()
		q.jobs = append([]Job{job}, q.jobs...)
		q.cond.Signal()
		q.mu.Unlock()
		return ctx.Err()
	}
	defer func() { <-q.slots }()
	return job.Run(ctx)
}
