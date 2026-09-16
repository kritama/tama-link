// Package upstream client core: configuration, request dispatch, and the
// stateless streamable-HTTP round trip.
package upstream

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TokenProvider supplies a bearer token for one upstream request.
// Implementations must refresh under their own coordination (the lease-based
// credential store). A provider failure and an endpoint rejection (401/403)
// both classify as KindAuth — unless the provider reports lease contention
// by wrapping ErrTokenContended, which classifies as KindTransport because
// the credential is valid and another process is merely refreshing it.
type TokenProvider func(ctx context.Context) (string, error)

// DefaultRequestTimeout bounds one finite upstream request. It applies per
// request, never to subscription streams, whose lifetime is owned by the
// caller context, credential expiry, and the stream-lifetime owner.
const DefaultRequestTimeout = 60 * time.Second

// Config configures one upstream client for one profile endpoint.
type Config struct {
	// Endpoint is the profile-validated upstream MCP endpoint URL.
	Endpoint string
	// ClientInfo identifies Tama Link in the per-request _meta triple.
	ClientInfo mcp.Implementation
	// ClientCapabilities is the JSON object declared in every request. It
	// must be an object; extension declarations survive verbatim.
	ClientCapabilities json.RawMessage
	// TokenProvider supplies the bearer token per request. Required.
	TokenProvider TokenProvider
	// MaxResponseBytes bounds one response body or one SSE event. Required.
	MaxResponseBytes int64
	// RequestTimeout bounds one finite request (discover, tools/list,
	// tools/call, tasks/*). It does not bound subscription streams. It
	// defaults to DefaultRequestTimeout.
	RequestTimeout time.Duration
	// HTTPClient is optional. Redirects are always rejected: every
	// destination must come from the validated profile, never from a
	// Location header. A supplied client is cloned; its Timeout is not used
	// to bound subscription streams.
	HTTPClient *http.Client
}

// Client is a stateless MCP 2026-07-28 streamable-HTTP client. A Client has
// no protocol session and no mutable per-request state; it is safe for
// concurrent use.
type Client struct {
	endpoint       *url.URL
	info           mcp.Implementation
	meta           json.RawMessage
	tokens         TokenProvider
	maxBytes       int64
	requestTimeout time.Duration
	httpClient     *http.Client
}

// New validates cfg and builds a Client.
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("endpoint is required")
	}
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse endpoint: %w", err)
	}
	if (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
		return nil, fmt.Errorf("endpoint must be an absolute http(s) URL")
	}
	if cfg.ClientInfo.Name == "" || cfg.ClientInfo.Version == "" {
		return nil, fmt.Errorf("client info name and version are required")
	}
	meta, err := buildMeta(cfg.ClientInfo, cfg.ClientCapabilities)
	if err != nil {
		return nil, fmt.Errorf("client capabilities: %w", err)
	}
	if cfg.TokenProvider == nil {
		return nil, fmt.Errorf("token provider is required")
	}
	if cfg.MaxResponseBytes <= 0 {
		return nil, fmt.Errorf("max response bytes must be positive")
	}
	requestTimeout := cfg.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = DefaultRequestTimeout
	}
	// Redirects are rejected on every client, default or supplied: copy the
	// struct and force a refusing callback rather than trusting an existing
	// one. The client carries no overall Timeout, supplied value included,
	// because that would also bound subscription streams; finite requests
	// get a per-request deadline in callWithMeta, and the caller context
	// owns the streams. Transport and other caller configuration survive the
	// clone.
	base := cfg.HTTPClient
	if base == nil {
		base = &http.Client{}
	}
	cloned := *base
	cloned.Timeout = 0
	cloned.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{
		endpoint:       endpoint,
		info:           cfg.ClientInfo,
		meta:           meta,
		tokens:         cfg.TokenProvider,
		maxBytes:       cfg.MaxResponseBytes,
		requestTimeout: requestTimeout,
		httpClient:     &cloned,
	}, nil
}

// Endpoint returns the profile endpoint this client is bound to. Wiring
// layers compare it against the profile to fail on cross-profile reuse.
func (c *Client) Endpoint() string { return c.endpoint.String() }

