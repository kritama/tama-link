// Package secretservice opens the Linux Secret Service collection behind the
// standard default alias and migrates credentials stranded in older Tama Link
// collections.
//
// The adapter deliberately cannot create a collection. Profile isolation stays
// at the item key, so macOS Keychain and Windows Credential Manager are
// untouched and a missing or locked desktop keyring fails closed.
package secretservice
