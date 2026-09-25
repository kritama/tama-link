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
	"github.com/kritama/tama-link/internal/upstream"
)

func TestLeaseLossDoesNotPublishOverDiscard(t *testing.T) {
	configDir := privateConfig(t)
	tmpl, err := tama2026.Lookup(profile.KindApp)
	if err != nil {
		t.Fatal(err)
	}
	session := &racingSession{
		dbPath: filepath.Join(t.TempDir(), "state.db"),
		ready:  true,
		token:  "memory-token",
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	svc := testService(t, configDir, nil, &fakeSession{}, func(ctx context.Context, _, _ string) (*upstream.DiscoverResult, []*upstream.LiveTool, error) {
		once.Do(func() { close(started) })
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-release:
		}
		return discoverResult(tmpl), liveFrom(tmpl), nil
	})
	svc.opts.OpenSession = func(context.Context, *profile.Profile) (Session, error) {
		return session, nil
	}
	record := journal{
		Version: journalVersion, ID: "0123456789abcdef0123456789abcdef",
		Name: "tama-app", Origin: "https://tama.example",
		Endpoint: "https://tama.example/mcp/app", Issuer: "https://auth.example",
		Template: tmpl.ID, Scopes: tmpl.Scopes,
		Database: stateDatabase, Credentials: stateCredentials, Stage: stageAuthorized,
	}
	if err := record.create(configDir); err != nil {
		t.Fatal(err)
	}
	cand := candidate{
		Name: "tama-app", Origin: record.Origin, Endpoint: record.Endpoint,
		Issuer: record.Issuer, Template: tmpl,
	}
	done := make(chan error, 1)
	go func() {
		_, err := svc.finish(context.Background(), Request{Yes: true}, cand, record)
		done <- err
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("finish returned before catalog: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("catalog did not start")
	}
	session.steal()
	if err := svc.discard(context.Background(), record); err != nil {
		t.Fatalf("discard: %v", err)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("stale finish reported success after lease loss")
	}
	if _, err := profile.Load("tama-app", configDir); !errors.Is(err, profile.ErrNotFound) {
		t.Fatalf("stale finish published a profile: %v", err)
	}
}

type racingSession struct {
	mu         sync.Mutex
	owner      string
	generation int64
	lost       bool
	ready      bool
	token      string
	dbPath     string
}

func (r *racingSession) Close() error { return nil }
func (r *racingSession) Authorize(context.Context, bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ready = true
	r.token = "memory-token"
	return nil
}
func (r *racingSession) Token(context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.token == "" {
		return "", errors.New("no token")
	}
	return r.token, nil
}
func (r *racingSession) Ready(context.Context) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ready, nil
}
func (r *racingSession) Logout(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ready = false
	r.token = ""
	return nil
}
func (r *racingSession) ClaimLease(_ context.Context, _, owner string, _ time.Duration) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owner != "" && r.owner != owner && !r.lost {
		return false, nil
	}
	r.owner = owner
	r.generation++
	r.lost = false
	return true, nil
}
func (r *racingSession) RenewLease(_ context.Context, _, owner string, _ time.Duration) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.owner == owner && !r.lost, nil
}
func (r *racingSession) ReleaseLease(_ context.Context, _, owner string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owner == owner {
		r.owner = ""
	}
	return nil
}
func (r *racingSession) LeaseGeneration(_ context.Context, _, owner string) (int64, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owner != owner || r.lost {
		return 0, false, nil
	}
	return r.generation, true, nil
}
func (r *racingSession) CommitLease(_ context.Context, _, owner string, generation int64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.owner == owner && !r.lost && r.generation == generation, nil
}
func (r *racingSession) DatabasePath() string { return r.dbPath }
func (r *racingSession) steal() {
	r.mu.Lock()
	r.lost = true
	r.mu.Unlock()
}

