package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/adapter/tama2026"
	"github.com/kritama/tama-link/internal/profile"
)

func TestFinishOpensSessionInsideCleanupFence(t *testing.T) {
	configDir := privateConfig(t)
	record, cand := bootstrapRecord(t, configDir, "0123456789abcdef0123456789abcdef")
	lockHeld, releaseLock, lockDone := holdPublicationLock(t, configDir)
	<-lockHeld

	opened := make(chan struct{})
	svc := testService(t, configDir, nil, &fakeSession{}, nil)
	svc.opts.OpenSession = func(context.Context, *profile.Profile) (Session, error) {
		close(opened)
		return nil, errors.New("stop after opening")
	}
	done := make(chan error, 1)
	go func() {
		_, err := svc.finish(context.Background(), Request{Yes: true}, cand, record)
		done <- err
	}()
	assertNoSignal(t, opened, "session opened while cleanup fence was held")
	releaseLock()
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-opened:
	case <-time.After(time.Second):
		t.Fatal("session did not open after cleanup fence was released")
	}
	if err := <-done; !errors.Is(err, ErrIncomplete) {
		t.Fatalf("finish error = %v, want incomplete", err)
	}
}

func TestAuthorizeFailureCleanupUsesSessionFence(t *testing.T) {
	configDir := privateConfig(t)
	record, _ := bootstrapRecord(t, configDir, "0123456789abcdef0123456789abcdef")
	dbDir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dbDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, "state.db")
	if err := os.WriteFile(dbPath, []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	lockHeld, releaseLock, lockDone := holdPublicationLock(t, configDir)
	<-lockHeld

	readyCalled := make(chan struct{})
	session := &cleanupProbeSession{
		fakeSession: fakeSession{dbPath: dbPath},
		readyCalled: readyCalled,
	}
	svc := testService(t, configDir, nil, &fakeSession{}, nil)
	cause := errors.New("authorization failed")
	done := make(chan error, 1)
	go func() {
		done <- svc.afterAuthorizeFailure(context.Background(), session, "owner", profile.Name(record.Name), cause)
	}()
	assertNoSignal(t, readyCalled, "cleanup started while session fence was held")
	releaseLock()
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-readyCalled:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not start after session fence was released")
	}
	if err := <-done; !errors.Is(err, cause) {
		t.Fatalf("cleanup error = %v, want authorization failure", err)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("database remains after cleanup: %v", err)
	}
	if _, err := loadJournal(configDir, profile.Name(record.Name)); !os.IsNotExist(err) {
		t.Fatalf("journal remains after cleanup: %v", err)
	}
}

func TestFinishRejectsReplacedJournalBeforeOpeningSession(t *testing.T) {
	configDir := privateConfig(t)
	stale, cand := bootstrapRecord(t, configDir, "0123456789abcdef0123456789abcdef")
	if err := removeJournal(configDir, profile.Name(stale.Name)); err != nil {
		t.Fatal(err)
	}
	current := stale
	current.ID = "fedcba9876543210fedcba9876543210"
	if err := current.create(configDir); err != nil {
		t.Fatal(err)
	}

	opened := false
	svc := testService(t, configDir, nil, &fakeSession{}, nil)
	svc.opts.OpenSession = func(context.Context, *profile.Profile) (Session, error) {
		opened = true
		return &fakeSession{}, nil
	}
	_, err := svc.finish(context.Background(), Request{Yes: true}, cand, stale)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("finish error = %v, want busy", err)
	}
	if opened {
		t.Fatal("opened state for a replaced bootstrap journal")
	}
	got, err := loadJournal(configDir, profile.Name(current.Name))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != current.ID {
		t.Fatalf("journal id = %q, want %q", got.ID, current.ID)
	}
}

type cleanupProbeSession struct {
	fakeSession
	readyCalled chan struct{}
}

func (s *cleanupProbeSession) Ready(context.Context) (bool, error) {
	close(s.readyCalled)
	return false, nil
}

func bootstrapRecord(t *testing.T, configDir, id string) (journal, candidate) {
	t.Helper()
	tmpl, err := tama2026.Lookup(profile.KindApp)
	if err != nil {
		t.Fatal(err)
	}
	record := journal{
		Version: journalVersion, ID: id, Name: "tama-app",
		Origin: "https://tama.example", Endpoint: "https://tama.example/mcp/app",
		Issuer: "https://auth.example", Template: tmpl.ID, Scopes: tmpl.Scopes,
		Database: stateDatabase, Credentials: stateCredentials, Stage: stageReserved,
	}
	if err := record.create(configDir); err != nil {
		t.Fatal(err)
	}
	return record, candidate{
		Name: profile.Name(record.Name), Origin: record.Origin, Endpoint: record.Endpoint,
		Issuer: record.Issuer, Template: tmpl,
	}
}

func holdPublicationLock(t *testing.T, configDir string) (<-chan struct{}, func(), <-chan error) {
	t.Helper()
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	var once sync.Once
	releaseLock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseLock)
	go func() {
		done <- withPublicationLock(configDir, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	return held, releaseLock, done
}

func assertNoSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
		t.Fatal(message)
	case <-time.After(100 * time.Millisecond):
	}
}
