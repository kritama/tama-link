package application

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kritama/tama-link/internal/adapter/tama2026"
	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/store"
)

const (
	taskQueueDepth    = 256
	taskSweepInterval = 5 * time.Second
	taskMaxInFlight   = 8
	taskPollFloor     = 20 * time.Millisecond
)

// SessionCredentials is the in-memory access-token lifetime the task
// subscription owner needs. Expiry does not refresh. Refresh forces one
// exchange even when a token is cached.
type SessionCredentials interface {
	Expiry() (time.Time, bool)
	Refresh(context.Context) (string, error)
}

// TaskConfig identifies one App-task runner and its lease.
type TaskConfig struct {
	Owner    string
	LeaseTTL time.Duration
	// SweepInterval bounds rediscovery of durable task submissions. Zero
	// selects the default.
	SweepInterval time.Duration
	// MaxInFlight bounds concurrent task drives. Zero selects the default.
	MaxInFlight int
	// Credentials bounds subscription streams by access-token expiry and
	// refreshes before a stream is reopened. Nil skips the bound; production
	// wiring must set it.
	Credentials SessionCredentials
	// Now is the clock used to compare token expiry. Zero uses time.Now.
	Now func() time.Time
	// After waits for a duration. Zero uses time.After. Tests use it to fire
	// credential expiry without sleeping.
	After func(time.Duration) <-chan time.Time
}

// TaskService owns the leased App-task pipeline for one profile. It is not
// the System worker: a recovered task is resumed by owner-bound tasks/get,
// and tools/call is replayed only for the ambiguous no-handle case.
type TaskService struct {
	runner   *taskRunner
	store    *store.Store
	queue    chan string
	slots    chan struct{}
	parked   chan struct{}
	sweep    time.Duration
	inFlight sync.Map
	drops    atomic.Int64
	wg       sync.WaitGroup
	cancel   context.CancelFunc
	done     chan struct{}
}

// NewTaskService starts the App-task dispatch loop. Stop is its shutdown path.
func NewTaskService(
	st *store.Store,
	connect func(context.Context) (*tama2026.Connection, error),
	cfg TaskConfig,
) (*TaskService, error) {
	runner, err := newTaskRunner(st, connect, cfg)
	if err != nil {
		return nil, err
	}
	sweep := cfg.SweepInterval
	if sweep < time.Millisecond {
		sweep = taskSweepInterval
	}
	maxInFlight := cfg.MaxInFlight
	if maxInFlight <= 0 {
		maxInFlight = taskMaxInFlight
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &TaskService{
		runner: runner,
		store:  st,
		queue:  make(chan string, taskQueueDepth),
		slots:  make(chan struct{}, maxInFlight),
		parked: make(chan struct{}, maxInFlight),
		sweep:  sweep,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go s.loop(ctx)
	return s, nil
}

// Start offers every durable non-terminal App task to the pool and returns
// without waiting for upstream calls.
func (s *TaskService) Start(ctx context.Context) error {
	s.sweepRunnable(ctx)
	return nil
}

// Dispatch offers one accepted task submission. A saturated queue does not
// lose work: the sweep rediscovers it.
func (s *TaskService) Dispatch(id string) {
	select {
	case s.queue <- id:
	default:
		s.drops.Add(1)
	}
}

// Stop cancels in-flight drives and waits for them to release their leases.
// It does not cancel upstream tasks.
func (s *TaskService) Stop() {
	s.cancel()
	<-s.done
	s.wg.Wait()
}

func (s *TaskService) loop(ctx context.Context) {
	ticker := time.NewTicker(s.sweep)
	defer ticker.Stop()
	defer close(s.done)
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-s.queue:
			s.offer(ctx, id)
		case <-ticker.C:
			s.sweepRunnable(ctx)
		}
	}
}

func (s *TaskService) offer(ctx context.Context, id string) {
	if id == "" {
		return
	}
	if _, loaded := s.inFlight.LoadOrStore(id, struct{}{}); loaded {
		return
	}
	s.wg.Add(1)
	holdsSlot := false
	select {
	case s.slots <- struct{}{}:
		holdsSlot = true
	default:
	}
	if !holdsSlot {
		select {
		case s.parked <- struct{}{}:
		default:
			s.inFlight.Delete(id)
			s.wg.Done()
			return
		}
	}
	go s.execute(ctx, id, holdsSlot)
}

func (s *TaskService) execute(ctx context.Context, id string, holdsSlot bool) {
	defer s.wg.Done()
	defer s.inFlight.Delete(id)
	if !holdsSlot {
		defer func() { <-s.parked }()
		select {
		case s.slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
	}
	defer func() { <-s.slots }()
	_ = s.runner.Run(ctx, id)
}

func (s *TaskService) sweepRunnable(ctx context.Context) {
	ids, err := s.store.ListByStatus(ctx, string(catalog.StrategyUpstreamTask), []contract.Status{
		contract.StatusAccepted,
		contract.StatusQueued,
		contract.StatusRunning,
		contract.StatusInputRequired,
	})
	if err != nil {
		return
	}
	for _, id := range ids {
		if _, loaded := s.inFlight.Load(id); loaded {
			continue
		}
		select {
		case s.queue <- id:
		default:
		}
	}
}
