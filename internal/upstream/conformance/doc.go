// Package conformance adapts the package-provided TamaMCP contract fixtures
// to the Tama Link upstream client.
//
// The fixtures stay on this side of the adapter. They are not copied into the
// downstream submit/await contract. A fixture request is not replayed byte
// for byte: Link generates request IDs, identifies itself as tama-link, and
// authenticates with Authorization rather than the package test header. The
// client must still satisfy the fixture's wire invariants, accept the pinned
// response, and refuse legacy methods.
//
// This package is the fixture gate. It is not live Tama acceptance.
package conformance
