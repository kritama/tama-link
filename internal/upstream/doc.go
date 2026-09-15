// Package upstream implements the stateless MCP 2026-07-28 streamable-HTTP
// client that every TamaMCP adapter uses.
//
// The client is single-purpose: it speaks the pinned TamaMCP wire contract
// (specification commit 6b5db00018d2774834db5a0f00eed5b9b55e1d2e) to a
// profile-selected upstream endpoint. It never sends initialize,
// notifications/initialized, or Mcp-Session-Id, never emits client-requested
// task augmentation, and has no fallback to a legacy handshake.
//
// # go-sdk audit
//
// The upstream client is built on the official MCP Go SDK
// github.com/modelcontextprotocol/go-sdk v1.7.0 for its public wire
// conventions, but not on its ClientSession connection, because the pinned
// release has these exact missing public seams for a stateless 2026-07-28
// upstream:
//
//  1. mcp.Client.Connect unconditionally falls back to the legacy
//     initialize/notifications/initialized handshake when server/discover
//     fails (mcp/client.go, Connect). There is no public opt-out, and TamaMCP
//     rejects the legacy methods.
//  2. The Mcp-Name header is derived only for tools/call, prompts/get, and
//     resources/read (mcp/streamable_headers.go, extractName). The task
//     methods require Mcp-Name to equal params.taskId, which the SDK cannot
//     express for custom methods.
//  3. mcp.NotificationSubscriptions has no task-ID field, so a task-scoped
//     subscriptions/listen request cannot be expressed.
//  4. The SDK exposes no Tasks extension types: the detailed tasks/get state,
//     tasks/update parameters, and notifications/tasks payloads.
//  5. SDK typed results decode schemas and structured content into any,
//     which renders JSON numbers as float64 and projects extension fields
//     through Go unions. Tama Link must retain canonical number literals and
//     validated raw JSON for catalog verification and terminal results.
//
// This package therefore reuses the SDK's public jsonrpc envelope package
// (documented for transport authors), the mcp.Implementation type, and the
// MetaKey protocol metadata constants, while carrying its own lossless wire
// types. It is not a general MCP client and must not grow one: new capability
// goes through the adapter boundary, not here.
package upstream
