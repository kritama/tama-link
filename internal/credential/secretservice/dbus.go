package secretservice

import (
	"context"
	"fmt"

	"github.com/godbus/dbus"
)

// dbusSecret is the org.freedesktop.Secret.Secret struct. Field order is the
// D-Bus signature (oayays).
type dbusSecret struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

type dbusService struct {
	conn        *dbus.Conn
	object      dbus.BusObject
	interactive bool
}

// Dial connects to the session Secret Service. The caller's context bounds
// socket setup and authentication. A cancelled setup closes the connection
// and waits for that work to finish; it does not leave a goroutine behind.
func Dial(ctx context.Context, cfg Config) (Service, error) {
	conn, err := connectSession(ctx)
	if err != nil {
		return nil, err
	}
	return &dbusService{
		conn:        conn,
		object:      conn.Object(dbusServiceName, dbus.ObjectPath(dbusServicePath)),
		interactive: cfg.Interactive,
	}, nil
}

func (s *dbusService) Close() error {
	if s == nil || s.conn == nil {
		return nil
	}
	return s.conn.Close()
}

func (s *dbusService) OpenSession(ctx context.Context) (Session, error) {
	var output dbus.Variant
	var path dbus.ObjectPath
	err := call(ctx, s.object, "org.freedesktop.Secret.Service.OpenSession", "plain", dbus.MakeVariant("")).Store(&output, &path)
	if err != nil {
		return nil, fmt.Errorf("open secret service session: %w", err)
	}
	if isRootPath(string(path)) {
		return nil, fmt.Errorf("open secret service session: empty session")
	}
	return dbusSession{path: string(path)}, nil
}

func (s *dbusService) ReadAlias(ctx context.Context, name string) (Collection, error) {
	var path dbus.ObjectPath
	if err := call(ctx, s.object, "org.freedesktop.Secret.Service.ReadAlias", name).Store(&path); err != nil {
		return nil, fmt.Errorf("read secret service alias %q: %w", name, err)
	}
	if isRootPath(string(path)) {
		return nil, ErrDefaultCollectionMissing
	}
	return s.collection(path), nil
}

func (s *dbusService) Collections(ctx context.Context) ([]Collection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	paths, err := objectPaths(ctx, s.object, "org.freedesktop.Secret.Service.Collections")
	if err != nil {
		return nil, fmt.Errorf("list secret service collections: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	collections := make([]Collection, 0, len(paths))
	for _, path := range paths {
		collections = append(collections, s.collection(path))
	}
	return collections, nil
}

func (s *dbusService) Unlock(ctx context.Context, object Object) error {
	if object == nil || isRootPath(object.Path()) {
		return fmt.Errorf("unlock secret service object: empty path")
	}
	var unlocked []dbus.ObjectPath
	var prompt dbus.ObjectPath
	err := call(ctx, s.object, "org.freedesktop.Secret.Service.Unlock", []dbus.ObjectPath{dbus.ObjectPath(object.Path())}).Store(&unlocked, &prompt)
	if err != nil {
		return fmt.Errorf("unlock secret service object: %w", err)
	}
	_, err = finishPrompt(ctx, s.conn, prompt, s.interactive)
	return err
}

func (s *dbusService) collection(path dbus.ObjectPath) *dbusCollection {
	return &dbusCollection{
		conn:        s.conn,
		object:      s.conn.Object(dbusServiceName, path),
		interactive: s.interactive,
	}
}

type dbusSession struct {
	path string
}

func (s dbusSession) Path() string { return s.path }

func call(ctx context.Context, object dbus.BusObject, method string, args ...interface{}) *dbus.Call {
	return object.CallWithContext(ctx, method, 0, args...)
}
