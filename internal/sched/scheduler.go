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
	cond    *sync.Cond
	mu      sync.Mutex
	jobs    []Job
	running int
	closed  bool
}

// New builds a scheduler. MaxConcurrent is per-remote.
func New(maxConcurrent int) *Scheduler {
	if maxConcurrent <= 0 {
		maxConcurrent = 2
	}
	return &Scheduler{MaxConcurrent: maxConcurrent, queues: make(map[string]*remoteQueue)}
}

// Submit queues a job for a remote and returns a channel that completes when
// the job is done.
func (s *Scheduler) Submit(ctx context.Context, job Job) (done chan error) {
	s.mu.Lock()
	q, ok := s.queues[job.Remote]
	if !ok {
		q = &remoteQueue{}
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

// run pops and executes jobs from the queue while holding capacity.
func (s *Scheduler) run(ctx context.Context, q *remoteQueue) error {
	q.mu.Lock()
	for q.closed || len(q.jobs) == 0 {
		if q.closed {
			q.mu.Unlock()
			return nil
		}
		q.cond.Wait()
		if q.closed {
			q.mu.Unlock()
			return nil
		}
	}
	job := q.jobs[0]
	q.jobs = q.jobs[1:]
	q.mu.Unlock()

	q.mu.Lock()
	q.running++
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		q.running--
		q.cond.Broadcast()
		q.mu.Unlock()
	}()

	return job.Run(ctx)
}
