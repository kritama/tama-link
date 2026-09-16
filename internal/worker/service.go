package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// queueDepth bounds queued-but-not-started submissions. The queue is a
// prompt-start aid only: every submission is already durable, so a saturated
// queue never loses work — the recurring durable sweep rediscovers it.
const queueDepth = 256

// defaultSweepInterval bounds how long an accepted submission can wait to be
// rediscovered from the durable store when the in-memory queue was saturated.
const defaultSweepInterval = 5 * time.Second

// defaultMaxInFlight bounds how many submissions this process executes at
// once. The prompt queue bounds queued work; this bound protects the store
// writer and the upstream connection pool when a sweep or burst starts many
// executions at once.
const defaultMaxInFlight = 8

// Service owns the leased local execution pipeline for one profile: startup
// recovery of pending replayable submissions, a dispatch loop for new ones,
// and a recurring durable sweep that re-derives runnable work from the
// store. It owns every goroutine it starts: Stop cancels the loop and
// in-flight work and waits for shutdown. At most Config.MaxInFlight
// submissions execute concurrently; the rest wait for a slot. A submission
// is never executed concurrently twice in one process, and the durable
// lease keeps that true across processes.
type Service struct {
	runner        *Runner
	queue         chan string
	slots         chan struct{}
	sweep         time.Duration
	inFlight      sync.Map // submission ID -> struct{}
	dispatchDrops atomic.Int64
	wg            sync.WaitGroup
	cancel        context.CancelFunc
	done          chan struct{}
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
	maxInFlight := cfg.MaxInFlight
	if maxInFlight <= 0 {
		maxInFlight = defaultMaxInFlight
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		runner: runner,
		queue:  make(chan string, queueDepth),
		slots:  make(chan struct{}, maxInFlight),
		sweep:  sweep,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go s.loop(ctx)
	return s, nil
}

// Start recovers pending replayable submissions after a restart or crash
// without delaying serve: it lists the durable runnable set (one store
// read) and offers each ID to the bounded pool, where execution happens
// asynchronously in the background. Live leases are skipped: their owning
// process remains responsible. Anything not yet accepted by the pool is
// redelivered by the recurring sweep, so no work waits inline for an
// upstream call before the MCP server starts accepting clients.
func (s *Service) Start(ctx context.Context) error {
	s.sweepRunnable(ctx)
	return nil
}

// Dispatch asks the service to execute one submission when a worker is free.
// It never blocks and never fails: the in-memory queue is a prompt-start aid
// only. If it is saturated the ID is dropped from the queue alone — the
// recurring durable sweep rediscovers every accepted replayable submission,
// and the lease remains the final single-winner guard.
func (s *Service) Dispatch(id string) {
	s.Offer(id)
}

// Offer places one submission ID on the prompt-start queue and reports
// whether the queue accepted it. A rejected offer is not lost work: the ID
// remains durable and a sweep — recurring or explicit — rediscovers it. The
// boolean exposes the queue decision for callers that need it; production
// callers use Dispatch, which ignores it.
func (s *Service) Offer(id string) bool {
	select {
	case s.queue <- id:
		return true
	default:
		s.dispatchDrops.Add(1)
		return false
	}
}

// Sweep offers every runnable replayable submission from the durable store
// to the prompt queue once. The dispatch loop calls it on a fixed cadence;
// callers may also invoke it explicitly, for example right after a
// saturation event, to shorten the recovery wait. Duplicates are absorbed
// by the in-flight guard, and the lease still decides single-winner
// ownership across processes.
func (s *Service) Sweep(ctx context.Context) {
	s.sweepRunnable(ctx)
}

// DroppedDispatches reports how many prompt dispatches a saturated queue has
// dropped since the service started. Dropped IDs never leave the durable
// store: the recurring sweep rediscovers them, so the counter measures queue
// saturation, not lost work.
func (s *Service) DroppedDispatches() int64 {
	return s.dispatchDrops.Load()
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
				// Already running. (Parked slot-waiters do not hold the
				// mark, so they can be offered again by the sweep; the
				// duplicate is absorbed at claim time.)
				continue
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				// The slot is taken before any lease claim or store write,
				// so a full pool parks goroutines (which cost little) while
				// the number of live SQLite transactions and upstream calls
				// stays bounded. While parked the ID is not in-flight: a
				// sweep may deliver it again, and the duplicate is absorbed
				// when this goroutine later starts (the claim rejects the
				// already-leased run, or the first writer wins the
				// transition). Without the release, a full pool would hold
				// queued-but-parked IDs against the sweep and starve IDs
				// dropped on a saturated queue.
				defer s.inFlight.Delete(id)
				select {
				case s.slots <- struct{}{}:
					defer func() { <-s.slots }()
				case <-ctx.Done():
					return
				}
				s.inFlight.Store(id, struct{}{})
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
		if _, loaded := s.inFlight.Load(id); loaded {
			// Already executing in this process. Re-offering in-flight IDs
			// would let them crowd the queue ahead of IDs that were
			// dropped, so the sweep only offers work this process is not
			// already running.
			continue
		}
		select {
		case s.queue <- id:
		default:
			// The queue is still saturated; the next sweep retries.
		}
	}
}
