package login

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"
)

const (
	// CallbackPath is the fixed path every login attempt serves. It is not
	// user-configurable and never follows a wildcard.
	CallbackPath = "/oauth/callback"

	// maxCallbackQueryBytes bounds one callback's encoded query string:
	// a code plus state plus an optional issuer is a few hundred bytes.
	maxCallbackQueryBytes = 8 * 1024

	// maxCallbackHeaderBytes bounds one callback's request head.
	maxCallbackHeaderBytes = 8 * 1024

	// maxCallbackCodeBytes bounds the authorization code itself.
	maxCallbackCodeBytes = 512

	// maxOAuthErrorBytes bounds a sanitized OAuth error code.
	maxOAuthErrorBytes = 64
)

// Outcome is the single terminal callback one wait selects: either the
// authorization code or the sanitized OAuth error the provider reported.
type Outcome struct {
	// Code is the authorization code, empty when the terminal callback
	// carried an OAuth error.
	Code string
	// OAuthError is a sanitized OAuth error code when the user or the
	// provider rejected the attempt. Free-text descriptions never surface.
	OAuthError string
}

// Callback is one bounded, single-use loopback callback attempt. It serves
// exactly one terminal response on the pre-bound listener, then the caller
// closes the listener; every other request gets a fixed non-reflective
// response.
type Callback struct {
	listener     net.Listener
	state        string
	redirectHost string
	issuer       string

	mu       sync.Mutex
	decided  bool
	terminal chan Outcome
}

// NewCallback arms one callback attempt on the bound listener. state is the
// value the callback must echo, redirectURI is the exact URI the
// authorization request carries (the listener must serve it), and issuer is
// the value the callback must echo in iss, or empty when the server does
// not advertise the issuer response parameter.
func NewCallback(listener net.Listener, state, redirectURI, issuer string) (*Callback, error) {
	if listener == nil {
		return nil, errors.New("callback listener is required")
	}
	if state == "" {
		return nil, errors.New("callback state is required")
	}
	u, err := url.Parse(redirectURI)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Path != CallbackPath {
		return nil, fmt.Errorf("redirect uri must be the exact bound loopback callback uri")
	}
	if addr, ok := listener.Addr().(*net.TCPAddr); !ok || !addr.IP.IsLoopback() {
		return nil, errors.New("callback listener must be bound to the IPv4 loopback")
	}
	if u.Host != listener.Addr().String() {
		return nil, errors.New("redirect uri does not match the bound listener")
	}
	return &Callback{
		listener:     listener,
		state:        state,
		redirectHost: u.Host,
		issuer:       issuer,
		terminal:     make(chan Outcome, 1),
	}, nil
}

// Wait blocks until exactly one validated terminal callback, the deadline,
// or caller cancellation, and closes the listener on every exit path. The
// default deadline is five minutes.
func (c *Callback) Wait(ctx context.Context, deadline time.Duration) (Outcome, error) {
	if deadline <= 0 {
		deadline = DefaultCallbackDeadline
	}
	waitCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	srv := &http.Server{
		Handler:        c,
		ErrorLog:       log.New(io.Discard, "", 0),
		MaxHeaderBytes: maxCallbackHeaderBytes,
	}
	srv.SetKeepAlivesEnabled(false)

	// The server goroutine is owned by this call: it ends when Wait closes
	// the listener, and Wait joins it before returning.
	served := make(chan error, 1)
	go func() { served <- srv.Serve(c.listener) }()

	var outcome Outcome
	got := false
	select {
	case outcome = <-c.terminal:
		got = true
	case <-waitCtx.Done():
	}
	_ = c.listener.Close()
	<-served
	if got {
		return outcome, nil
	}
	if ctx.Err() != nil {
		return Outcome{}, fmt.Errorf("%w: %v", ErrCallbackCancelled, ctx.Err())
	}
	return Outcome{}, ErrCallbackTimeout
}

// securityHeaders are the restrictive response headers every callback
// response carries.
func securityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
}

// acceptPage and rejectPage are fixed local HTML with no reflected query
// values.
const (
	acceptPage = `<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>Tama Link</title>
<style>body{font-family:system-ui,sans-serif;background:#101418;color:#e6edf3;display:grid;place-items:center;height:100vh;margin:0}</style>
</head><body><main><h1>Authorization complete</h1><p>You can close this tab and return to Tama Link.</p></main></body></html>`

	rejectPage = `<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>Tama Link</title>
<style>body{font-family:system-ui,sans-serif;background:#101418;color:#e6edf3;display:grid;place-items:center;height:100vh;margin:0}</style>
</head><body><main><h1>Authorization callback rejected</h1><p>This callback was not accepted. Return to Tama Link and try again.</p></main></body></html>`
)

// ServeHTTP validates one callback and selects at most one terminal
// response for the attempt.
func (c *Callback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		// A handler panic must not strand the attempt: answer and let
		// the normal deadline bound the wait.
		if recover() != nil {
			c.reject(w, http.StatusInternalServerError)
		}
	}()
	if r.Method != http.MethodGet {
		c.reject(w, http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path != CallbackPath || r.Host != c.redirectHost {
		c.reject(w, http.StatusNotFound)
		return
	}
	if len(r.URL.RawQuery) > maxCallbackQueryBytes {
		c.reject(w, http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	for _, key := range []string{"code", "state", "iss", "error", "error_description"} {
		if len(q[key]) > 1 {
			c.reject(w, http.StatusBadRequest)
			return
		}
	}
	state := q.Get("state")
	if state == "" || !constantTimeEqual(state, c.state) {
		c.reject(w, http.StatusBadRequest)
		return
	}
	if c.issuer != "" && !constantTimeEqual(q.Get("iss"), c.issuer) {
		c.reject(w, http.StatusBadRequest)
		return
	}
	code := q.Get("code")
	oauthError := q.Get("error")
	var outcome Outcome
	switch {
	case code != "" && oauthError != "":
		c.reject(w, http.StatusBadRequest)
		return
	case code != "":
		if len(code) > maxCallbackCodeBytes {
			c.reject(w, http.StatusBadRequest)
			return
		}
		outcome = Outcome{Code: code}
	case oauthError != "":
		outcome = Outcome{OAuthError: sanitizeOAuthError(oauthError)}
	default:
		c.reject(w, http.StatusBadRequest)
		return
	}
	if !c.selectTerminal(outcome) {
		c.reject(w, http.StatusGone)
		return
	}
	securityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, acceptPage)
}

// selectTerminal records the one terminal outcome for the attempt. Only the
// first valid callback wins; every later request is rejected as gone.
func (c *Callback) selectTerminal(outcome Outcome) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.decided {
		return false
	}
	c.decided = true
	select {
	case c.terminal <- outcome:
	default:
	}
	return true
}

// reject answers a non-terminal request with fixed local HTML.
func (c *Callback) reject(w http.ResponseWriter, status int) {
	securityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, rejectPage)
}

// constantTimeEqual compares two callback values in constant time.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// oauthErrorCodePattern is the sanitized OAuth error code grammar: an
// enum-like token from the OAuth error registry.
var oauthErrorCodePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// sanitizeOAuthError keeps only the provider's enum-like error code. Free
// text and unrecognized values surface as "unknown", so provider prose that
// could embed request data never reaches diagnostics.
func sanitizeOAuthError(value string) string {
	if len(value) > maxOAuthErrorBytes || !oauthErrorCodePattern.MatchString(value) {
		return "unknown"
	}
	return value
}
