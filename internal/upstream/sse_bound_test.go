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
			_, err := client.Discover(context.Background())
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
		_, err := client.Discover(context.Background())
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
		_, err := client.Discover(context.Background())
		var uerr *Error
		if !errors.As(err, &uerr) || uerr.Kind != KindTransport {
			t.Fatalf("err = %v, want transport (closed without a response)", err)
		}
	})
}
