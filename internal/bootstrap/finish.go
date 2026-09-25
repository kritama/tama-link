package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
	session, err := s.opts.OpenSession(ctx, shell)
	if err != nil {
		return nil, incomplete(err)
	}
	defer func() { _ = session.Close() }()

	owner, err := newOwner()
	if err != nil {
		return nil, failErr(err)
	}
	claimed, err := session.ClaimLease(ctx, LeaseName, owner, leaseTTL)
	if err != nil {
		return nil, failErr(fmt.Errorf("claim bootstrap lease: %w", err))
	}
	if !claimed {
		return nil, failErr(fmt.Errorf("%w: wait for it to finish and retry", ErrBusy))
	}
	generation, held, err := session.LeaseGeneration(ctx, LeaseName, owner)
	if err != nil {
		return nil, failErr(fmt.Errorf("read bootstrap lease: %w", err))
	}
	if !held {
		return nil, failErr(fmt.Errorf("%w: bootstrap lease was lost", ErrBusy))
	}
	defer func() {
		_ = session.ReleaseLease(context.WithoutCancel(ctx), LeaseName, owner)
	}()
	work, cancelWork := context.WithCancel(ctx)
	stopRenew := s.renew(work, session, owner, cancelWork)
	defer func() {
		cancelWork()
		stopRenew()
	}()

	if record.Stage != stageAuthorized {
		if err := session.Authorize(work, req.NoBrowser); err != nil {
			return nil, s.afterAuthorizeFailure(ctx, session, cand.Name, err)
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
		if _, err := loadJournal(s.opts.ConfigDir, cand.Name); err != nil {
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

func (s *Service) afterAuthorizeFailure(ctx context.Context, session Session, name profile.Name, err error) error {
	ready, readyErr := session.Ready(ctx)
	if readyErr == nil && !ready {
		_ = removeDatabasePath(session.DatabasePath())
		_ = removeJournal(s.opts.ConfigDir, name)
		return failErr(err)
	}
	return incomplete(err)
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
