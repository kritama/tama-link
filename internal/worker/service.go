package worker

import (
	"context"
	"sync"
)

// queueDepth bounds queued-but-not-started submissions. The queue is a
// prompt-start aid only: every submission is already durable, so a full
// queue defers to startup recovery or a later dispatch and loses nothing.
const queueDepth = 256

// Service owns the leased local execution pipeline for one profile: startup
// recovery of pending replayable submissions and a dispatch loop for new
// ones. It owns every goroutine it starts: Stop cancels the loop and
// in-flight work and waits for shutdown. A submission is never executed
// concurrently twice in one process, and the durable lease keeps that true
// across processes.
type Service struct {
	runner   *Runner
	queue    chan string
	inFlight sync.Map // submission ID -> struct{}
	wg       sync.WaitGroup
	cancel   context.CancelFunc
	done     chan struct{}
}

// NewService validates cfg and starts the dispatch loop. The loop owns its
// context; Stop is the only shutdown path.
func NewService(state State, executor Executor, cfg Config) (*Service, error) {
	runner, err := New(state, executor, cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		runner: runner,
		queue:  make(chan string, queueDepth),
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
// It never blocks and never fails: the durable store owns the submission.
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
		}
	}
}