// call performs one stateless request and returns the raw JSON-RPC result
// value with the client's default capabilities.
func (c *Client) call(ctx context.Context, method, name string, params json.RawMessage) (json.RawMessage, error) {
	return c.callWithMeta(ctx, method, name, params, nil)
}

// callWithMeta performs one stateless finite request. metaOverride, when
// non-nil, replaces the client's default _meta triple for this request only
// (the per-request Tasks capability seam). The per-request deadline bounds
// this one round trip only; subscription streams never take this path.
func (c *Client) callWithMeta(ctx context.Context, method, name string, params, metaOverride json.RawMessage) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	id, resp, err := c.doRequest(ctx, method, name, params, metaOverride)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return c.readResult(method, id, resp)
}

// doRequest builds and sends one stateless request. metaOverride, when
// non-nil, replaces the client's default _meta triple for this request.
// The caller owns the response body. The returned id correlates the reply
// for stream reads.
func (c *Client) doRequest(ctx context.Context, method, name string, params, metaOverride json.RawMessage) (string, *http.Response, error) {
	if !IsJSONObject(params) {
		return "", nil, fmt.Errorf("params for %s must be a JSON object", method)
	}
	meta := c.meta
	if metaOverride != nil {
		meta = metaOverride
	}
	fullParams, err := withMeta(meta, params)
	if err != nil {
		return "", nil, err
	}
	id, err := newRequestID()
	if err != nil {
		return "", nil, err
	}
	idValue, err := jsonrpc.MakeID(id)
	if err != nil {
		return "", nil, fmt.Errorf("encode %s call id: %w", method, err)
	}
	reqMsg := &jsonrpc.Request{ID: idValue, Method: method, Params: fullParams}
	body, err := jsonrpc.EncodeMessage(reqMsg)
	if err != nil {
		return "", nil, fmt.Errorf("encode %s body: %w", method, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return "", nil, fmt.Errorf("build %s request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	token, err := c.tokens(ctx)
	if err != nil {
		// Lease contention is transient: the credential is valid and the
		// winning process is refreshing it. It must not surface as an
		// authentication rejection, which callers treat as terminal.
		if IsTokenContended(err) {
			return "", nil, newError(KindTransport, 0, err)
		}
		return "", nil, newError(KindAuth, 0, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if err := setStandardHeaders(req.Header, method, name); err != nil {
		return "", nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", nil, newError(KindTransport, 0, err)
	}
	return id, resp, nil
}

// withMeta merges the _meta triple into a params object without re-encoding
// any other field, so number literals and unknown fields survive byte for
// byte.
func withMeta(meta json.RawMessage, params json.RawMessage) (json.RawMessage, error) {
	if isMetaPresent(params) {
		return nil, fmt.Errorf("params already carry _meta")
	}
	trimmed := strings.TrimSpace(string(params))
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		return nil, fmt.Errorf("params must be a JSON object")
	}
	// Rebuild the object textually: _meta first, then the original members
	// verbatim. The original document is known-valid because IsJSONObject
	// accepted it.
	inner := trimmed[1 : len(trimmed)-1]
	var out strings.Builder
	out.WriteByte('{')
	out.WriteString(`"_meta":`)
	out.Write(meta)
	if strings.TrimSpace(inner) != "" {
		out.WriteByte(',')
		out.WriteString(inner)
	}
	out.WriteByte('}')
	merged := json.RawMessage(out.String())
	if !IsJSONObject(merged) {
		return nil, fmt.Errorf("merged params are not a JSON object")
	}
	return merged, nil
}

// isMetaPresent reports whether a params object already contains _meta.
func isMetaPresent(params json.RawMessage) bool {
	var v map[string]json.RawMessage
	if json.Unmarshal(params, &v) != nil {
		return false
	}
	_, ok := v["_meta"]
	return ok
}

// newRequestID returns a random opaque request identifier.
func newRequestID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate request id: %w", err)
	}
	return "link_" + hex.EncodeToString(b[:]), nil
}
