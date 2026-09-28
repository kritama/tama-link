package login

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
)

// bindCallbackListener binds the exact callback address before dynamic
// registration. A first login asks the OS for a free high port; subsequent
// logins bind the redirect persisted with the reusable client registration.
// If that port is unavailable, the attempt fails without replacing the
// existing registration or its refresh credential.
func (s *Service) bindCallbackListener(ctx context.Context) (net.Listener, string, error) {
	bindAddress := "127.0.0.1:0"
	rec, found, err := s.client.RegisteredClient(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("load registered callback: %w", err)
	}
	if found && rec.RedirectURI != "" {
		bindAddress, err = registeredCallbackAddress(rec.RedirectURI)
		if err != nil {
			return nil, "", err
		}
	}

	listener, err := net.Listen("tcp4", bindAddress)
	if err != nil {
		if found && rec.RedirectURI != "" {
			return nil, "", fmt.Errorf("bind registered loopback callback %s: %w", bindAddress, err)
		}
		return nil, "", fmt.Errorf("bind loopback listener: %w", err)
	}
	redirectURI := fmt.Sprintf("http://%s%s", listener.Addr().String(), CallbackPath)
	return listener, redirectURI, nil
}

func registeredCallbackAddress(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" ||
		u.Path != CallbackPath || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("stored client registration has an invalid loopback callback")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1024 || port > 65535 {
		return "", fmt.Errorf("stored client registration has an invalid loopback callback port")
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil
}
