// Package oauth implements Tama Link's non-UI OAuth 2.1 client machinery:
// RFC 9728 protected-resource discovery, RFC 8414 authorization-server
// metadata, RFC 7591 dynamic client registration, authorization code with
// PKCE S256 and RFC 8707 resource binding, and refresh-token lifecycle.
//
// Design decisions (plan D4/D5/D13):
//
//   - Access tokens live in memory only. Refresh credentials and the client
//     registration record live in the profile-scoped secure keyring through
//     a SecretStore. Nothing secret reaches SQLite, logs, JSON output, or
//     error strings.
//   - Authorization is auth-code + PKCE S256 with an ephemeral 127.0.0.1
//     loopback redirect. This package builds the authorization request and
//     completes the code exchange; opening a browser and running the
//     listener remain Phase 3 (login command).
//   - Refresh is coordinated across processes through a profile-scoped
//     SQLite lease: claim, re-read the stored refresh credential, exchange,
//     and atomically retain any replacement refresh token. One
//     invalid_grant maps to ErrGrantInvalid and is never retried in a loop;
//     both sentinels map to the contract code authentication_required.
//
// Every network destination is validated: protected-resource and
// authorization-server metadata URLs are derived from the profile endpoint
// and expected issuer, and http is accepted only for loopback origins.
package oauth
