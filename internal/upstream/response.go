package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// readResult consumes one call response and returns the raw JSON-RPC result
// value. The response is either a single application/json body or an SSE
// stream carrying exactly the matching response.
func (c *Client) readResult(method, requestID string, resp *http.Response) (json.RawMessage, error) {
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		drain(resp.Body)
		return nil, newError(KindAuth, resp.StatusCode, nil)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.readHTTPError(resp)
	}
	contentType := baseMediaType(resp.Header.Get("Content-Type"))
	switch contentType {
	case "application/json":
		return c.readJSONResult(method, requestID, resp.Body)
	case "text/event-stream":
		var result json.RawMessage
		sawResponse := false
		err := c.scanSSE(resp.Body, func(msg jsonrpc.Message) (bool, error) {
			r, done, derr := matchResult(msg, requestID)
			if derr != nil {
				return true, derr
			}
			if done {
				result = r
				sawResponse = true
				return true, nil
			}
			return false, nil
		})
		if err != nil {
			return nil, err
		}
		if !sawResponse {
			return nil, newError(KindTransport, 0, fmt.Errorf("%s stream closed without a response", method))
		}
		return result, nil
	default:
		drain(resp.Body)
		return nil, newError(KindTransport, 0, fmt.Errorf("unsupported content type %q", contentType))
	}
}

// readJSONResult decodes a single application/json response body.
func (c *Client) readJSONResult(method, requestID string, body io.Reader) (json.RawMessage, error) {
	data, err := readBounded(body, c.maxBytes)
	if err != nil {
		return nil, err
	}
	msg, err := jsonrpc.DecodeMessage(data)
	if err != nil {
		return nil, newError(KindTransport, 0, fmt.Errorf("decode %s response: jsonrpc", method))
	}
	if werr, ok := asWireError(msg); ok {
		return nil, protocolError(werr)
	}
	result, done, err := matchResult(msg, requestID)
	if err != nil {
		return nil, err
	}
	if !done {
		return nil, newError(KindTransport, 0, fmt.Errorf("%s response id mismatch", method))
	}
	return result, nil
}

// readHTTPError classifies a non-200 response, extracting a JSON-RPC error
// object when the body carries one.
func (c *Client) readHTTPError(resp *http.Response) error {
	data, err := readBounded(resp.Body, c.maxBytes)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) > 0 {
		if msg, err := jsonrpc.DecodeMessage(data); err == nil {
			if werr, ok := asWireError(msg); ok {
				return protocolError(werr)
			}
		}
	}
	return newError(KindHTTP, resp.StatusCode, nil)
}

// readBounded reads at most bound+1 bytes and fails closed on overflow so a
// hostile or oversized body is never allocated past its bound.
func readBounded(r io.Reader, bound int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, bound+1))
	if err != nil {
		return nil, newError(KindTransport, 0, fmt.Errorf("read upstream body: %w", err))
	}
	if int64(len(data)) > bound {
		return nil, newError(KindTooLarge, 0, fmt.Errorf("body exceeds %d bytes", bound))
	}
	return data, nil
}

// matchResult reports whether msg is the response to requestID and returns
// its raw result. A JSON-RPC error response is reported as a protocol error.
func matchResult(msg jsonrpc.Message, requestID string) (json.RawMessage, bool, error) {
	resp, ok := msg.(*jsonrpc.Response)
	if !ok {
		return nil, false, newError(KindTransport, 0, fmt.Errorf("unexpected server request in response stream"))
	}
	if !idMatches(resp.ID, requestID) {
		return nil, false, nil
	}
	if werr, ok := asWireError(resp); ok {
		return nil, false, protocolError(werr)
	}
	return resp.Result, true, nil
}

// idMatches compares a response ID with the string request ID that produced
// it.
func idMatches(id jsonrpc.ID, requestID string) bool {
	raw := id.Raw()
	s, ok := raw.(string)
	return ok && s == requestID
}

// asWireError reports whether msg carries a JSON-RPC error object.
func asWireError(msg jsonrpc.Message) (*jsonrpc.Error, bool) {
	resp, ok := msg.(*jsonrpc.Response)
	if !ok || resp.Error == nil {
		return nil, false
	}
	var werr *jsonrpc.Error
	if !errors.As(resp.Error, &werr) {
		return nil, false
	}
	return werr, true
}

// protocolError classifies a JSON-RPC error object as a protocol failure.
func protocolError(werr *jsonrpc.Error) error {
	return newError(KindProtocol, int(werr.Code), fmt.Errorf("jsonrpc code %d", werr.Code))
}

// scanSSE consumes an SSE stream, dispatching each data payload as a decoded
// JSON-RPC message until dispatch stops, an error occurs, or the stream
// closes. Every frame counts against the per-event bound; the stream itself
// is bounded by the caller context. A clean close without a stop signal
// returns nil: stream end is an ordinary outcome the caller reconciles.
func (c *Client) scanSSE(r io.Reader, dispatch func(msg jsonrpc.Message) (stop bool, err error)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), int(c.maxBytes))
	var data []string
	for scanner.Scan() {
		line := scanner.Text()
		if int64(len(line)) > c.maxBytes {
			return newError(KindTooLarge, 0, fmt.Errorf("SSE line exceeds %d bytes", c.maxBytes))
		}
		if line == "" {
			payload := strings.Join(data, "\n")
			data = data[:0]
			if payload == "" {
				continue
			}
			msg, err := jsonrpc.DecodeMessage([]byte(payload))
			if err != nil {
				return newError(KindTransport, 0, fmt.Errorf("decode SSE payload: jsonrpc"))
			}
			stop, err := dispatch(msg)
			if stop || err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // keepalive comment
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, stripOneSpace(value))
		}
		// event:, id:, retry: fields carry no payload for this contract.
	}
	if err := scanner.Err(); err != nil {
		if isContextError(err) {
			return err
		}
		return newError(KindTooLarge, 0, fmt.Errorf("SSE frame exceeds bound: %w", err))
	}
	return nil
}

// stripOneSpace removes the single optional space after a field colon, per
// the SSE grammar.
func stripOneSpace(s string) string {
	if strings.HasPrefix(s, " ") {
		return s[1:]
	}
	return s
}

// isContextError reports whether err wraps context cancellation or deadline.
func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// baseMediaType extracts the media type without parameters.
func baseMediaType(header string) string {
	if i := strings.IndexByte(header, ';'); i >= 0 {
		header = header[:i]
	}
	return strings.TrimSpace(strings.ToLower(header))
}

// drain consumes and closes a response body without inspecting it.
func drain(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4096))
	_ = body.Close()
}
