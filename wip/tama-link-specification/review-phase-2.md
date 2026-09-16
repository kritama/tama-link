# Phase 2 Implementation Review — Issues #2–#4

Status: changes required; issues #2–#4 are not ready to close

Review date: 2026-09-16

Reviewed branch: `feature/phase-2-current-tama-adapters`

Reviewed local head: `c52825c420022f9bf5e160399ed3924cc28672cf`

Reviewed commits:

- `6466ecc` — stateless MCP `2026-07-28` upstream transport;
- `ccdb2f0` — OAuth discovery, PKCE, and coordinated refresh; and
- `c52825c` — TamaMCP 2026 adapter with verified pinned catalog.

The reviewed local branch was three commits ahead of
`origin/feature/phase-2-current-tama-adapters`. This review therefore applies
to the local commits above, whether or not they have subsequently been pushed.

Tracking issues:

- [#2 — MCP 2026 upstream transport and Tasks client](https://github.com/kritama/tama-link/issues/2)
- [#3 — OAuth discovery, PKCE, and coordinated refresh](https://github.com/kritama/tama-link/issues/3)
- [#4 — TamaMCP discovery and pinned catalog](https://github.com/kritama/tama-link/issues/4)
- [#9 — Phase 2 tracker](https://github.com/kritama/tama-link/issues/9)

Normative upstream baseline: TamaMCP specification commit
[`6b5db00018d2774834db5a0f00eed5b9b55e1d2e`](https://github.com/kritama/tama-mcp/blob/6b5db00018d2774834db5a0f00eed5b9b55e1d2e/wip/tama-mcp-specification.md).

## Decision

The implementation is directionally sound. Its package boundaries are
cohesive, the upstream client is correctly designed around stateless MCP
`2026-07-28`, and the SDK audit gives a concrete justification for the focused
wire extension. The OAuth, transport, and adapter packages have substantial
behavioral fixture coverage, and the repository gate is green.

Issues #2–#4 must nevertheless remain open. Six priority-1 defects affect the
actual Tama OAuth path, redirect safety, bounded stream behavior, owner-bound
notification correlation, and issuer isolation. Three priority-2 contract gaps
cover malformed upstream state, stale profile compatibility, and the default
client's one-minute limit on nominally long-lived subscriptions.

No implementation code changes were made during this review.

## What is sound

- `internal/upstream` has no initialization fallback, protocol session, or
  `Mcp-Session-Id` path. It emits the required protocol, method, conditional
  name, and per-request metadata fields.
- The SDK-versus-extension ownership decision is recorded in
  `internal/upstream/doc.go` with five concrete gaps in `go-sdk v1.7.0`.
- JSON-RPC results and catalog schemas are retained as raw JSON, preserving
  number literals and unknown extension fields.
- The task client implements `tasks/get`, `tasks/update`, and `tasks/cancel`
  without introducing `tasks/result`, `tasks/list`, or client-requested task
  augmentation.
- The subscription state machine enforces acknowledgement-before-notification
  ordering and subscription-ID correlation on the covered paths.
- `internal/oauth` separates metadata discovery, registration, authorization,
  token storage, and refresh coordination. Access tokens remain memory-only;
  client registrations and refresh credentials use the profile keyring.
- Refresh acquisition is lease-bounded and re-reads the durable refresh
  credential after claiming the lease, allowing replacement-token adoption.
- `internal/adapter/tama2026` intersects live tools with the pinned allowlist,
  rejects missing and drifted approved tools, ignores unpinned live tools, and
  enforces the expected `tools/call` result shape for the tested task policies.

## Priority 1 findings

### P1. Public-client token requests omit `client_id`

Location: `internal/oauth/register.go`, `postToken`, lines 164–174.

`postToken` constructs the HTTP request body from `form.Encode()` before
`ClientRecord.applyAuth` mutates the form. For `none`, `applyAuth` adds
`client_id`; for `client_secret_post`, it adds both `client_id` and
`client_secret`. Neither mutation reaches the already-created request body.

This blocks the intended Tama integration. Tama's dynamic client registration
currently returns `token_endpoint_auth_methods_supported: ["none"]`, so both
the authorization-code and refresh exchanges will omit the required client ID.
The current OAuth fixtures use `client_secret_basic`, whose credentials live in
the header, and therefore do not expose this failure.

Required change:

- apply form-based authentication before encoding the request body;
- apply Basic authentication after constructing the request, or split the form
  and header responsibilities explicitly; and
- add authorization-code and refresh fixtures for both `none` and
  `client_secret_post`, asserting the exact encoded form.

### P1. The authorization and token exchanges use different redirect URIs

Location: `internal/oauth/authcode.go`, `CompleteAuthorization`, lines 62–75.

`NewAuthorizationRequest` puts the configured `c.redirectURI` into the
authorization request. `CompleteAuthorization` then accepts a different
`observedRedirectURI` and sends that value to the token endpoint. The default
configuration uses the portless `http://127.0.0.1`, while its test exchanges
the code with a URI containing an ephemeral port.

An OAuth authorization-code exchange requires the token request's
`redirect_uri` to equal the value used in the authorization request. The
ephemeral listener port must therefore be selected before the authorization URL
is constructed.

Required change:

- bind the loopback listener and obtain its actual port before creating the
  authorization request;
- place that exact URI in `AuthorizationRequest.RedirectURI` and the
  authorization URL;
- require the callback and token exchange to use that exact URI; and
- add a negative fixture proving a mismatched port or path is rejected before
  a token request is sent.

### P1. Default HTTP clients follow redirects

Locations:

- `internal/oauth/client.go`, `New`, lines 132–140; and
- `internal/upstream/client.go`, `New`, lines 86–97.

Both constructors install a rejecting `CheckRedirect` callback only when the
caller supplied an HTTP client. The default clients leave `CheckRedirect` nil
and therefore use Go's redirect-following behavior. OAuth metadata,
registration, token, and authenticated MCP requests may leave their validated
destination despite both packages documenting that redirects are rejected.

The existing upstream redirect test checks only that the complete discovery
call eventually fails. Its redirected target returns a response with a
mismatched JSON-RPC ID, so the test passes even though the redirect was
followed.

Required change:

- install the refusing callback on both default and cloned clients; and
- make redirect tests record the target request count and assert it remains
  zero, including `307`/`308` cases for authenticated POST requests.

### P1. Task notifications are not bound to the acknowledged task-ID set

Location: `internal/upstream/subscribe.go`, `readSubscription`, lines 84–120.

The subscription reader records only an `acknowledged` boolean. It does not
verify that acknowledged IDs are a subset of the requested IDs, retain the
authorized set, or reject a later `notifications/tasks` snapshot whose task ID
is outside that set. A matching subscription ID is therefore sufficient to
deliver an unrequested task state to the callback.

The same state machine also accepts a matching final JSON-RPC response before
the required acknowledgement and treats a matching JSON-RPC error response as
a graceful successful close.

Required change:

- pass the requested task IDs into the stream reader;
- reject duplicate request IDs and acknowledgements containing IDs outside the
  requested set;
- retain the acknowledged set and reject every task notification outside it;
- require acknowledgement before a final response; and
- classify a matching final JSON-RPC error rather than returning nil.

### P1. The complete SSE event is not bounded

Location: `internal/upstream/response.go`, `scanSSE`, lines 160–199.

The scanner limits an individual line to `maxBytes`, but `data` can accumulate
an unlimited number of individually valid `data:` lines before a blank event
delimiter. `strings.Join` then allocates the complete unbounded payload. The
documented per-event bound therefore does not hold and a peer can drive
unbounded memory growth.

Required change:

- track cumulative encoded event bytes, including inserted newline bytes;
- return `KindTooLarge` immediately when the event exceeds `maxBytes`;
- handle or explicitly reject a final unterminated event at EOF according to
  the selected SSE parsing contract; and
- add multi-line boundary tests immediately below, exactly at, and above the
  configured event limit.

### P1. Refresh credentials are not bound to the active profile issuer

Location: `internal/oauth/refresh.go`, `refresh`, lines 56–71.

Refresh verifies only that the stored client record and stored refresh
credential name the same issuer. It never requires either value to equal the
client's configured `c.issuer`. If a profile changes issuer while retaining its
credential namespace, Tama Link will continue sending the old refresh token to
the old stored token endpoint.

This contradicts the package's stated fail-closed issuer binding and weakens
profile isolation.

Required change:

- require `rec.Issuer == c.issuer` and `cred.Issuer == c.issuer` before using
  either stored value;
- validate the stored token endpoint against the issuer-bound metadata policy
  before use; and
- add a fixture where the two stored issuers agree with each other but differ
  from the current profile issuer.

## Priority 2 findings

### P2. Detailed task-state responses are only partially validated

Location: `internal/upstream/tasks.go`, `decodeTaskState` and
`checkStatePayloads`, lines 117–186.

Payload presence is defined as `len(raw) > 0`. JSON `null` therefore satisfies
the required `result`, `error`, or `inputRequests` check. A `tasks/get` response
also passes without `resultType: "complete"`, and the decoder does not validate
the nested terminal `CallToolResult`, failure object, input-request map,
timestamps, TTL, or polling interval against the pinned schemas and bounds.

Related acknowledgement paths return the raw `tasks/update` and
`tasks/cancel` result without requiring their documented complete-result shape.
The initial task result from `tools/call` also accepts an unknown task status.

Required change:

- validate the complete state-specific Tasks result with the pinned schema or
  an equivalent focused validator before returning `TaskState`;
- distinguish an absent member from an explicit JSON `null` value;
- require the method-specific `resultType` and acknowledgement shape; and
- add malformed fixtures for null payloads, missing required common fields,
  invalid safe integers/timestamps, invalid nested `CallToolResult`, and an
  invalid initial task status.

### P2. Discovery does not enforce profile bounds or capability object shapes

Location: `internal/adapter/tama2026/discover.go`, `verifyDiscovery` and
`decodeCapabilities`, lines 30–59.

Discovery confirms that the server supports `2026-07-28`, but it does not
confirm that this version falls within `profile.Bounds`. A structurally valid
profile pinned exclusively to an older or future revision can therefore create
a connection.

Capability validation uses only raw-message length. A declaration such as
`"tools": null` is accepted because the raw value is non-empty, and
`DiscoverResult.HasTaskExtension` treats a null Tasks extension value as
present. These are malformed capability declarations rather than supported
empty capability objects.

Required change:

- require the adapter protocol to fall inclusively within the profile's
  compatibility bounds;
- require `capabilities`, `tools`, `extensions`, and the Tasks extension value
  to have their specified JSON object shapes; and
- add stale-profile, future-profile, null, scalar, and array capability
  fixtures.

### P2. The default finite-call timeout terminates every subscription

Location: `internal/upstream/client.go`, `New`, lines 86–90.

The default `http.Client.Timeout` is 60 seconds. In Go this timeout covers the
complete response body, so it also applies to `subscriptions/listen` and
forcibly terminates every long-lived stream after one minute. Reconciliation
through `tasks/get` preserves correctness, but the client cannot honor the
server's longer credential or configured stream lifetime and will create
unnecessary reconnect churn.

Required change:

- use per-request contexts or a finite-call transport wrapper for ordinary
  requests;
- leave the subscription body controlled by its caller context, credential
  expiry, and stream-lifetime owner; and
- add a deterministic test proving a stream may remain open beyond the finite
  request timeout while still closing promptly on cancellation.

## Validation evidence

A full local `make check` completed successfully after running outside the
network-restricted sandbox so the HTTP fixtures could bind loopback ports. It
passed:

- `go test ./...`;
- `go test -race ./...`;
- `go vet ./...`;
- `golangci-lint run ./...` with zero issues; and
- the trimmed host binary build.

`git diff --check a1e21e9..c52825c` also passed, and the reviewed worktree was
clean. The findings above are contract and coverage gaps; they are not failures
currently detected by the repository gate.

## Re-review requirements

Before issues #2–#4 are closed:

1. Address every priority-1 finding and add a regression test that fails on the
   reviewed implementation.
2. Address the priority-2 task, discovery, capability, and subscription-timeout
   findings or explicitly revise the authoritative Phase 2 contract before
   requesting re-review.
3. Run `make check` and `git diff --check` on the exact proposed head.
4. Verify the real Tama dynamic-client `none` authorization-code and refresh
   flow, not only a `client_secret_basic` fixture.
5. Run the applicable TamaMCP conformance fixtures for malformed task states,
   subscription authorization, acknowledgement ordering, stream bounds, and
   graceful/error closure.
6. Request review against the exact commit containing the remediations and
   report whether it has been pushed to the tracked remote branch.

## Remediation report — 2026-09-16

Status (updated after the Phase 2.1 re-review): the original findings were
addressed as described below, but the re-review in
`review-phase-2-1.md` identified three residual defects (an SSE stop-signal
regression introduced by the SSE-bound rewrite, an incomplete task-envelope
validation, and a supplied-client timeout that still bounded subscriptions).
Those are addressed in the Phase 2.1 remediation report in that document; see
its status for the current position.

### Priority 1

- **P1 `client_id` omitted from public-client token requests** — `postToken`
  now splits the auth method: `applyFormAuth` runs before `form.Encode()`
  (`none` adds `client_id`, `client_secret_post` adds both credentials) and
  `applyHeaderAuth` applies Basic credentials after request construction.
  Regression: `TestTokenExchangePublicClientNone` and
  `TestTokenExchangeClientSecretPost` assert the exact encoded form for both
  the authorization-code and refresh exchanges and assert no Authorization
  header for `none`/`client_secret_post`. Verified the `none` fixture fails
  against the reviewed implementation.
- **P1 authorization and token exchanges used different redirect URIs** —
  `NewAuthorizationRequest` now takes the exact loopback redirect URI
  (validated as an http 127.0.0.1 URI) and carries it in the authorization
  URL and `AuthorizationRequest.RedirectURI`; `CompleteAuthorization` rejects
  an observed URI that differs from it before any token request is sent.
  Regression: mismatched-port and mismatched-path fixtures assert the error
  and zero token-endpoint calls. The spec and plan D5 now state the
  exact-URI binding.
- **P1 default HTTP clients followed redirects** — both `oauth.New` and
  `upstream.New` now refuse redirects on every client, default or supplied
  (supplied clients are cloned, so caller state is never mutated).
  Regression: redirect tests count requests received by the redirect target
  and assert zero, for 302/307/308, including authenticated `tools/call`
  POSTs.
- **P1 task notifications not bound to the acknowledged task-ID set** —
  `readSubscription` now receives the requested ID set; the acknowledgement
  must authorize a non-duplicate subset of it and is retained as the
  authorized set; every `notifications/tasks` snapshot must carry an
  authorized ID; the final response must follow the acknowledgement; a
  matching final JSON-RPC error is classified as a protocol failure.
  Regression: fixtures for unrequested-ID authorization, repeated IDs in the
  acknowledgement, snapshots outside the acknowledged set, a final response
  before the acknowledgement, and a final error.
- **P1 the complete SSE event was not bounded** — `scanSSE` now counts every
  `data` line's value plus its join separator against `maxBytes` and returns
  `KindTooLarge` before the event completes; the scanner line bound is the
  event bound plus the field prefix. An event whose blank-line delimiter
  never arrives at EOF is not dispatched (WHATWG implied line feed), which
  the caller treats as a clean close and reconciles. Regression: fixtures
  immediately below, exactly at, and above the limit for a single line; a
  multi-line accumulation of individually valid lines past the limit; and the
  unterminated final event.
- **P1 refresh credentials not bound to the active profile issuer** —
  refresh now requires the stored client record and the stored refresh
  credential to each equal the client's configured issuer, and re-validates
  the stored token endpoint (secure absolute URL on the issuer's origin)
  before use. Regression: a fixture where both stored values agree with each
  other but differ from the profile issuer, plus stored-endpoint policy
  fixtures; all assert zero token-endpoint calls.

### Priority 2

- **P2 detailed task states only partially validated** — `decodeTaskState`
  now distinguishes an absent member from an explicit JSON `null`, requires
  `resultType: "complete"` on `tasks/get` and its absence on notification
  payloads, validates timestamps as RFC3339, rejects negative TTLs and
  unsafe integer literals, and validates the state-specific payloads: a
  completed state must carry a `CallToolResult` object (complete resultType,
  content array, boolean isError), a failed state an error object, an
  input_required state an input-request object. `tasks/update` and
  `tasks/cancel` acknowledgements must be complete-result objects, and a
  `tools/call` task result must carry a known initial status. Regression:
  `TestTaskGetMalformedFixtures`, `TestTaskUpdateAckShape`,
  `TestCallToolRejectsUnknownInitialTaskStatus`, and notification fixtures
  for a resultType envelope and a null payload.
- **P2 discovery did not enforce profile bounds or capability shapes** —
  `verifyDiscovery` (and `Adapter.New`, for fail-fast wiring) require
  `2026-07-28` to fall inclusively within `profile.Bounds` (new
  `Bounds.Contains`); `decodeCapabilities` requires `capabilities`, `tools`,
  `extensions`, and every extension value to be JSON objects;
  `DiscoverResult.HasTaskExtension` treats a null extension value as absent.
  This also fixed `upstream.IsJSONObject`, which accepted JSON `null` because
  `json.Unmarshal` succeeds for it. Regression: stale-profile,
  future-profile, null-tools, scalar-tools, array-extensions, and
  null-Tasks-extension fixtures.
- **P2 the default finite-call timeout terminated every subscription** —
  the upstream client no longer carries an overall HTTP timeout. Finite
  requests (`server/discover`, `tools/list`, `tools/call`, `tasks/*`) get a
  per-request context deadline (`RequestTimeout`, default
  `DefaultRequestTimeout` = 60s); `subscriptions/listen` streams are bounded
  only by the caller context, credential expiry, and the stream-lifetime
  owner. Regression: `TestFiniteCallHonorsRequestTimeout` and
  `TestSubscriptionNotBoundByRequestTimeout` (stream holds open beyond the
  50ms request timeout, then closes promptly on cancellation).

### Validation

- `make check` green on the remediation head: `go test ./...`,
  `go test -race ./...`, `go vet ./...`, `golangci-lint run ./...` with zero
  issues, and the trimmed host binary build. `git diff --check` passes.
- Re-review requirement 4 (live Tama dynamic-client `none` flow): the `none`
  and `client_secret_post` wire fixtures are in place; the live
  authorization-code and refresh exchange against the real Tama
  authorization server remains gated on the Tama-side work tracked by issue
  #8 and is not yet verifiable.
- Re-review requirement 5 (TamaMCP conformance fixtures): the TamaMCP
  repository's fixture suite is not yet available; the equivalent malformed
  task-state, subscription-authorization, acknowledgement-ordering,
  stream-bound, and graceful/error-closure behaviors are covered by the
  fixtures listed above and will be cross-checked when the upstream suite
  lands.
