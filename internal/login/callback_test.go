package login

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// bindLoopback binds an ephemeral IPv4 loopback listener and returns its
// exact redirect URI for the callback path.
func bindLoopback(t *testing.T) (net.Listener, string) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind loopback: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener, fmt.Sprintf("http://%s%s", listener.Addr().String(), CallbackPath)
}

// doCallback issues one request against the bound listener and returns the
// response status and body.
func doCallback(t *testing.T, redirectURI string, mutate func(*http.Request)) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, redirectURI, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if mutate != nil {
		mutate(req)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("callback request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, string(body)
}

func TestCallbackAcceptsValidCode(t *testing.T) {
	t.Parallel()

	listener, redirectURI := bindLoopback(t)
	cb, err := NewCallback(listener, "state-1", redirectURI, "")
	if err != nil {
		t.Fatalf("NewCallback: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() {
		_, waitErr := cb.Wait(context.Background(), 5*time.Second)
		waitDone <- waitErr
	}()

	status, body := doCallback(t, redirectURI, func(r *http.Request) {
		q := r.URL.Query()
		q.Set("code", "the-code")
		q.Set("state", "state-1")
		r.URL.RawQuery = q.Encode()
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if strings.Contains(body, "the-code") || strings.Contains(body, "state-1") {
		t.Fatalf("response reflected request values: %q", body)
	}
	if err := <-waitDone; err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestCallbackRejectsEverythingButTheTerminal(t *testing.T) {
	t.Parallel()

	listener, redirectURI := bindLoopback(t)
	cb, err := NewCallback(listener, "state-1", redirectURI, "https://issuer.example/oauth")
	if err != nil {
		t.Fatalf("NewCallback: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() {
		_, waitErr := cb.Wait(context.Background(), 10*time.Second)
		waitDone <- waitErr
	}()
	defer func() {
		if err := <-waitDone; err != nil {
			t.Fatalf("Wait: %v", err)
		}
	}()

	valid := func() *url.Values {
		q := &url.Values{}
		q.Set("code", "the-code")
		q.Set("state", "state-1")
		q.Set("iss", "https://issuer.example/oauth")
		return q
	}

	cases := []struct {
		name   string
		mutate func(*http.Request)
		status int
	}{
		{"wrong method", func(r *http.Request) {
			r.Method = http.MethodPost
			r.URL.RawQuery = valid().Encode()
		}, http.StatusMethodNotAllowed},
		{"wrong path", func(r *http.Request) {
			r.URL.Path = "/other"
			r.URL.RawQuery = valid().Encode()
		}, http.StatusNotFound},
		{"wrong host", func(r *http.Request) {
			r.Host = "127.0.0.1:1"
			r.URL.RawQuery = valid().Encode()
		}, http.StatusNotFound},
		{"duplicate code parameter", func(r *http.Request) {
			q := valid()
			q.Add("code", "other-code")
			r.URL.RawQuery = q.Encode()
		}, http.StatusBadRequest},
		{"duplicate state parameter", func(r *http.Request) {
			q := valid()
			q.Add("state", "state-1")
			r.URL.RawQuery = q.Encode()
		}, http.StatusBadRequest},
		{"missing state", func(r *http.Request) {
			q := valid()
			q.Del("state")
			r.URL.RawQuery = q.Encode()
		}, http.StatusBadRequest},
		{"wrong state", func(r *http.Request) {
			q := valid()
			q.Set("state", "forged")
			r.URL.RawQuery = q.Encode()
		}, http.StatusBadRequest},
		{"missing required issuer", func(r *http.Request) {
			q := valid()
			q.Del("iss")
			r.URL.RawQuery = q.Encode()
		}, http.StatusBadRequest},
		{"wrong required issuer", func(r *http.Request) {
			q := valid()
			q.Set("iss", "https://other.example/oauth")
			r.URL.RawQuery = q.Encode()
		}, http.StatusBadRequest},
		{"code and error together", func(r *http.Request) {
			q := valid()
			q.Set("error", "access_denied")
			r.URL.RawQuery = q.Encode()
		}, http.StatusBadRequest},
		{"neither code nor error", func(r *http.Request) {
			q := valid()
			q.Del("code")
			r.URL.RawQuery = q.Encode()
		}, http.StatusBadRequest},
		{"oversized code", func(r *http.Request) {
			q := valid()
			q.Set("code", strings.Repeat("c", maxCallbackCodeBytes+1))
			r.URL.RawQuery = q.Encode()
		}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, _ := doCallback(t, redirectURI, tc.mutate)
			if status != tc.status {
				t.Fatalf("status = %d, want %d", status, tc.status)
			}
		})
	}
	// A rejected callback never consumes the attempt: a valid one still
	// terminates the wait.
	if status, _ := doCallback(t, redirectURI, func(r *http.Request) {
		r.URL.RawQuery = valid().Encode()
	}); status != http.StatusOK {
		t.Fatalf("terminal after rejections = %d, want 200", status)
	}
}

func TestCallbackOversizedQueryFailsClosed(t *testing.T) {
	t.Parallel()

	listener, redirectURI := bindLoopback(t)
	cb, err := NewCallback(listener, "state-1", redirectURI, "")
	if err != nil {
		t.Fatalf("NewCallback: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() {
		_, waitErr := cb.Wait(context.Background(), 300*time.Millisecond)
		waitDone <- waitErr
	}()
	// The oversized query is rejected by the bounded request head before
	// the handler; it is a non-terminal 4xx, and the wait must still run
	// to its deadline rather than selecting a bogus outcome.
	status, _ := doCallback(t, redirectURI, func(r *http.Request) {
		r.URL.RawQuery = "state=state-1&" + strings.Repeat("p=", maxCallbackQueryBytes)
	})
	if status < 400 || status >= 500 {
		t.Fatalf("status = %d, want a 4xx rejection", status)
	}
	if waitErr := <-waitDone; !errors.Is(waitErr, ErrCallbackTimeout) {
		t.Fatalf("Wait = %v, want ErrCallbackTimeout: no terminal outcome may come from a rejected request", waitErr)
	}

	// Unit-level: the handler itself rejects an oversized query even when
	// the server head bound is higher.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, redirectURI, nil)
	req.URL.RawQuery = "state=state-1&" + strings.Repeat("p=", maxCallbackQueryBytes)
	cb.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("handler status = %d, want 400", rec.Code)
	}
}

func TestCallbackIssuerNotRequiredIgnoresExtraIssuer(t *testing.T) {
	t.Parallel()

	listener, redirectURI := bindLoopback(t)
	cb, err := NewCallback(listener, "state-1", redirectURI, "")
	if err != nil {
		t.Fatalf("NewCallback: %v", err)
	}
	outcome, waitErr := waitOne(t, cb, func() {
		status, _ := doCallback(t, redirectURI, func(r *http.Request) {
			q := r.URL.Query()
			q.Set("code", "the-code")
			q.Set("state", "state-1")
			q.Set("iss", "https://spoof.example/oauth")
			r.URL.RawQuery = q.Encode()
		})
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
	})
	if waitErr != nil {
		t.Fatalf("Wait: %v", waitErr)
	}
	if outcome.Code != "the-code" {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestCallbackSurfacesSanitizedOAuthError(t *testing.T) {
	t.Parallel()

	listener, redirectURI := bindLoopback(t)
	cb, err := NewCallback(listener, "state-1", redirectURI, "")
	if err != nil {
		t.Fatalf("NewCallback: %v", err)
	}
	run := func(attempt *Callback, attemptURI, errorValue string) string {
		t.Helper()
		outcome, waitErr := waitOne(t, attempt, func() {
			status, _ := doCallback(t, attemptURI, func(r *http.Request) {
				q := r.URL.Query()
				q.Set("state", "state-1")
				q.Set("error", errorValue)
				r.URL.RawQuery = q.Encode()
			})
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
		})
		if waitErr != nil {
			t.Fatalf("Wait: %v", waitErr)
		}
		return outcome.OAuthError
	}
	if got := run(cb, redirectURI, "access_denied"); got != "access_denied" {
		t.Fatalf("sanitized = %q", got)
	}
	// Each attempt is single-use: rebuild for the second case.
	listener2, redirect2 := bindLoopback(t)
	cb2, err := NewCallback(listener2, "state-1", redirect2, "")
	if err != nil {
		t.Fatalf("NewCallback: %v", err)
	}
	if got := run(cb2, redirect2, "EVIL: free text with code=1"); got != "unknown" {
		t.Fatalf("sanitized = %q, want unknown", got)
	}
}

// waitOne runs the callback's Wait in a background goroutine, runs the
// probe that drives the attempt, and returns the terminal outcome.
func waitOne(t *testing.T, cb *Callback, probe func()) (Outcome, error) {
	t.Helper()
	type result struct {
		outcome Outcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		outcome, err := cb.Wait(context.Background(), 5*time.Second)
		done <- result{outcome, err}
	}()
	probe()
	select {
	case r := <-done:
		return r.outcome, r.err
	case <-time.After(10 * time.Second):
		t.Fatal("callback wait did not finish")
		return Outcome{}, errors.New("wait timeout")
	}
}

func TestCallbackConcurrentCodesSelectOne(t *testing.T) {
	t.Parallel()

	listener, redirectURI := bindLoopback(t)
	cb, err := NewCallback(listener, "state-1", redirectURI, "")
	if err != nil {
		t.Fatalf("NewCallback: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, waitErr := cb.Wait(context.Background(), 5*time.Second)
		done <- waitErr
	}()

	// Both connections establish before either request is written, so the
	// race is real: the listener is still bound when the second connects.
	conns := make([]net.Conn, 2)
	for i := range conns {
		conn, err := net.Dial("tcp4", listener.Addr().String())
		if err != nil {
			t.Fatalf("connect %d: %v", i, err)
		}
		conns[i] = conn
	}
	host := listener.Addr().String()
	request := "GET " + CallbackPath + "?code=contending&state=state-1 HTTP/1.1\r\nHost: " + host + "\r\nConnection: close\r\n\r\n"
	for i, conn := range conns {
		if _, err := conn.Write([]byte(request)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	var got200, lost int
	for i, conn := range conns {
		defer func() { _ = conn.Close() }()
		buf := make([]byte, 24)
		_, err := conn.Read(buf)
		switch {
		case strings.Contains(string(buf), " 200 "):
			got200++
		case strings.Contains(string(buf), " 410 "), err != nil:
			// The loser gets the fixed 410 page, or the connection closed by
			// the attempt's shutdown as soon as the winner was selected:
			// either way a second terminal was never served.
			lost++
		default:
			t.Fatalf("response %d = %q", i, buf)
		}
	}
	if got200 != 1 || lost != 1 {
		t.Fatalf("outcomes: 200=%d lost=%d, want exactly one winner and one loser", got200, lost)
	}
	if err := <-done; err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestCallbackClosesListenerOnEveryPath(t *testing.T) {
	t.Parallel()

	// Timeout path: no callback arrives at all.
	listener, _ := bindLoopback(t)
	cb, err := NewCallback(listener, "state-1", fmt.Sprintf("http://%s%s", listener.Addr().String(), CallbackPath), "")
	if err != nil {
		t.Fatalf("NewCallback: %v", err)
	}
	if _, err := cb.Wait(context.Background(), 100*time.Millisecond); !errors.Is(err, ErrCallbackTimeout) {
		t.Fatalf("Wait = %v, want ErrCallbackTimeout", err)
	}
	if _, err := net.Dial("tcp4", listener.Addr().String()); err == nil {
		t.Fatal("listener still open after timeout")
	}

	// Cancellation path.
	listener2, redirect2 := bindLoopback(t)
	cb2, err := NewCallback(listener2, "state-1", redirect2, "")
	if err != nil {
		t.Fatalf("NewCallback: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cb2.Wait(ctx, 5*time.Second); !errors.Is(err, ErrCallbackCancelled) {
		t.Fatalf("Wait = %v, want ErrCallbackCancelled", err)
	}
	if _, err := net.Dial("tcp4", listener2.Addr().String()); err == nil {
		t.Fatal("listener still open after cancellation")
	}
}

func TestNewCallbackValidation(t *testing.T) {
	t.Parallel()

	listener, redirectURI := bindLoopback(t)

	cases := []struct {
		name     string
		state    string
		redirect string
		issuer   string
		listener net.Listener
	}{
		{"nil listener", "state", redirectURI, "", nil},
		{"missing state", "", redirectURI, "", listener},
		{"non-loopback redirect", "state", "https://127.0.0.1/oauth/callback", "", listener},
		{"wrong redirect path", "state", "http://" + listener.Addr().String() + "/other", "", listener},
		{"redirect on another port", "state", "http://127.0.0.1:1/oauth/callback", "", listener},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewCallback(tc.listener, tc.state, tc.redirect, tc.issuer); err == nil {
				t.Fatal("NewCallback accepted the fixture")
			}
		})
	}
	if _, err := NewCallback(listener, "state", redirectURI, ""); err != nil {
		t.Fatalf("NewCallback rejected a valid fixture: %v", err)
	}
}

// TestCallbackShutdownClosesIdleConnections proves the server goroutine's
// shutdown path: a connection that sends nothing cannot keep Wait blocked
// after a terminal arrives, and the attempt's shutdown closes it instead of
// leaking the read until the 10-second header timeout.
func TestCallbackShutdownClosesIdleConnections(t *testing.T) {
	t.Parallel()

	listener, redirectURI := bindLoopback(t)
	cb, err := NewCallback(listener, "state-1", redirectURI, "")
	if err != nil {
		t.Fatalf("NewCallback: %v", err)
	}
	idle, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial idle connection: %v", err)
	}
	defer func() { _ = idle.Close() }()

	waitDone := make(chan error, 1)
	go func() {
		_, waitErr := cb.Wait(context.Background(), 30*time.Second)
		waitDone <- waitErr
	}()

	start := time.Now()
	status, _ := doCallback(t, redirectURI, func(r *http.Request) {
		q := r.URL.Query()
		q.Set("code", "the-code")
		q.Set("state", "state-1")
		r.URL.RawQuery = q.Encode()
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if err := <-waitDone; err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Wait took %s with an idle connection open, want the shutdown to bound it", elapsed)
	}

	// The shutdown must have closed the idle connection: a read returns
	// EOF promptly instead of blocking on a header that never comes.
	_ = idle.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	n, readErr := idle.Read(buf)
	if n != 0 || readErr == nil {
		t.Fatalf("idle read = %d, %v; want a closed connection", n, readErr)
	}
}
