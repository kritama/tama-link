package secretservice

import (
	"context"
	"fmt"

	"github.com/godbus/dbus"
)

type dbusCollection struct {
	conn        *dbus.Conn
	object      dbus.BusObject
	interactive bool
}

func (c *dbusCollection) Path() string { return string(c.object.Path()) }

func (c *dbusCollection) Label(ctx context.Context) (string, error) {
	return stringProperty(ctx, c.object, "org.freedesktop.Secret.Collection.Label")
}

func (c *dbusCollection) Locked(ctx context.Context) (bool, error) {
	return boolProperty(ctx, c.object, "org.freedesktop.Secret.Collection.Locked")
}

func (c *dbusCollection) Items(ctx context.Context) ([]Item, error) {
	paths, err := objectPaths(ctx, c.object, "org.freedesktop.Secret.Collection.Items")
	if err != nil {
		return nil, err
	}
	return c.items(paths), nil
}

func (c *dbusCollection) SearchItems(ctx context.Context, attributes map[string]string) ([]Item, error) {
	var paths []dbus.ObjectPath
	err := call(ctx, c.object, "org.freedesktop.Secret.Collection.SearchItems", attributes).Store(&paths)
	if err != nil {
		return nil, err
	}
	return c.items(paths), nil
}

func (c *dbusCollection) CreateItem(ctx context.Context, session Session, label string, secret Secret, replace bool) (Item, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if session == nil || isRootPath(session.Path()) {
		return nil, fmt.Errorf("create secret service item: session is required")
	}
	properties := map[string]dbus.Variant{
		"org.freedesktop.Secret.Item.Label": dbus.MakeVariant(label),
		"org.freedesktop.Secret.Item.Attributes": dbus.MakeVariant(map[string]string{
			itemKeyAttribute: label,
		}),
	}
	payload := dbusSecret{
		Session:     dbus.ObjectPath(session.Path()),
		Parameters:  []byte{},
		Value:       append([]byte{}, secret.Value...),
		ContentType: secret.ContentType,
	}
	var path dbus.ObjectPath
	var prompt dbus.ObjectPath
	err := call(ctx, c.object, "org.freedesktop.Secret.Collection.CreateItem", properties, payload, replace).Store(&path, &prompt)
	if err != nil {
		return nil, err
	}
	result, err := finishPrompt(ctx, c.conn, prompt, c.interactive)
	if err != nil {
		return nil, err
	}
	if isRootPath(string(path)) {
		prompted, ok := result.Value().(dbus.ObjectPath)
		if !ok || isRootPath(string(prompted)) {
			return nil, fmt.Errorf("create secret service item: empty item path")
		}
		path = prompted
	}
	return c.item(path), nil
}

func (c *dbusCollection) items(paths []dbus.ObjectPath) []Item {
	items := make([]Item, 0, len(paths))
	for _, path := range paths {
		items = append(items, c.item(path))
	}
	return items
}

func (c *dbusCollection) item(path dbus.ObjectPath) *dbusItem {
	return &dbusItem{
		conn:        c.conn,
		object:      c.conn.Object(dbusServiceName, path),
		interactive: c.interactive,
	}
}

type dbusItem struct {
	conn        *dbus.Conn
	object      dbus.BusObject
	interactive bool
}

func (item *dbusItem) Path() string { return string(item.object.Path()) }

func (item *dbusItem) Label(ctx context.Context) (string, error) {
	return stringProperty(ctx, item.object, "org.freedesktop.Secret.Item.Label")
}

func (item *dbusItem) Attributes(ctx context.Context) (map[string]string, error) {
	value, err := property(ctx, item.object, "org.freedesktop.Secret.Item.Attributes")
	if err != nil {
		return nil, err
	}
	switch attributes := value.Value().(type) {
	case map[string]string:
		return cloneAttributes(attributes), nil
	default:
		return nil, fmt.Errorf("secret service item attributes have unexpected type %T", value.Value())
	}
}

func (item *dbusItem) Locked(ctx context.Context) (bool, error) {
	return boolProperty(ctx, item.object, "org.freedesktop.Secret.Item.Locked")
}

func (item *dbusItem) GetSecret(ctx context.Context, session Session) (Secret, error) {
	if session == nil || isRootPath(session.Path()) {
		return Secret{}, fmt.Errorf("read secret service item: session is required")
	}
	var payload dbusSecret
	err := call(ctx, item.object, "org.freedesktop.Secret.Item.GetSecret", dbus.ObjectPath(session.Path())).Store(&payload)
	if err != nil {
		return Secret{}, err
	}
	return Secret{Value: append([]byte{}, payload.Value...), ContentType: payload.ContentType}, nil
}

func (item *dbusItem) Delete(ctx context.Context) error {
	var prompt dbus.ObjectPath
	if err := call(ctx, item.object, "org.freedesktop.Secret.Item.Delete").Store(&prompt); err != nil {
		return err
	}
	_, err := finishPrompt(ctx, item.conn, prompt, item.interactive)
	return err
}

func cloneAttributes(attributes map[string]string) map[string]string {
	cloned := make(map[string]string, len(attributes))
	for key, value := range attributes {
		cloned[key] = value
	}
	return cloned
}
