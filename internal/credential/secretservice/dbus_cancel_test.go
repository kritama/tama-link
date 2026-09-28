package secretservice

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/godbus/dbus"
)

func TestPropertyReadHonorsCancellation(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	object := &stallBus{started: started, release: release}
	collection := &dbusCollection{object: object}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := collection.Label(ctx)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("property read did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("property read = %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("production collection Label stayed blocked after context cancellation")
	}
	close(release)
}

func TestAuthenticateReleasesBlockedSetup(t *testing.T) {
	t.Parallel()

	client, server := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	conn, err := dbus.NewConn(client)
	if err != nil {
		t.Fatalf("new connection: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- authenticate(ctx, conn, client)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("authenticate = %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("setup stayed blocked after cancellation")
	}
	if err := server.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		return
	}
	buf := make([]byte, 64)
	released := false
	for range 4 {
		_, err := server.Read(buf)
		if err == nil {
			continue
		}
		released = !errors.Is(err, os.ErrDeadlineExceeded)
		break
	}
	if !released {
		t.Fatal("setup connection was not released")
	}
}

func TestDialSessionSocketHonorsDeadline(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+t.TempDir()+"/bus")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := dialSessionSocket(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("dial = %v, want cancellation", err)
	}
}

type stallBus struct {
	started chan struct{}
	release chan struct{}
}

func (s *stallBus) Call(string, dbus.Flags, ...interface{}) *dbus.Call {
	panic("unexpected call")
}

func (s *stallBus) CallWithContext(ctx context.Context, _ string, _ dbus.Flags, _ ...interface{}) *dbus.Call {
	select {
	case s.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return &dbus.Call{Err: ctx.Err()}
	case <-s.release:
		return &dbus.Call{Body: []interface{}{dbus.MakeVariant("Login")}}
	}
}

func (s *stallBus) Go(string, dbus.Flags, chan *dbus.Call, ...interface{}) *dbus.Call {
	panic("unexpected go")
}

func (s *stallBus) GoWithContext(context.Context, string, dbus.Flags, chan *dbus.Call, ...interface{}) *dbus.Call {
	panic("unexpected go")
}

func (s *stallBus) AddMatchSignal(string, string, ...dbus.MatchOption) *dbus.Call {
	panic("unexpected match")
}

func (s *stallBus) RemoveMatchSignal(string, string, ...dbus.MatchOption) *dbus.Call {
	panic("unexpected match")
}

func (s *stallBus) GetProperty(string) (dbus.Variant, error) {
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-s.release
	return dbus.MakeVariant("Login"), nil
}

func (s *stallBus) SetProperty(string, interface{}) error { panic("unexpected set") }

func (s *stallBus) Destination() string { return dbusServiceName }

func (s *stallBus) Path() dbus.ObjectPath { return dbus.ObjectPath(dbusServicePath) }

// Ensure the stall bus satisfies the interface used by property reads.
var _ dbus.BusObject = (*stallBus)(nil)
