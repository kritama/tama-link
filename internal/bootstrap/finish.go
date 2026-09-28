package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/kritama/tama-link/internal/profile"
)

func (s *Service) finish(ctx context.Context, req Request, cand candidate, record journal) (*Result, error) {
	if loaded, err := profile.Load(cand.Name, s.opts.ConfigDir); err == nil {
		return s.completePublished(ctx, cand, loaded)
	} else if !errors.Is(err, profile.ErrNotFound) {
		if _, statErr := os.Stat(profile.Path(s.opts.ConfigDir, cand.Name)); statErr == nil {
			return nil, failErr(fmt.Errorf("%w: existing profile was preserved", profile.ErrExists))
		}
		return nil, incomplete(err)
	}
	shell := shellProfile(cand)
	owner, err := newOwner()
	if err != nil {
		return nil, failErr(err)
	}
	session, record, generation, err := s.openClaimedSession(ctx, shell, record, owner)
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Close() }()
	defer func() {
		_ = session.ReleaseLease(context.WithoutCancel(ctx), LeaseName, owner)
	}()
	work, cancelWork := context.WithCancel(ctx)
	stopRenew := s.renew(work, session, owner, cancelWork)
	var renewStopped sync.Once
	stop := func() {
		renewStopped.Do(func() {
			cancelWork()
			stopRenew()
		})
	}
	defer stop()

	if record.Stage != stageAuthorized {
		if err := session.Authorize(work, req.NoBrowser); err != nil {
			stop()
			return nil, s.afterAuthorizeFailure(ctx, session, owner, cand.Name, err)
		}
		record.Stage = stageAuthorized
		if err := record.save(s.opts.ConfigDir); err != nil {
			return nil, err
		}
	}
	if err := work.Err(); err != nil {
		return nil, incomplete(fmt.Errorf("%w: bootstrap lease was lost", ErrBusy))
	}
	ready, err := session.Ready(work)
	if err != nil || !ready {
		return nil, incomplete(fmt.Errorf("durable credential is not ready"))
	}
	token, err := session.Token(work)
	if err != nil {
		return nil, incomplete(fmt.Errorf("read access token: %v", err))
	}
	disc, live, err := s.opts.ReadCatalog(work, cand.Endpoint, token)
	if err != nil {
		return nil, incomplete(fmt.Errorf("discover catalog: %v", err))
	}
	ops, err := cand.Template.Materialize(disc, live)
	if err != nil {
		return nil, incomplete(err)
	}
	full, err := completeProfile(shell, ops)
	if err != nil {
		return nil, incomplete(err)
	}
	// The generation gate is the cross-process authority for publication.
	// Cancellation of work is not enough: profile publication does not take
	// a context, so a stale writer must fail this check before the rename.
	if err := work.Err(); err != nil {
		return nil, incomplete(fmt.Errorf("%w: bootstrap lease was lost", ErrBusy))
	}
	stillHeld, err := session.CommitLease(work, LeaseName, owner, generation)
	if err != nil {
		return nil, incomplete(fmt.Errorf("confirm bootstrap lease: %w", err))
	}
	if !stillHeld {
		return nil, incomplete(fmt.Errorf("%w: bootstrap lease was lost before publication", ErrBusy))
	}
	// The lease gate is not the publication fence. Another process can claim
	// the lease and finish discard after this check. Publication and discard
	// therefore share a cross-process lock, and the profile file is created
	// only while that lock is held and the journal still exists.
	if beforePublish != nil {
		beforePublish()
	}
	err = withPublicationLock(s.opts.ConfigDir, func() error {
		current, err := loadJournal(s.opts.ConfigDir, cand.Name)
		if err != nil || current.ID != record.ID {
			return incomplete(fmt.Errorf("%w: bootstrap was discarded before publication", ErrBusy))
		}
		heldNow, err := session.CommitLease(work, LeaseName, owner, generation)
		if err != nil {
			return incomplete(fmt.Errorf("confirm bootstrap lease: %w", err))
		}
		if !heldNow {
			return incomplete(fmt.Errorf("%w: bootstrap lease was lost before publication", ErrBusy))
		}
		if err := profile.Publish(s.opts.ConfigDir, full); err != nil {
			return err
		}
		readyNow, err := session.Ready(work)
		if err != nil || !readyNow {
			return incomplete(fmt.Errorf("published profile has no matching credential"))
		}
		return removeJournal(s.opts.ConfigDir, cand.Name)
	})
	if errors.Is(err, profile.ErrExists) {
		return s.reconcileWinner(ctx, cand, full)
	}
	if err != nil {
		return nil, err
	}
	return createdResult(s, cand.Name), nil
}

