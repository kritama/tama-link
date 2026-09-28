package secretservice

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/godbus/dbus"
)

func finishPrompt(ctx context.Context, conn *dbus.Conn, prompt dbus.ObjectPath, interactive bool) (dbus.Variant, error) {
	if !isPrompt(prompt) {
		return dbus.Variant{}, nil
	}
	if err := ctx.Err(); err != nil {
		return dbus.Variant{}, err
	}
	if !interactive {
		return dbus.Variant{}, ErrCollectionLocked
	}
	return waitForPrompt(ctx, conn, prompt)
}

func isPrompt(path dbus.ObjectPath) bool {
	return strings.HasPrefix(string(path), dbusServicePath+"/prompt/")
}

func waitForPrompt(ctx context.Context, conn *dbus.Conn, prompt dbus.ObjectPath) (dbus.Variant, error) {
	signals := make(chan *dbus.Signal, 8)
	conn.Signal(signals)
	defer conn.RemoveSignal(signals)

	object := conn.Object(dbusServiceName, prompt)
	if err := object.CallWithContext(ctx, "org.freedesktop.Secret.Prompt.Prompt", 0, "").Store(); err != nil {
		return dbus.Variant{}, fmt.Errorf("prompt secret service: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			dismissPrompt(conn, prompt)
			return dbus.Variant{}, ctx.Err()
		case signal, ok := <-signals:
			if !ok {
				return dbus.Variant{}, errors.New("secret service prompt ended before completion")
			}
			if signal.Path != prompt || signal.Name != "org.freedesktop.Secret.Prompt.Completed" {
				continue
			}
			return promptCompleted(signal.Body)
		}
	}
}

// dismissPrompt clears an outstanding prompt on the caller's goroutine.
// Cleanup uses a short timeout independent of the cancelled operation so a
// dismissed prompt is not left for a later process, and no extra goroutine
// is abandoned waiting on the signal.
func dismissPrompt(conn *dbus.Conn, prompt dbus.ObjectPath) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	object := conn.Object(dbusServiceName, prompt)
	_ = object.CallWithContext(ctx, "org.freedesktop.Secret.Prompt.Dismiss", 0).Store()
}

func promptCompleted(body []interface{}) (dbus.Variant, error) {
	if len(body) < 1 {
		return dbus.Variant{}, errors.New("secret service prompt completed without a result")
	}
	dismissed, ok := body[0].(bool)
	if !ok {
		return dbus.Variant{}, errors.New("secret service prompt result is malformed")
	}
	if dismissed {
		return dbus.Variant{}, ErrPromptDismissed
	}
	if len(body) < 2 {
		return dbus.Variant{}, nil
	}
	result, ok := body[1].(dbus.Variant)
	if !ok {
		return dbus.Variant{}, errors.New("secret service prompt result is malformed")
	}
	return result, nil
}

func stringProperty(ctx context.Context, object dbus.BusObject, name string) (string, error) {
	value, err := property(ctx, object, name)
	if err != nil {
		return "", err
	}
	text, ok := value.Value().(string)
	if !ok {
		return "", fmt.Errorf("property %s has unexpected type %T", name, value.Value())
	}
	return text, nil
}

func boolProperty(ctx context.Context, object dbus.BusObject, name string) (bool, error) {
	value, err := property(ctx, object, name)
	if err != nil {
		return true, err
	}
	locked, ok := value.Value().(bool)
	if !ok {
		return true, fmt.Errorf("property %s has unexpected type %T", name, value.Value())
	}
	return locked, nil
}

func objectPaths(ctx context.Context, object dbus.BusObject, name string) ([]dbus.ObjectPath, error) {
	value, err := property(ctx, object, name)
	if err != nil {
		return nil, err
	}
	paths, ok := value.Value().([]dbus.ObjectPath)
	if !ok {
		return nil, fmt.Errorf("property %s has unexpected type %T", name, value.Value())
	}
	return paths, nil
}

// property reads one D-Bus property with the caller's context. The pinned
// godbus GetProperty helper cannot be cancelled, so this calls
// org.freedesktop.DBus.Properties.Get directly. A result that arrives after
// cancellation is discarded.
func property(ctx context.Context, object dbus.BusObject, name string) (dbus.Variant, error) {
	iface, member, err := splitProperty(name)
	if err != nil {
		return dbus.Variant{}, err
	}
	if err := ctx.Err(); err != nil {
		return dbus.Variant{}, err
	}
	var value dbus.Variant
	err = object.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, iface, member).Store(&value)
	if err != nil {
		return dbus.Variant{}, err
	}
	if err := ctx.Err(); err != nil {
		return dbus.Variant{}, err
	}
	return value, nil
}

func splitProperty(name string) (string, string, error) {
	index := strings.LastIndex(name, ".")
	if index <= 0 || index == len(name)-1 {
		return "", "", fmt.Errorf("property %s is malformed", name)
	}
	return name[:index], name[index+1:], nil
}
