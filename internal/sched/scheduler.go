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
	jobs []queuedJob
}

type queuedJob struct {
	ctx  context.Context
	job  Job
	done chan<- error
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
	done = make(chan error, 1)
	s.mu.Lock()
	q, ok := s.queues[job.Remote]
	if !ok {
		q = &remoteQueue{}
		q.cond = sync.NewCond(&q.mu)
		s.queues[job.Remote] = q
		for i := 0; i < s.MaxConcurrent; i++ {
			go s.run(q)
		}
	}
	s.mu.Unlock()

	q.mu.Lock()
	inserted := false
	queued := queuedJob{ctx: ctx, job: job, done: done}
	for i, j := range q.jobs {
		if job.Priority > j.job.Priority {
			q.jobs = append(q.jobs, queuedJob{})
			copy(q.jobs[i+1:], q.jobs[i:])
			q.jobs[i] = queued
			inserted = true
			break
		}
	}
	if !inserted {
		q.jobs = append(q.jobs, queued)
	}
	q.cond.Signal()
	q.mu.Unlock()
	return done
}

// run owns one execution slot for a remote queue. Jobs are dequeued only by
// these workers, so priority ordering is preserved even when Submit starts
// several calls concurrently.
func (s *Scheduler) run(q *remoteQueue) {
	for {
		q.mu.Lock()
		for len(q.jobs) == 0 {
			q.cond.Wait()
		}
		queued := q.jobs[0]
		q.jobs = q.jobs[1:]
		q.mu.Unlock()

		queued.done <- queued.job.Run(queued.ctx)
	}
}