// beforePublish runs after the lease gate and before the publication lock.
// Tests use it to discard the bootstrap in that interval.
var beforePublish func()

func (s *Service) openClaimedSession(ctx context.Context, shell *profile.Profile, record journal, owner string) (Session, journal, int64, error) {
	var session Session
	var current journal
	var generation int64
	err := withPublicationLock(s.opts.ConfigDir, func() error {
		loaded, err := loadJournal(s.opts.ConfigDir, shell.Name)
		if err != nil {
			if os.IsNotExist(err) {
				return failErr(fmt.Errorf("%w: bootstrap journal was removed before lease claim", ErrBusy))
			}
			return incomplete(fmt.Errorf("read bootstrap journal before lease claim: %w", err))
		}
		if loaded.ID != record.ID || loaded.Name != record.Name {
			return failErr(fmt.Errorf("%w: bootstrap journal changed before lease claim", ErrBusy))
		}
		current = loaded
		session, err = s.opts.OpenSession(ctx, shell)
		if err != nil {
			return incomplete(err)
		}
		generation, err = claimBootstrapLease(ctx, session, owner)
		if err != nil {
			_ = session.Close()
			session = nil
			return err
		}
		return nil
	})
	if err != nil {
		return nil, journal{}, 0, err
	}
	return session, current, generation, nil
}

func claimBootstrapLease(ctx context.Context, session Session, owner string) (int64, error) {
	claimed, err := session.ClaimLease(ctx, LeaseName, owner, leaseTTL)
	if err != nil {
		return 0, failErr(fmt.Errorf("claim bootstrap lease: %w", err))
	}
	if !claimed {
		return 0, failErr(fmt.Errorf("%w: wait for it to finish and retry", ErrBusy))
	}
	generation, held, err := session.LeaseGeneration(ctx, LeaseName, owner)
	if err != nil {
		_ = session.ReleaseLease(context.WithoutCancel(ctx), LeaseName, owner)
		return 0, failErr(fmt.Errorf("read bootstrap lease: %w", err))
	}
	if !held {
		_ = session.ReleaseLease(context.WithoutCancel(ctx), LeaseName, owner)
		return 0, failErr(fmt.Errorf("%w: bootstrap lease was lost", ErrBusy))
	}
	return generation, nil
}

func (s *Service) afterAuthorizeFailure(ctx context.Context, session Session, owner string, name profile.Name, cause error) error {
	cleanupErr := withPublicationLock(s.opts.ConfigDir, func() error {
		ready, readyErr := session.Ready(ctx)
		if readyErr != nil || ready {
			return cause
		}
		if err := session.Logout(ctx); err != nil {
			return err
		}
		// Windows denies DELETE while SQLite still holds the database. The
		// publication lock prevents a new session from opening or claiming the
		// state after this release and before cleanup finishes.
		if err := session.ReleaseLease(context.WithoutCancel(ctx), LeaseName, owner); err != nil {
			return err
		}
		if err := session.Close(); err != nil {
			return err
		}
		if err := removeDatabasePath(session.DatabasePath()); err != nil {
			return err
		}
		return removeJournal(s.opts.ConfigDir, name)
	})
	if cleanupErr != nil {
		return incomplete(cleanupErr)
	}
	return failErr(cause)
}

func (s *Service) renew(ctx context.Context, session Session, owner string, lost func()) func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		interval := leaseTTL / 3
		timer := timeAfter(interval)
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer:
				owned, err := session.RenewLease(ctx, LeaseName, owner, leaseTTL)
				if ctx.Err() != nil {
					return
				}
				if err != nil || !owned {
					lost()
					return
				}
				timer = timeAfter(interval)
			}
		}
	}()
	return func() { <-done }
}

func newOwner() (string, error) {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("bootstrap-%d-%s", os.Getpid(), hex.EncodeToString(buf[:])), nil
}

func (s *Service) removeDatabase(shell *profile.Profile) error {
	root, err := profile.StateDir(s.opts.ConfigDir)
	if err != nil {
		return err
	}
	path, err := profile.DatabasePath(root, shell)
	if err != nil {
		return err
	}
	return removeDatabasePath(path)
}

func removeDatabasePath(path string) error {
	if path == "" {
		return nil
	}
	dir, name := filepath.Split(path)
	dir = filepath.Clean(dir)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := profile.RemovePrivateFile(dir, name+suffix); err != nil {
			return err
		}
	}
	return nil
}
