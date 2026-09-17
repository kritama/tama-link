// Package tama2026 is the TamaMCP 2026-07-28 adapter boundary: it
// authenticates through the profile OAuth client, performs the stateless
// server/discover handshake, reads the complete live tools/list result, and
// verifies the pinned profile catalog before any operation may execute.
//
// The effective catalog is always the live catalog intersected with the
// pinned approved descriptors: a live tool that is not pinned is never
// exposed, and a pinned operation with security-relevant drift fails closed
// with a stable boundary error. The adapter never sends initialize, never
// holds a protocol session, and never leaks upstream-specific payloads past
// its typed boundary.
//
// Task support is declared per request, never inferred from a missing wire
// field: an operation pinned as upstream_task requires the discovered Tasks
// extension and receives the Tasks capability in its tools/call _meta, and
// the expected resultType is enforced on the response.
package tama2026
