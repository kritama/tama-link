package repair

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/kritama/tama-link/internal/store"
)

const credentialLeaseTTL = 30 * time.Second

// claimCredentialLease holds the profile credential lease for the rest of the
// repair write. The returned context is cancelled if that lease is lost, so
// a later write observes the loss. The release function stops renewal and
// drops the lease; it is safe to call after cancellation.
func claimCredentialLease(ctx context.Context, req Request, keyID string, key []byte) (context.Context, func(), error) {
	opened, err := store.Open(ctx, req.DatabasePath, candidateKey{id: keyID, key: key}, store.Config{Limits: req.Limits})
	if err != nil {
		return nil, nil, fmt.Errorf("open profile lease: %w", err)
	}
	owner, err := newLeaseOwner()
	if err != nil {
		_ = opened.Close()
		return nil, nil, err
	}
	claimed, err := opened.ClaimLease(ctx, store.CredentialLeaseName, owner, credentialLeaseTTL)
	if err != nil || !claimed {
		_ = opened.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, ErrLeaseBusy
	}
	opCtx, cancelOp := context.WithCancel(ctx)
	renewCtx, cancelRenew := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go renewCredentialLease(renewCtx, cancelOp, opened, owner, done)
	return opCtx, func() {
		cancelOp()
		cancelRenew()
		<-done
		_ = opened.ReleaseLease(context.WithoutCancel(ctx), store.CredentialLeaseName, owner)
		_ = opened.Close()
	}, nil
}

func renewCredentialLease(ctx context.Context, cancelOp context.CancelFunc, opened *store.Store, owner string, done chan struct{}) {
	defer close(done)
	interval := credentialLeaseTTL / 3
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			renewCtx, cancel := context.WithTimeout(ctx, interval)
			owned, err := opened.RenewLease(renewCtx, store.CredentialLeaseName, owner, credentialLeaseTTL)
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err != nil || !owned {
				cancelOp()
				return
			}
			timer.Reset(interval)
		}
	}
}

func newLeaseOwner() (string, error) {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("repair lease owner: %w", err)
	}
	return fmt.Sprintf("repair-%d-%s", os.Getpid(), hex.EncodeToString(buf[:])), nil
}
