package secretservice

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"strings"

	"github.com/godbus/dbus"
)

// connectSession dials the session bus and authenticates within ctx. The
// socket dial is context-aware. Authentication has no context parameter, so
// cancellation closes the connection and joins that call before returning.
func connectSession(ctx context.Context) (*dbus.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := dialSessionSocket(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := dbus.NewConn(raw)
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("open secret service connection: %w", err)
	}
	if err := authenticate(ctx, conn, raw); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func dialSessionSocket(ctx context.Context) (net.Conn, error) {
	address, err := sessionBusAddress()
	if err != nil {
		return nil, err
	}
	network, socket, err := unixSocket(address)
	if err != nil {
		return nil, err
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, network, socket)
	if err != nil {
		return nil, fmt.Errorf("connect secret service: %w", err)
	}
	return conn, nil
}

func sessionBusAddress() (string, error) {
	if address := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); address != "" && address != "autolaunch:" {
		return address, nil
	}
	current, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve secret service address: %w", err)
	}
	socket := "/run/user/" + current.Uid + "/bus"
	info, err := os.Stat(socket)
	if err != nil {
		return "", fmt.Errorf("resolve secret service address: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("resolve secret service address: %s is a directory", socket)
	}
	return "unix:path=" + socket, nil
}

func unixSocket(address string) (string, string, error) {
	if !strings.HasPrefix(address, "unix:") {
		return "", "", fmt.Errorf("unsupported secret service address")
	}
	path := busKey(address[len("unix:"):], "path")
	abstract := busKey(address[len("unix:"):], "abstract")
	switch {
	case path != "" && abstract == "":
		return "unix", path, nil
	case abstract != "" && path == "":
		return "unix", "@" + abstract, nil
	default:
		return "", "", fmt.Errorf("secret service address is missing a socket")
	}
}

func busKey(keys, name string) string {
	for _, part := range strings.Split(keys, ",") {
		key, value, ok := strings.Cut(part, "=")
		if ok && key == name {
			return value
		}
	}
	return ""
}

// authenticate runs the blocking bus handshake on one owned goroutine. On
// cancellation it closes the raw socket before joining setup and closing
// godbus. The pinned godbus Close waits for a lock held by blocked writes, so
// closing godbus first would prevent cancellation from releasing those writes.
func authenticate(ctx context.Context, conn *dbus.Conn, raw net.Conn) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		done <- finishAuthentication(conn)
	}()
	select {
	case <-ctx.Done():
		rawErr := raw.Close()
		authErr := <-done
		closeErr := conn.Close()
		return errors.Join(ctx.Err(), authErr, rawErr, closeErr)
	case err := <-done:
		if err != nil {
			return err
		}
		return ctx.Err()
	}
}

func finishAuthentication(conn *dbus.Conn) error {
	if err := conn.Auth(nil); err != nil {
		return fmt.Errorf("authenticate secret service: %w", err)
	}
	if err := conn.Hello(); err != nil {
		return fmt.Errorf("hello secret service: %w", err)
	}
	if err := conn.AddMatchSignal(
		dbus.WithMatchInterface("org.freedesktop.Secret.Prompt"),
		dbus.WithMatchMember("Completed"),
	); err != nil {
		return fmt.Errorf("watch secret service prompts: %w", err)
	}
	return nil
}
