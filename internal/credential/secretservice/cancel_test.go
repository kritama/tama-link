package secretservice_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/credential/secretservice"
	"github.com/kritama/tama-link/internal/credential/secrettest"
)

func TestCancelledLabelReadDoesNotWrite(t *testing.T) {
	service := secrettest.New()
	service.SetDefault(defaultPath, defaultName)
	legacy := service.AddCollection(legacyPath, secretservice.LegacyCollectionLabel)
	legacy.Put("demo/default/state/v1-abc", []byte("exact-secret"), "application/json")
	started := make(chan struct{}, 1)
	stalling := &stallingService{Service: service, started: started}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := secretservice.ReadPlan(ctx, stalling, secretservice.MigrateRequest{
			Prefix:      "demo/default/",
			StateKey:    "demo/default/state/v1-abc",
			Interactive: true,
		})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("label read did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("read plan = %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled label read stayed blocked")
	}
	if service.CreateItemCalls() != 0 {
		t.Fatalf("cancelled property read wrote %d items", service.CreateItemCalls())
	}
}

type stallingService struct {
	*secrettest.Service
	started chan struct{}
}

func (s *stallingService) Collections(ctx context.Context) ([]secretservice.Collection, error) {
	collections, err := s.Service.Collections(ctx)
	if err != nil {
		return nil, err
	}
	wrapped := make([]secretservice.Collection, 0, len(collections))
	for _, collection := range collections {
		wrapped = append(wrapped, stallingCollection{Collection: collection, started: s.started})
	}
	return wrapped, nil
}

type stallingCollection struct {
	secretservice.Collection
	started chan struct{}
}

func (c stallingCollection) Label(ctx context.Context) (string, error) {
	select {
	case c.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(time.Second):
		return "", errors.New("label read was not cancelled")
	}
}
