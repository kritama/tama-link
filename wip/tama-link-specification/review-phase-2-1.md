# Phase 2.1 Re-review — Issues #2–#4

Status: changes required; issues #2–#4 are not ready to close

Review date: 2026-09-16

Reviewed branch: `feature/phase-2-current-tama-adapters`

Reviewed head: `3aba5ca6729155d9c8bcbb0c43f2f56f222bb38e`

Reviewed remediation commit:

- `3aba5ca` — resolve Phase 2 review findings for issues #2–#4.

The reviewed branch was clean and synchronized with
`origin/feature/phase-2-current-tama-adapters` at the reviewed head.

Tracking issues:

- [#2 — MCP 2026 upstream transport and Tasks client](https://github.com/kritama/tama-link/issues/2)
- [#3 — OAuth discovery, PKCE, and coordinated refresh](https://github.com/kritama/tama-link/issues/3)
- [#4 — TamaMCP discovery and pinned catalog](https://github.com/kritama/tama-link/issues/4)
- [#8 — TamaMCP conformance and live Tama acceptance](https://github.com/kritama/tama-link/issues/8)
- [#9 — Phase 2 tracker](https://github.com/kritama/tama-link/issues/9)

Normative upstream baseline: TamaMCP specification commit
[`6b5db00018d2774834db5a0f00eed5b9b55e1d2e`](https://github.com/kritama/tama-mcp/blob/6b5db00018d2774834db5a0f00eed5b9b55e1d2e/wip/tama-mcp-specification.md).

## Decision

The remediation substantially improves the implementation, and the OAuth and
discovery changes described in the implementer's comments are sound. The exact
reviewed head passes the complete repository gate.

Issue #2 is not ready to close. One priority-1 SSE lifecycle regression and two
priority-2 contract gaps remain. These also prevent the collective closure of
issues #2–#4. The remediation report in
`wip/tama-link-specification/review-phase-2.md` therefore overstates the result
when it says that all priority-1 and priority-2 findings are addressed.

No implementation or GitHub changes were made during this re-review.

## Remaining findings

### P1. The SSE parser discards a successful stop signal

Location: `internal/upstream/response.go`, `scanSSE`, lines 176–199.

`dispatch` returns `(stop bool, err error)`. When it returns `(true, nil)`, the
`flush` closure returns only the nil error. The caller cannot distinguish that
successful stop from an ordinary event and continues scanning.

This affects both SSE uses:

- a finite request that has already received its matching JSON-RPC response can
  wait until the response body closes or its request deadline expires; and
- a subscription that has received its valid final JSON-RPC response can remain
  blocked until the server closes the connection or the caller cancels it.

The current fixtures close their server response immediately, so they do not
exercise the failure. This also reintroduces the processed-versus-stopped state
conflation that the transport implementation was intended to avoid.

Required change:

- preserve and propagate the stop boolean from `flush`, or use an explicit
  internal stop sentinel;
- return immediately after a matching finite response or graceful subscription
  final response; and
- add finite-response and subscription tests in which the server flushes the
  valid final event but deliberately keeps the HTTP response open.

### P2. Task-state validation is still incomplete

Locations:

- `internal/upstream/tasks.go`, `decodeTaskState`, lines 146–209; and
- `internal/upstream/calltool.go`, `decodeCallTool`, lines 87–124.

The detailed task-state decoder validates timestamps and integers only when
`stringMember` or `intMember` reports a successfully decoded value. Consequently:

- missing `createdAt` and `lastUpdatedAt` values are accepted;
- missing or explicit-null `ttlMs` values are accepted, even though the
  TamaMCP profile does not emit unlimited tasks; and
- `ttlMs` and `pollIntervalMs` values above the pinned Tasks maximum safe
  integer `9_007_199_254_740_991` are accepted when they still fit in `int64`.

The new unsafe-integer fixture uses `92233720368547758080`, which tests `int64`
overflow rather than the Tasks schema's lower maximum safe-integer boundary.

The initial task result from `tools/call` remains less strict: it requires only
a task ID and any known status. It therefore accepts missing common task
fields, unsafe but `int64`-representable values, and terminal initial states,
although the pinned TamaMCP profile creates tasks in `working` state.

Required change:

- require `createdAt`, `lastUpdatedAt`, and `ttlMs` with their specified types;
- reject null TTL for the TamaMCP profile;
- enforce non-negative values and the exact maximum safe-integer bound on TTL
  and polling values;
- validate an initial `tools/call` task result through the same common task
  envelope rules; and
- enforce the TamaMCP profile's `working` initial state at the appropriate
  upstream or adapter boundary.

Regression fixtures should cover missing timestamps, missing and null TTL,
`9_007_199_254_740_991`, `9_007_199_254_740_992`, incomplete initial task
results, and a terminal initial task state.

### P2. A supplied HTTP client timeout still bounds subscriptions

Location: `internal/upstream/client.go`, `New`, lines 98–119.

The default upstream client now correctly has no overall `http.Client.Timeout`,
and finite requests use per-request context deadlines. However, when the caller
supplies an HTTP client, `New` clones it without clearing `cloned.Timeout`.

Any non-zero timeout on the supplied client therefore still covers the complete
subscription response body and terminates the stream at that deadline. This
contradicts the configuration documentation and only partially resolves the
original timeout finding. `TestSubscriptionNotBoundByRequestTimeout` constructs
the default client and does not cover this path.

Required change:

- set the cloned client's overall timeout to zero while retaining its transport
  and other caller configuration;
- continue using the request context deadline for finite calls; and
- add a regression test with a supplied `http.Client` whose timeout is shorter
  than the held-open subscription duration.

## Implementer comment assessment

### Issue #2

The following remediations are present and sound:

- cumulative SSE event bounding;
- requested, acknowledged, and delivered task-ID binding;
- acknowledgement-first and final-error subscription handling;
- redirect refusal for default and supplied clients;
- discovery capability object validation and profile-bound checks; and
- per-request deadlines for finite calls on the default client path.

The comment's statements that all task-state and timeout findings are resolved
are not yet accurate because of the two priority-2 gaps above. The SSE-bound
rewrite also introduced the priority-1 stop-signal regression.

### Issue #3

The OAuth remediation is sound at the code and deterministic-fixture level:

- `none` and `client_secret_post` credentials are added before form encoding;
- `client_secret_basic` remains header-based;
- the exact loopback redirect URI is used in the authorization request and
  token exchange;
- redirect responses are refused; and
- stored client, refresh credential, active profile issuer, and token endpoint
  are checked before refresh.

Live verification of the real Tama public-client authorization-code and refresh
flow remains outstanding under issue #8.

### Issue #4

The discovery remediation is sound:

- the adapter protocol must fall inside the profile bounds;
- capability containers and extension values must be JSON objects; and
- a null Tasks extension is not treated as a declaration.

No additional issue-#4-specific defect was found in the remediation commit.
Collective closure remains blocked by issue #2 and the live acceptance gate.

## Outstanding external gates

- Tama Link issue #8 remains open for package conformance and live Tama
  acceptance.
- `kritama/tama-mcp` issue #9 remains open, so the consumable upstream Phase 3
  subscription fixture suite is not yet available on the reviewed baseline.
- Live evidence is still required for OAuth, owner isolation, notifications,
  polling recovery, terminal outcomes, restarts, and expired credentials before
  Phase 2 can be declared complete.

## Validation evidence

The following completed successfully on exact head
`3aba5ca6729155d9c8bcbb0c43f2f56f222bb38e`:

- `make check`:
  - `go test ./...`;
  - `go test -race ./...`;
  - `go vet ./...`;
  - `golangci-lint run ./...` with zero issues; and
  - the trimmed host binary build.
- `git diff --check c52825c..HEAD`.

The worktree remained clean after validation, and the reviewed branch matched
its tracked remote. The remaining findings are protocol and coverage defects
that are not detected by the current repository gate.

## Re-review requirements

Before issues #2–#4 are closed:

1. Correct the SSE stop propagation and add held-open stream regressions.
2. Complete required task-envelope and safe-integer validation for detailed and
   initial task results.
3. Ensure supplied HTTP client timeouts do not bound subscription streams.
4. Run `make check` and `git diff --check` on the exact remediation head.
5. Run the applicable upstream conformance fixtures when they are available.
6. Complete the live acceptance evidence tracked by issue #8 before declaring
   Phase 2 complete.

## Phase 2.1 remediation report

Status: all three remaining findings addressed; `make check` and
`git diff --check` green on the remediation head; issues #2–#4 remain open
pending re-review.

- **P1, discarded SSE stop signal** — `scanSSE`'s `flush` now returns the
  dispatch stop boolean, and the scan returns immediately when the matching
  finite response or the graceful subscription final response is parsed, even
  while the peer keeps the body open. Regressions:
  `TestFiniteResponseStopsScanImmediately` (server flushes the matching
  response and holds the body open; a 400ms request deadline would fire if
  the client waited) and
  `TestSubscriptionFinalResponseStopsScanImmediately` (ack + final response
  flushed, stream held open; the test fails with a deadline if the client
  waits). Both were verified to fail against the stop-discarding
  implementation.
- **P2, incomplete task-state validation** — the common task envelope is now
  shared by detailed task states and initial `tools/call` task results:
  `createdAt`, `lastUpdatedAt`, and `ttlMs` are required, correctly typed,
  non-negative, and bounded by the Tasks maximum safe integer
  `9_007_199_254_740_991` (2^53-1); `pollIntervalMs` carries the same bounds
  when present; explicit nulls fail like missing values. An initial
  `tools/call` task result must be in the `working` state — the pinned
  TamaMCP profile's initial state — so terminal or unknown initial states are
  protocol failures. Fixtures: missing/null `createdAt` and `lastUpdatedAt`,
  missing/null `ttlMs`, null `pollIntervalMs`, TTL and poll interval at
  `9_007_199_254_740_992` (rejected) and `9_007_199_254_740_991` (accepted),
  an incomplete initial task result, a terminal initial state, and an
  unknown initial state.
- **P2, supplied client timeout** — `upstream.New` now zeroes the overall
  timeout on the cloned caller-supplied client while retaining its transport
  and other configuration; finite requests keep their per-request deadline.
  Regression: `TestSuppliedClientTimeoutDoesNotBoundSubscriptions` supplies a
  client with a 50ms overall timeout, holds the acknowledged stream open for
  300ms, asserts the stream survives past the supplied timeout and closes
  promptly on cancellation, and asserts the supplied client object is not
  mutated. Verified to fail when the timeout is not cleared.

The specification's stream contract now states the stop-signal semantics, the
per-request deadline, the supplied-client timeout clearing, and the common
task envelope including the `working` initial state. Live acceptance evidence
and the upstream conformance fixture run remain outstanding under issue #8
and `kritama/tama-mcp#9`.
