# Tama Link Login Plan

Status: Implemented; live acceptance pending
Updated: 2026-09-25
Target: Phase 3
Tracking: [kritama/tama-link#15](https://github.com/kritama/tama-link/issues/15)
Profile producer: [kritama/memovee-cli#3](https://github.com/kritama/memovee-cli/issues/3)

## Purpose

Implement the existing-profile path of `tama-link login` as the explicit
interactive OAuth entry point for a profile that already exists.

The command authorizes that profile, persists the durable OAuth material needed
by later `serve` processes, and exits. This path does not create or edit
profiles, start the MCP server, or make an upstream MCP request. Creating a
previously absent profile is the separate bootstrap path in
`plans/profile-bootstrap.md`.

This plan refines the Phase 3 login work described by
`wip/tama-link-specification.md` and
`wip/tama-link-specification/plan.md`. Those documents remain authoritative;
the contract changes identified here must be incorporated into them with the
implementation.

## Current State

Tama Link already has the non-interactive OAuth foundation:

- protected-resource and authorization-server discovery;
- dynamic client registration with durable client metadata;
- authorization-code requests with PKCE, state, resource binding, and an exact
  redirect URI;
- authorization-code completion and token exchange;
- keyring-backed refresh credentials and in-memory access tokens;
- credential fencing, refresh leases, invalid-grant retirement, logout, and
  credential-presence checks.

The login work is implemented per this plan:

- `cmd/tama-link/login.go` is a thin adapter over the `internal/login`
  service with the documented flags, output channels, and exit statuses;
- the durable `oauth/login` lease, bounded loopback callback, fixed platform
  browser openers, and manual handoff are implemented;
- profile schema version 2 declares the canonical scope set, which
  participates in the profile digest and binds discovery, registration,
  authorization, token, and refresh behavior.

The local Tama authorization servers require a non-empty supported scope and
advertise the RFC 9207 issuer response parameter. The App resource currently
uses `mcp.message`; the System resource exposes distinct read and review
scopes.

What remains is live acceptance: a distributable binary completing real
user-visible App and System logins against the local authorization servers,
followed by `submit` and `await` from a fresh `serve` process. That evidence
belongs to issue #8 and is not implied by the fixture, mock, repository-gate,
or cross-build results.

The upstream durable-task prerequisite tracked by `upmaru/tama#123` is
complete. Login remains Phase 3 work, while live App and System runtime
acceptance remains a separate acceptance gate for Tama Link issue #8.

## Ownership and Non-Goals

### Tama Link owns

- loading and validating an existing profile;
- OAuth metadata discovery and validation;
- dynamic client registration and reuse;
- PKCE, state, redirect, resource, issuer, and scope binding;
- opening the user's browser or presenting a manual authorization URL;
- the short-lived loopback callback listener;
- token exchange and fenced keyring persistence;
- stable command output, exit status, cleanup, and diagnostics.

### Other components own

- The Memovee CLI creates and reconciles profiles it manages. Tama Link's
  bootstrap flow may create a previously absent profile from a reviewed
  template; this existing-profile path does not.
- The authorization server owns user authentication, consent, authorization
  codes, and tokens.
- The user owns all interaction with the browser. Tama Link must never collect
  or automate entry of the user's credentials.

### Not part of login

- creating or editing profiles on this path; new-profile creation belongs to
  `plans/profile-bootstrap.md`;
- accepting tokens from flags, environment variables, or profile files;
- running a persistent HTTP callback server;
- starting `serve` or exposing the downstream MCP tools;
- calling an upstream MCP method to prove authorization;
- implementing `logout`, `doctor`, or progress rendering;
- changing the downstream MCP surface beyond `submit` and `await`;
- detecting provider behavior by client name or user agent.

## Contract Decisions

### 1. Command surface

The existing-profile command is:

```text
tama-link login --profile <name> [--config-dir <dir>] [--no-browser]
```

- `--profile` names an existing profile on this path.
- `--config-dir` follows the same resolution rules as `serve` and supports
  managed installations and isolated acceptance tests.
- `--no-browser` prints the authorization URL and waits for the callback. It is
  intended for environments where automatic browser launch is unavailable or
  undesirable.
- This path requires an existing, valid profile. It never creates or rewrites
  one. Bootstrap flags are rejected when the named profile already exists.

### 2. Profile scopes

Introduce profile schema version 2 with a required `scopes` array.

- The array must be non-empty.
- Values must be valid OAuth scope tokens, unique, bounded in number and
  length, and stored in canonical sorted order.
- The canonical scope set participates in the profile digest and therefore in
  profile identity and isolation.
- A profile requests only its declared scopes. Tama Link must never infer or
  silently request every scope advertised by a server.
- Existing version 1 profiles fail with an actionable migration error rather
  than receiving an implicit privilege set.

The application that creates the profile is responsible for selecting the
least-privileged scope set. For the current App endpoint that is
`mcp.message`. A System profile must explicitly list only the System operations
the caller requires.

### 3. Scope discovery and validation

Decode `scopes_supported` from both protected-resource and authorization-server
metadata.

Before registration or browser launch, the requested profile scopes must be a
subset of every authoritative scope list that is present. Missing advertised
scope metadata is handled according to the discovery specification; a present
list must never be ignored.

The canonical scope string is included in:

- dynamic client registration metadata when the server accepts it;
- the authorization request;
- the durable credential binding used to validate future refreshes.

If a token response omits `scope`, the grant inherits the requested scope set.
If it includes `scope`, the returned set must match the requested set. A
reduced, expanded, malformed, or otherwise different set fails closed so that
privilege changes cannot occur silently.

### 4. Redirect registration and callback URI

Dynamic registration uses the native-loopback redirect base plus the fixed
callback path:

```text
http://127.0.0.1/oauth/callback
```

The per-attempt port is not registered because it varies; providers that
compare redirect paths must see the fixed path every authorization uses.

Each login attempt binds an IPv4 listener to `127.0.0.1:0` before constructing
the authorization request. The exact selected callback URI is:

```text
http://127.0.0.1:<port>/oauth/callback
```

That exact URI is used in both the authorization request and token exchange.
The different ephemeral port is allowed by the native-app loopback redirect
contract and must be covered by provider acceptance tests.

The listener must not bind a wildcard address, a non-loopback interface, an
IPv6 fallback, or a user-configurable callback path.

### 5. Authorization request

Every authorization request contains and binds:

- `response_type=code`;
- the registered client identifier;
- the exact per-attempt redirect URI;
- a cryptographically random state value;
- a cryptographically random PKCE verifier and S256 challenge;
- the exact profile resource URL;
- the canonical profile scope string.

State, verifier, code, authorization URL, and token material are ephemeral
secrets. They must not be written to logs, SQLite, profile files, diagnostic
snapshots, or ordinary status output.

### 6. Browser handoff

The default path invokes a fixed operating-system browser opener without a
shell or a user-controlled executable name. Platform adapters may use the
native Windows URL handler, `open` on macOS, and `xdg-open` on Linux.

Browser launch failure does not invalidate the OAuth attempt. Tama Link prints
the authorization URL as an intentional manual handoff and continues waiting
for the callback. `--no-browser` selects that behavior directly.

The manual URL is written only for the requesting user; it must not also be
logged or included in later diagnostics because it contains attempt-bound
state.

### 7. Callback validation

The callback server is single-purpose and short-lived:

- accept only `GET` on the fixed callback path;
- reject unexpected hosts, paths, methods, duplicate security parameters, and
  oversized headers or query strings;
- compare state in constant time;
- require and validate the authorization response issuer when discovery says
  the issuer response parameter is supported;
- accept exactly one terminal response containing either a code or a sanitized
  OAuth error;
- stop accepting callbacks as soon as a terminal response is selected;
- use a fixed five-minute deadline and honor command-context cancellation;
- close the listener on success, failure, timeout, cancellation, or panic.

The browser response is fixed local HTML with no reflected query values. It
uses `Cache-Control: no-store`, `Referrer-Policy: no-referrer`,
`X-Content-Type-Options: nosniff`, and a restrictive content security policy
such as `default-src 'none'; style-src 'unsafe-inline'`.

### 8. Durable concurrency and fencing

Use a profile-scoped durable lease named `oauth/login` to serialize interactive
login attempts across processes.

- Lease acquisition occurs before creating the listener or authorization
  attempt.
- The lease is renewed during the bounded browser wait.
- A competing login fails promptly with a clear busy diagnostic.
- Expiry permits recovery after a crashed login process.
- The lease and listener are released on every exit path.

Do not hold the existing refresh lease across browser interaction. The login
lease owns the interactive attempt; the existing refresh lease and credential
fence still protect the final credential mutation. A completion that has lost
its fence must fail without overwriting newer credentials.

An unsuccessful re-login must preserve any still-valid prior registration and
refresh credential. A replacement registration may retire an old credential
only under the existing rule that the old registration is already unusable.

### 9. Credential result

On successful exchange:

- the dynamic client record and refresh credential are committed through the
  existing fenced keyring path;
- granted scopes are durably bound to the refresh credential;
- the access token remains memory-only and dies with the login process;
- the command verifies the durable credential presence after commit and exits;
- the next `serve` process obtains an access token through the normal refresh
  path.

Login does not perform an MCP request as a hidden verification step. Runtime
protocol acceptance is a separate, observable test.

### 10. Output and exit status

Ordinary progress and errors go to standard error. Standard output is reserved
for the manual authorization URL when `--no-browser` is selected or automatic
browser launch fails.

Exit status is stable:

- `0`: durable login state was committed and verified;
- `1`: discovery, keyring, registration, browser/callback, authorization,
  timeout, token, or persistence failure;
- `2`: invalid command usage, missing profile, or invalid profile contract.

No output may contain authorization codes, PKCE verifiers, access tokens,
refresh tokens, client secrets, keyring payloads, or unredacted upstream
responses.

## Execution Flow

The login service performs these steps in order:

1. Parse flags and resolve the configuration directory.
2. Load the named profile and validate its endpoint, resource, adapter, and
   canonical scope set.
3. Open the profile-isolated SQLite state and secure keyring namespace.
4. Acquire the durable `oauth/login` lease.
5. Construct the OAuth client with the profile resource and registration
   redirect base.
6. Discover and validate protected-resource and authorization-server metadata,
   including issuer, endpoints, PKCE support, resource binding, and scopes.
7. Reuse or dynamically register the fenced client record.
8. Bind the exact IPv4 loopback listener and construct the authorization
   request with its selected redirect URI.
9. Open the authorization URL or present it for manual handoff.
10. Wait for one validated callback within the fixed deadline.
11. Exchange the code using the original redirect URI and PKCE verifier, then
    commit the fenced refresh credential and scope binding.
12. Verify durable credential presence, close all ephemeral resources, release
    the login lease, and report success.

Every failure path closes the listener, stops lease renewal, releases or lets
the bounded lease expire safely, and returns a sanitized operation-specific
error.

## Package Boundaries

Keep the CLI and protocol packages focused:

- `cmd/tama-link/login.go` owns flags, stable presentation, and exit mapping.
  It delegates the flow after validating command syntax.
- `internal/login/service.go` owns the application-level login sequence and
  consumes small interfaces for OAuth, state, callback, browser, clock, and
  randomness.
- `internal/login/callback.go` owns the bounded loopback listener and callback
  validation.
- platform-specific files under `internal/login` own fixed browser-opening
  commands without shell evaluation.
- `internal/oauth` gains scope metadata, request, response, and refresh
  validation. It remains independent of CLI output, browser behavior, and HTTP
  callback presentation.
- `internal/profile` owns schema version 2, scope validation, canonicalization,
  and digest participation.
- `internal/store` continues to own generic durable leases; login supplies the
  `oauth/login` lease name rather than adding an in-memory lock.

Shared runtime construction may be extracted from `serve` only when it removes
real duplication. It must retain the existing package boundaries and must not
become a catch-all manager.

## Failure Behavior

The implementation must distinguish these user-visible classes without
exposing sensitive payloads:

- missing, outdated, or invalid profile;
- unavailable secure keyring;
- discovery metadata mismatch or unsupported requested scope;
- unusable registration or dynamic-registration failure;
- another login already in progress;
- browser launch failure with manual fallback;
- user denial or authorization-server callback error;
- callback state, issuer, host, path, method, or size failure;
- callback timeout or command cancellation;
- token exchange or returned-scope failure;
- lost lease or credential fence;
- durable commit or post-commit verification failure.

Errors are wrapped with operation context internally and translated to stable,
sanitized CLI diagnostics only at the command boundary.

## Test Plan

### Profile and discovery

- profile version 2 parsing, scope validation, sorting, deduplication, bounds,
  digest changes, and version 1 migration errors;
- protected-resource and authorization-server scope decoding;
- requested-scope subset validation against either or both metadata documents;
- explicit rejection of missing, malformed, unsupported, or empty scopes.

### OAuth request and token binding

- canonical `scope`, exact `resource`, state, S256 challenge, client ID, and
  exact callback URI in authorization requests;
- base loopback redirect registration with per-attempt ephemeral ports;
- token response scope omission, exact equality, reduction, expansion,
  malformed values, and durable refresh-scope binding;
- preservation of prior usable credentials on denial and failed re-login;
- stale completion rejection through lease and credential fences.

### Callback and browser

- IPv4 loopback-only binding and cleanup on every path;
- accepted callback and rejection of wrong method, host, path, state, issuer,
  duplicate values, oversized input, and mixed code/error responses;
- timeout, cancellation, listener-close, and concurrent callback races;
- fixed security headers and absence of reflected query data;
- browser opener selection, argument passing without a shell, launch failure,
  and `--no-browser` manual handoff.

### Orchestration and persistence

- end-to-end `httptest` authorization flow with SQLite and an isolated fake
  keyring;
- durable login-lease contention, renewal, release, and crash recovery across
  independent store handles;
- token exchange and credential commit only after a valid callback;
- command flags, exit statuses, stdout/stderr separation, and sanitized errors;
- assertions that secrets never appear in SQLite, profile files, logs, command
  output, errors, or test snapshots;
- race-enabled tests for callback selection, lease renewal, cancellation, and
  credential replacement.

### Repository gates

- focused package tests during implementation;
- `go test -race ./...`;
- `make check`;
- supported cross-builds for the distributable binary.

### Live acceptance

Live acceptance is performed separately for both local resources with clean
OAuth state:

1. Create the correct App or System profile with its explicit least-privileged
   scopes.
2. Run the distributable `tama-link login` binary.
3. Let the user authenticate and consent in the browser; automation must not
   enter credentials.
4. Confirm login exits successfully with only durable keyring-backed OAuth
   state.
5. Start a fresh `tama-link serve` process from the same profile.
6. Exercise the real downstream `submit` and `await` tools against the live
   upstream resource.
7. Restart Tama Link and confirm the durable workflow remains usable.

Fixture servers, mocked OAuth, Compose configuration, and static source review
do not satisfy this live gate. The App and System results should be captured as
separate evidence for issue #8.

## Delivery Phases

### Phase 1: Contract alignment

- Add profile version 2 and its scope contract to both authoritative
  specification documents.
- Reconcile stale Phase 2 references now that `upmaru/tama#123` is complete.
- Update the associated GitHub tracker so the WIP and live issue describe the
  same remaining work.

Gate: the schema migration, exact command surface, security invariants, and
acceptance boundary are reviewable before implementation begins.

### Phase 2: Scope and OAuth primitives

- Implement profile scope parsing and digest binding.
- Decode advertised scopes and validate the requested subset.
- Send canonical scopes in authorization requests and bind token/refresh
  results to them.
- Add focused metadata and OAuth tests.

Gate: the existing non-UI OAuth layer can complete a scope-correct flow without
browser or listener code.

### Phase 3: Interactive login service

- Add the durable login lease.
- Implement the bounded loopback callback component.
- Implement fixed platform browser openers and manual fallback.
- Compose the pieces in the login application service with injected test
  boundaries.

Gate: deterministic integration tests cover success, denial, timeout,
concurrency, cancellation, fencing, and secret hygiene.

### Phase 4: CLI and distributable workflow

- Replace the login stub with the thin command adapter.
- Document profile creation, login, manual login, serve, and troubleshooting.
- Verify command output, exit statuses, cross-builds, and the complete
  repository gate.

Gate: a locally built distributable binary supports the documented workflow
without development-only setup.

### Phase 5: Live acceptance

- Run clean App and System logins against the local Tama authorization servers.
- Exercise `submit` and `await` through fresh and restarted server processes.
- Record exact evidence and limitations against issue #8.

Gate: both resource profiles pass the real browser, keyring, refresh, MCP, and
restart workflow. Only this gate closes the remaining live-acceptance claim.

## Acceptance Criteria

Login is complete when all of the following are true:

1. Existing-profile `login` requires a version 2 profile with explicit scopes.
   Creating a profile is covered by `plans/profile-bootstrap.md`, not this path.
2. Requested scopes are canonical, least-privileged, sent in the authorization
   request, and validated against discovery and token responses.
3. The callback uses a pre-bound exact IPv4 loopback URI and a fixed path.
4. State, issuer, PKCE, resource, redirect, and scope are validated end to end.
5. Browser launch uses fixed platform behavior without a shell, and manual
   handoff works when requested or necessary.
6. The callback listener is bounded, single-use, securely rendered, and always
   closed.
7. Concurrent login attempts are serialized across processes with a durable,
   recoverable lease.
8. Lost leases and stale credential fences cannot overwrite newer state.
9. Refresh credentials and client secrets exist only in the secure keyring;
   access tokens remain memory-only.
10. Failed login does not destroy still-usable prior credentials.
11. Output and errors contain no OAuth secrets or unredacted sensitive data.
12. The command has stable flags, output channels, exit statuses, and actionable
    sanitized errors.
13. Unit, integration, race, repository, and cross-build gates pass.
14. The distributable binary completes live App and System login followed by
    real `submit` and `await` calls from a fresh `serve` process.
15. Restart acceptance proves that the keyring-backed credential can restore
    operation without rerunning login.

## Deferred Follow-Ups

`logout`, `doctor`, and progress rendering remain separate Phase 3 commands.
They may reuse the focused profile/runtime construction introduced for login,
but they must not be folded into the login service or used to broaden this
delivery.