func TestDiscardAfterPublicationGateDoesNotPublish(t *testing.T) {
	configDir := privateConfig(t)
	tmpl, err := tama2026.Lookup(profile.KindApp)
	if err != nil {
		t.Fatal(err)
	}
	session := &racingSession{
		dbPath: filepath.Join(t.TempDir(), "state.db"),
		ready:  true,
		token:  "memory-token",
	}
	svc := testService(t, configDir, nil, &fakeSession{}, func(context.Context, string, string) (*upstream.DiscoverResult, []*upstream.LiveTool, error) {
		return discoverResult(tmpl), liveFrom(tmpl), nil
	})
	svc.opts.OpenSession = func(context.Context, *profile.Profile) (Session, error) {
		return session, nil
	}
	record := journal{
		Version: journalVersion, ID: "0123456789abcdef0123456789abcdef",
		Name: "tama-app", Origin: "https://tama.example",
		Endpoint: "https://tama.example/mcp/app", Issuer: "https://auth.example",
		Template: tmpl.ID, Scopes: tmpl.Scopes,
		Database: stateDatabase, Credentials: stateCredentials, Stage: stageAuthorized,
	}
	if err := record.create(configDir); err != nil {
		t.Fatal(err)
	}
	beforePublish = func() {
		session.steal()
		if err := svc.discard(context.Background(), record); err != nil {
			t.Errorf("discard: %v", err)
		}
	}
	t.Cleanup(func() { beforePublish = nil })
	_, err = svc.finish(context.Background(), Request{Yes: true}, candidate{
		Name: "tama-app", Origin: record.Origin, Endpoint: record.Endpoint,
		Issuer: record.Issuer, Template: tmpl,
	}, record)
	if err == nil {
		t.Fatal("stale finish published after discard completed")
	}
	if _, err := profile.Load("tama-app", configDir); !errors.Is(err, profile.ErrNotFound) {
		t.Fatalf("profile remains after completed discard: %v", err)
	}
	if _, err := loadJournal(configDir, "tama-app"); !os.IsNotExist(err) {
		t.Fatalf("discard left a journal: %v", err)
	}
	session.mu.Lock()
	token := session.token
	session.mu.Unlock()
	if token != "" {
		t.Fatal("discard left a credential")
	}
}

func TestRecoveryRejectsConflictingWinner(t *testing.T) {
	t.Parallel()
	configDir := privateConfig(t)
	tmpl, err := tama2026.Lookup(profile.KindApp)
	if err != nil {
		t.Fatal(err)
	}
	winner := publishedProfile(t, tmpl, "https://other.example")
	if err := profile.Publish(configDir, winner); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(profile.Path(configDir, winner.Name))
	if err != nil {
		t.Fatal(err)
	}
	record := journal{
		Version: journalVersion, ID: "0123456789abcdef0123456789abcdef",
		Name: winner.Name.String(), Origin: "https://tama.example",
		Endpoint: "https://tama.example/mcp/app", Issuer: "https://auth.example",
		Template: tmpl.ID, Scopes: tmpl.Scopes,
		Database: stateDatabase, Credentials: stateCredentials, Stage: stageAuthorized,
	}
	if err := record.create(configDir); err != nil {
		t.Fatal(err)
	}
	session := &fakeSession{ready: true, token: "memory-token"}
	svc := testService(t, configDir, nil, session, func(context.Context, string, string) (*upstream.DiscoverResult, []*upstream.LiveTool, error) {
		return discoverResult(tmpl), liveFrom(tmpl), nil
	})
	_, err = svc.completePublished(context.Background(), candidate{
		Name: winner.Name, Origin: record.Origin, Endpoint: record.Endpoint,
		Issuer: record.Issuer, Template: tmpl,
	}, winner)
	if !errors.Is(err, profile.ErrExists) {
		t.Fatalf("error = %v, want conflict", err)
	}
	got, err := os.ReadFile(profile.Path(configDir, winner.Name))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatal("conflicting winner was modified")
	}
	if _, err := loadJournal(configDir, winner.Name); err != nil {
		t.Fatalf("journal was not retained: %v", err)
	}
}

