package worker

import (
	"context"
	"sync"
	"time"
)

// queueDepth bounds queued-but-not-started submissions. The queue is a
// prompt-start aid only: every submission is already durable, so a saturated
// queue never loses work — the recurring durable sweep rediscovers it.
const queueDepth = 256

// defaultSweepInterval bounds how long an accepted submission can wait to be
// rediscovered from the durable store when the in-memory queue was saturated.
const defaultSweepInterval = 5 * time.Second

// Service owns the leased local execution pipeline for one profile: startup
// recovery of pending replayable submissions, a dispatch loop for new ones,
// and a recurring durable sweep that re-derives runnable work from the
// store. It owns every goroutine it starts: Stop cancels the loop and
// in-flight work and waits for shutdown. A submission is never executed
// concurrently twice in one process, and the durable lease keeps that true
// across processes.
type Service struct {
	runner   *Runner
	queue    chan string
	sweep    time.Duration
	inFlight sync.Map // submission ID -> struct{}
	wg       sync.WaitGroup
	cancel   context.CancelFunc
	done     chan struct{}
}

// NewService validates cfg and starts the dispatch and sweep loop. The loop
// owns its context; Stop is the only shutdown path.
func NewService(state State, executor Executor, cfg Config) (*Service, error) {
	runner, err := New(state, executor, cfg)
	if err != nil {
		return nil, err
	}
	sweep := cfg.SweepInterval
	if sweep < time.Millisecond {
		sweep = defaultSweepInterval
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		runner: runner,
		queue:  make(chan string, queueDepth),
		sweep:  sweep,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go s.loop(ctx)
	return s, nil
}

// Start recovers pending replayable submissions after a restart or crash.
// Live leases are skipped: their owning process remains responsible.
func (s *Service) Start(ctx context.Context) error {
	return s.runner.Recover(ctx)
}

// Dispatch asks the service to execute one submission when a worker is free.
// It never blocks and never fails: the in-memory queue is a prompt-start aid
// only. If it is saturated the ID is dropped from the queue alone — the
// recurring durable sweep rediscovers every accepted replayable submission,
// and the lease remains the final single-winner guard.
func (s *Service) Dispatch(id string) {
	select {
	case s.queue <- id:
	default:
	}
}

// Stop cancels the loop and in-flight execution and waits for shutdown.
func (s *Service) Stop() {
	s.cancel()
	<-s.done
	s.wg.Wait()
}

// Run executes one submission under the lease. Exposed for tests and for
// callers that own execution synchronously.
func (s *Service) Run(ctx context.Context, id string) error {
	return s.runner.Run(ctx, id)
}

func (s *Service) loop(ctx context.Context) {
	ticker := time.NewTicker(s.sweep)
	defer ticker.Stop()
	defer close(s.done)
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-s.queue:
			if id == "" {
				continue
			}
			if _, loaded := s.inFlight.LoadOrStore(id, struct{}{}); loaded {
				// Already running: the lease would reject a duplicate
				// anyway, and skipping avoids the claim round trip.
				continue
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer func() { s.inFlight.Delete(id) }()
				_ = s.runner.Run(ctx, id)
			}()
		case <-ticker.C:
			s.sweepRunnable(ctx)
		}
	}
}

// sweepRunnable re-derives every runnable replayable submission from the
// durable store and offers each one to the queue. This is what makes a
// saturated queue lossless: an ID dropped on a full queue is rediscovered on
// the next sweep. Duplicates are absorbed by the in-flight guard, and the
// lease still decides single-winner ownership across processes.
func (s *Service) sweepRunnable(ctx context.Context) {
	ids, err := s.runner.Runnable(ctx)
	if err != nil {
		// The store was unavailable for this sweep; the next tick retries
		// and in-flight executions keep their own leases alive.
		return
	}
	for _, id := range ids {
		select {
		case s.queue <- id:
		default:
			// The queue is still saturated; the next sweep retries.
		}
	}
}
