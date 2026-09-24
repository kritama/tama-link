// Package acceptance records the gates that are not package-fixture results.
//
// Fixture tests live in internal/upstream/conformance. Mocked integration
// tests live beside the application and worker packages. Compose pin checks
// and live runtime acceptance are separate commands and must not be inferred
// from either of those.
package acceptance