func TestRecoveryAcceptsMatchingWinner(t *testing.T) {
	t.Parallel()
	configDir := privateConfig(t)
	tmpl, err := tama2026.Lookup(profile.KindApp)
	if err != nil {
		t.Fatal(err)
	}
	origin := "https://tama.example"
	winner := publishedProfile(t, tmpl, origin)
	winner.Issuer = "https://auth.example"
	digest, err := winner.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	winner.Digest = digest
	if err := profile.Publish(configDir, winner); err != nil {
		t.Fatal(err)
	}
	record := journal{
		Version: journalVersion, ID: "0123456789abcdef0123456789abcdef",
		Name: winner.Name.String(), Origin: origin,
		Endpoint: origin + "/mcp/app", Issuer: winner.Issuer,
		Template: tmpl.ID, Scopes: tmpl.Scopes,
		Database: stateDatabase, Credentials: stateCredentials, Stage: stageAuthorized,
	}
	if err := record.create(configDir); err != nil {
		t.Fatal(err)
	}
	session := &fakeSession{ready: true, token: "memory-token"}
	svc := testService(t, configDir, nil, session, func(context.Context, string, string) (*upstream.DiscoverResult, []*upstream.LiveTool, error) {
		return discoverResult(tmpl), liveFrom(tmpl), nil
	})
	result, err := svc.completePublished(context.Background(), candidate{
		Name: winner.Name, Origin: origin, Endpoint: origin + "/mcp/app",
		Issuer: winner.Issuer, Template: tmpl,
	}, winner)
	if err != nil {
		t.Fatalf("completePublished: %v", err)
	}
	if !result.Created {
		t.Fatalf("result = %+v", result)
	}
	if _, err := loadJournal(configDir, winner.Name); !os.IsNotExist(err) {
		t.Fatalf("matching winner left a journal: %v", err)
	}
}

func TestRecoveryRejectsUndigestedAndStaleWinners(t *testing.T) {
	t.Parallel()
	tmpl, err := tama2026.Lookup(profile.KindApp)
	if err != nil {
		t.Fatal(err)
	}
	expected := publishedProfile(t, tmpl, "https://tama.example")
	expected.Issuer = "https://auth.example"
	digest, err := expected.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	expected.Digest = digest

	missing := *expected
	missing.Digest = ""
	if samePublished(&missing, expected) {
		t.Fatal("accepted a profile with no persisted digest")
	}
	stale := *expected
	stale.Digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if samePublished(&stale, expected) {
		t.Fatal("accepted a stale persisted digest")
	}
	if !samePublished(expected, expected) {
		t.Fatal("rejected an exact persisted digest")
	}

	configDir := privateConfig(t)
	undigested := *expected
	undigested.Digest = ""
	if err := profile.Publish(configDir, &undigested); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(profile.Path(configDir, undigested.Name))
	if err != nil {
		t.Fatal(err)
	}
	record := journal{
		Version: journalVersion, ID: "0123456789abcdef0123456789abcdef",
		Name: undigested.Name.String(), Origin: undigested.Origin,
		Endpoint: undigested.Endpoint, Issuer: undigested.Issuer,
		Template: tmpl.ID, Scopes: tmpl.Scopes,
		Database: stateDatabase, Credentials: stateCredentials, Stage: stageAuthorized,
	}
	if err := record.create(configDir); err != nil {
		t.Fatal(err)
	}
	session := &fakeSession{ready: true, token: "memory-token"}
	svc := testService(t, configDir, nil, session, func(context.Context, string, string) (*upstream.DiscoverResult, []*upstream.LiveTool, error) {
		return discoverResult(tmpl), liveFrom(tmpl), nil
	})
	_, err = svc.completePublished(context.Background(), candidate{
		Name: undigested.Name, Origin: undigested.Origin, Endpoint: undigested.Endpoint,
		Issuer: undigested.Issuer, Template: tmpl,
	}, &undigested)
	if !errors.Is(err, profile.ErrExists) {
		t.Fatalf("undigested winner error = %v, want conflict", err)
	}
	got, err := os.ReadFile(profile.Path(configDir, undigested.Name))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatal("undigested winner was edited")
	}
	if _, err := loadJournal(configDir, undigested.Name); err != nil {
		t.Fatalf("journal was not retained: %v", err)
	}
}

func publishedProfile(t *testing.T, tmpl tama2026.Template, origin string) *profile.Profile {
	t.Helper()
	p := &profile.Profile{
		Version: profile.SchemaVersion, Name: "tama-app",
		Origin: origin, Endpoint: origin + "/mcp/app", Issuer: "https://auth.example",
		Instructions: tmpl.Instructions, Bounds: tmpl.Bounds,
		State:      profile.StateRefs{Database: stateDatabase, Credentials: stateCredentials},
		Scopes:     tmpl.Scopes,
		Operations: tmpl.Operations,
	}
	digest, err := p.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	p.Digest = digest
	return p
}
