package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newBoundClient builds a client with one small event bound.
func newBoundClient(t *testing.T, url string, bound int64) *Client {
	t.Helper()
	client, err := New(Config{
		Endpoint:           url,
		ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "0.1.0"},
		ClientCapabilities: json.RawMessage(`{}`),
		TokenProvider:      func(context.Context) (string, error) { return "t", nil },
		MaxResponseBytes:   bound,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

// sseIDServer answers one request with a scripted SSE body, rendering it
// with the request's JSON-RPC id.
func sseIDServer(t *testing.T, build func(id string) string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 8192))
		var envelope struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &envelope)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, build(envelope.ID))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// padTo renders a valid discover response padded to exactly want bytes.
func padTo(t *testing.T, id string, want int) string {
	t.Helper()
	result := `{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}},"pad":""}`
	base := fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"result":%s}`, id, result)
	padding := want - len(base)
	if padding < 0 {
		t.Fatalf("pad target %d smaller than base %d", want, len(base))
	}
	result = fmt.Sprintf(`{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}},"pad":"%s"}`, strings.Repeat("x", padding))
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"result":%s}`, id, result)
}

// TestSSEEventBound covers the complete-event bound: one line and many
// individually valid lines both count, measured immediately below, exactly
// at, and above the configured limit.
func TestSSEEventBound(t *testing.T) {
	const bound = 256

	// A single data line: the encoded count is len(payload) + 1.
	cases := []struct {
		name string
		// payloadLen produces a payload of that exact length;
		// count = len(payload) + 1.
		payloadLen int
		wantErr    bool
	}{
		{"below", bound - 2, false}, // count 255
		{"at", bound - 1, false},    // count 256
		{"above", bound, true},      // count 257
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := sseIDServer(t, func(id string) string {
				return sseReply(padTo(t, id, tc.payloadLen))
			})
			client := newBoundClient(t, ts.URL, bound)
			_, err := client.Discover(context.Background(), 0)
			if tc.wantErr {
				var uerr *Error
				if !errors.As(err, &uerr) || uerr.Kind != KindTooLarge {
					t.Fatalf("err = %v, want too-large kind", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
		})
	}

	t.Run("multi-line accumulation exceeds bound", func(t *testing.T) {
		// Three individually valid lines whose joined payload exceeds the
		// bound: the cumulative count must trigger the failure.
		ts := sseIDServer(t, func(id string) string {
			payload := padTo(t, id, 300)
			// Split the raw payload into three data lines verbatim; each line
			// is individually below the bound but the joined payload exceeds
			// it.
			var data strings.Builder
			parts := [3]string{payload[:100], payload[100:200], payload[200:]}
			for _, p := range parts {
				data.WriteString("data: ")
				data.WriteString(p)
				data.WriteByte('\n')
			}
			data.WriteByte('\n')
			return data.String()
		})
		client := newBoundClient(t, ts.URL, bound)
		_, err := client.Discover(context.Background(), 0)
		var uerr *Error
		if !errors.As(err, &uerr) || uerr.Kind != KindTooLarge {
			t.Fatalf("err = %v, want too-large kind for multi-line accumulation", err)
		}
	})

	t.Run("unterminated final event is dropped", func(t *testing.T) {
		// The final event never receives its blank-line delimiter: per the
		// WHATWG parser it is not dispatched, and the caller sees a clean
		// close without a response.
		ts := sseIDServer(t, func(id string) string {
			return "data: " + padTo(t, id, 200) // no trailing blank line
		})
		client := newBoundClient(t, ts.URL, 4096)
		_, err := client.Discover(context.Background(), 0)
		var uerr *Error
		if !errors.As(err, &uerr) || uerr.Kind != KindTransport {
			t.Fatalf("err = %v, want transport (closed without a response)", err)
		}
	})
}

// heldOpenSSEServer answers one subscriptions/listen or finite request with
// a scripted SSE body, flushes it, and then deliberately keeps the HTTP
// response open until the client goes away.
func heldOpenSSEServer(t *testing.T, build func(id string) string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 8192))
		var envelope struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &envelope)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprint(w, build(envelope.ID))
		flusher.Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestFiniteResponseStopsScanImmediately proves a finite request that has
// already received its matching JSON-RPC response returns as soon as the
// event is parsed, even while the server keeps the response body open.
func TestFiniteResponseStopsScanImmediately(t *testing.T) {
	ts := heldOpenSSEServer(t, func(id string) string {
		result := `{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}},"instructions":"x"}`
		return sseReply(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"result":%s}`, id, result))
	})
	client, err := New(Config{
		Endpoint:           ts.URL,
		ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "0.1.0"},
		ClientCapabilities: json.RawMessage(`{}`),
		TokenProvider:      func(context.Context) (string, error) { return "t", nil },
		MaxResponseBytes:   1 << 20,
		RequestTimeout:     400 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	start := time.Now()
	if _, err := client.Discover(context.Background(), 0); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Errorf("Discover waited on the held-open body: %s", elapsed)
	}
}

// TestSubscriptionFinalResponseStopsScanImmediately proves a subscription
// that has received its graceful final response returns nil as soon as the
// event is parsed, even while the server keeps the stream open.
func TestSubscriptionFinalResponseStopsScanImmediately(t *testing.T) {
	ts := heldOpenSSEServer(t, func(id string) string {
		return ackEvent(id, subTaskID) + finalResponse(id)
	})
	client := newSubClient(t, ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if err := client.Subscribe(ctx, []string{subTaskID}, &SubscribeCallbacks{
		OnAcknowledged: func([]string) error { return nil },
		OnTask:         func(TaskState) error { return nil },
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 1*time.Second {
		t.Errorf("Subscribe waited on the held-open stream: %s", elapsed)
	}
}

// TestSuppliedClientTimeoutDoesNotBoundSubscriptions proves a caller-supplied
// HTTP client's overall timeout does not terminate a subscription stream:
// the clone zeroes it while retaining the rest of the caller configuration.
func TestSuppliedClientTimeoutDoesNotBoundSubscriptions(t *testing.T) {
	block := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		var envelope struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &envelope)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, `data: {"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%q},"notifications":{"taskIds":[]}}}`+"\n\n", envelope.ID)
		flusher.Flush()
		<-r.Context().Done()
		close(block)
	}))
	defer ts.Close()
	supplied := &http.Client{Timeout: 50 * time.Millisecond}
	client, err := New(Config{
		Endpoint:           ts.URL,
		ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "0.1.0"},
		ClientCapabilities: json.RawMessage(`{"extensions":{"io.modelcontextprotocol/tasks":{}}}`),
		TokenProvider:      func(context.Context) (string, error) { return "t", nil },
		MaxResponseBytes:   1 << 20,
		HTTPClient:         supplied,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The supplied client itself must not be mutated by the clone.
	if supplied.Timeout != 50*time.Millisecond {
		t.Fatalf("supplied client timeout mutated to %s", supplied.Timeout)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ackedCh := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- client.Subscribe(ctx, []string{"task-1"}, &SubscribeCallbacks{
			OnAcknowledged: func([]string) error { close(ackedCh); return nil },
			OnTask:         func(TaskState) error { return nil },
		})
	}()
	select {
	case <-ackedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never acknowledged")
	}
	// Hold the stream open well past the supplied 50ms client timeout.
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-errCh:
		t.Fatalf("stream died at the supplied client timeout: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close promptly on cancellation")
	}
	<-block
}
