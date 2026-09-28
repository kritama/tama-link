// Package bootstrap creates one new Tama Link profile through an explicit
// interactive or fully specified login. It does not edit an existing profile
// and does not replace the existing-profile authorization path.
//
// The package owns the application sequence, the non-secret journal, and the
// resume or discard decision. Prompting, metadata discovery, reviewed
// templates, profile publication, and the durable runtime are supplied
// through small interfaces so tests do not need a terminal or a live server.
package bootstrap
