package upstream

import (
	"encoding/base64"
	"fmt"
	"net/http"
)

// Pinned MCP protocol version. The adapter is 2026-07-28 only; any other
// value is a protocol mismatch, never a downgrade.
const protocolVersion = "2026-07-28"

// MCP standard HTTP headers for 2026-07-28 streamable HTTP.
const (
	headerProtocolVersion = "MCP-Protocol-Version"
	headerMethod          = "Mcp-Method"
	headerName            = "Mcp-Name"
)

// setStandardHeaders writes the protocol and method headers for one
// outgoing request. name is empty when the method carries no Mcp-Name.
func setStandardHeaders(h http.Header, method, name string) error {
	h.Set(headerProtocolVersion, protocolVersion)
	h.Set(headerMethod, method)
	if name != "" {
		encoded, err := encodeHeaderValue(name)
		if err != nil {
			return err
		}
		h.Set(headerName, encoded)
	}
	return nil
}

// base64 sentinel wrappers, per the pinned header encoding rules.
const (
	base64Prefix = "=?base64?"
	base64Suffix = "?="
)

// encodeHeaderValue encodes a standard header value, wrapping it in the
// Base64 sentinel when it contains characters outside the printable
// one-byte range or leading or trailing whitespace.
func encodeHeaderValue(value string) (string, error) {
	if !requiresBase64(value) {
		return value, nil
	}
	for _, r := range value {
		if r > 0x10FFFF {
			return "", fmt.Errorf("header value contains an invalid code point")
		}
	}
	return base64Prefix + base64.StdEncoding.EncodeToString([]byte(value)) + base64Suffix, nil
}

// requiresBase64 reports whether the value must use the sentinel encoding.
func requiresBase64(s string) bool {
	if len(s) == 0 {
		return false
	}
	if s[0] == ' ' || s[0] == '\t' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t' {
		return true
	}
	for _, c := range s {
		if c < 0x20 || c > 0x7E {
			return true
		}
	}
	return false
}
