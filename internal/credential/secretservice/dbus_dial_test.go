package secretservice

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus"
)

func TestAuthenticateCancelsBlockedSetupWrite(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"Hello", "AddMatch"} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			client, server := net.Pipe()
			started := make(chan struct{})
			raw := &setupWriteSocket{Conn: client, started: started, blockMatch: method == "AddMatch"}
			conn, err := dbus.NewConn(raw)
			if err != nil {
				t.Fatal(err)
			}
			serverDone := make(chan error, 1)
			go func() { serverDone <- serveSetupHandshake(server, method) }()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				done <- authenticate(ctx, conn, raw)
			}()
			t.Cleanup(func() {
				cancel()
				_ = client.Close()
				_ = server.Close()
				<-finished
				if err := <-serverDone; err != nil {
					t.Errorf("setup peer: %v", err)
				}
			})
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatalf("%s write did not start", method)
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("authenticate = %v, want cancellation", err)
				}
			case <-time.After(200 * time.Millisecond):
				// Release the raw socket before failing even against the old code.
				_ = client.Close()
				<-done
				t.Fatalf("authentication stayed blocked after cancellation while sending %s", method)
			}
		})
	}
}

// setupWriteSocket observes the first byte of each binary D-Bus request. The
// peer stops reading at the selected request so its Write blocks in godbus.
type setupWriteSocket struct {
	net.Conn
	started    chan struct{}
	once       sync.Once
	blockMatch bool
	requests   int
}

func (s *setupWriteSocket) Write(b []byte) (int, error) {
	if len(b) > 0 && (b[0] == 'l' || b[0] == 'B' && !strings.HasPrefix(string(b), "BEGIN")) {
		s.requests++
		if !s.blockMatch || s.requests == 2 {
			s.once.Do(func() { close(s.started) })
		}
	}
	return s.Conn.Write(b)
}

func serveSetupHandshake(conn net.Conn, blockedMethod string) error {
	reader := bufio.NewReader(conn)
	if _, err := reader.ReadByte(); err != nil {
		return err
	}
	for _, reply := range []string{"REJECTED EXTERNAL\r\n", "OK 0123456789abcdef0123456789abcdef\r\n"} {
		if _, err := reader.ReadString('\n'); err != nil {
			return err
		}
		if _, err := io.WriteString(conn, reply); err != nil {
			return err
		}
	}
	if _, err := reader.ReadString('\n'); err != nil { // BEGIN
		return err
	}
	if blockedMethod == "Hello" {
		return nil
	}
	hello, err := dbus.DecodeMessage(reader)
	if err != nil {
		return err
	}
	if hello.Headers[dbus.FieldMember].Value() != "Hello" {
		return fmt.Errorf("unexpected setup method")
	}
	reply := &dbus.Message{
		Type: dbus.TypeMethodReply,
		Headers: map[dbus.HeaderField]dbus.Variant{
			dbus.FieldReplySerial: dbus.MakeVariant(hello.Serial()),
			dbus.FieldSignature:   dbus.MakeVariant(dbus.SignatureOf(":1.1")),
		},
		Body: []interface{}{":1.1"},
	}
	return reply.EncodeTo(conn, binary.LittleEndian)
}
