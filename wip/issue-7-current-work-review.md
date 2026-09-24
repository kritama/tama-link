# Issue #7 current-work review

Date: 2026-09-24

Branch: `feature/phase-2-downstream-handlers`

Head: `0a7463bb345b3f2350bfa675c72b982400707ad5`

Base: `develop` at `5ece259fa9d4d5e65cdf0a545255d55f751287cf`

Scope: the named HEAD plus the current uncommitted working-tree changes.

Issue: [#7 Phase 2: wire submit, await, and request-correlated input responses](https://github.com/kritama/tama-link/issues/7)

## Revalidation verdict

All four original gaps are resolved in the working tree. Profile isolation,
mandatory application wiring, built-binary App/System STDIO coverage, and the
server-side cancellation completion assertion are present and pass the
repository gates. The System binary flow also requires
`status == completed`. Live migrated-Tama acceptance remains Issue #8.

## Findings

### 1. High: the loaded profile does not select an isolated App or System service

Resolved. `Profile.Kind` rejects mixed App and System operations and requires
the matching endpoint path. `application.New` and `buildApp` start only the
owned service. Downstream fixtures and the binary gate use separate profiles.

### 1-original. High: the loaded profile does not select an isolated App or System service

Issue #7 requires the thin handlers to call the focused App or System service
selected by the loaded process profile. The specification is stricter: one
profile must not combine `/mcp/app` and `/mcp/system` catalogs, credentials,
instructions, or durable state (`wip/tama-link-specification.md:129-145`).

The current application configuration requires both the synchronous worker and
the App task service for every profile (`internal/application/service.go:18-42,
67-101`). `buildApp` constructs and starts both services unconditionally
(`cmd/tama-link/serve.go:183-229`), while `Submit` chooses between them per
operation descriptor (`internal/application/submit.go:150-164`). Profile
validation checks each descriptor independently but never rejects a catalog
that mixes `local_replayable` and `upstream_task`
(`internal/profile/config.go:112-169`).

The new success tests conceal this boundary because their default fixture
profile contains both the System `status` operation and the App `message`
operation in one profile (`internal/application/fixture_test.go:
173-241`). Only the System-only filtered case is checked; there is no matching
App-only profile test.

This permits one process to combine both execution models and share one
database and credential namespace, contrary to the documented isolation
contract. It also means the downstream tests do not prove that a loaded profile
selects exactly one focused service.

Required correction:

- make the profile carry or derive one explicit execution kind and reject
  catalogs that mix App-task and System-replay strategies;
- construct and start only the service owned by that profile, while preserving
  recovery of submissions belonging to that profile; and
- split the downstream fixtures into separate App-only and System-only
  profiles, proving that each advertises and executes only its own operations.

### 2. Medium: the public server still exposes the repository-foundation placeholders

Resolved. `server.New` returns an error when the application is missing, and
the placeholder handlers are gone.

### 2-original. Medium: the public server still exposes the repository-foundation placeholders

The goal of Issue #7 is to replace the downstream `not_implemented`
placeholders. `server.New` still accepts a nil application, selects the
placeholder `submit` and `await` operations, and advertises the normal two-tool
surface (`internal/server/server.go:25-52,174-181`). Existing server tests still
construct this nil-app server (`internal/server/server_test.go:49-52`).

Production currently supplies the application, and the binary test proves that
its calls do not return `not_implemented`. The fallback nevertheless leaves a
public construction path that silently creates a server which satisfies MCP
initialization and tool discovery but cannot perform Issue #7 behavior. That is
the placeholder path the issue says to remove.

Required correction: make a non-nil application mandatory when constructing a
server. Prefer a constructor that returns an error for a missing application;
tests that only need catalog/schema inspection should supply a focused fake
application instead of relying on production placeholders. Remove the
placeholder handlers and message once no caller can reach them.

### 3. Medium: the downstream cancellation test never proves an active MCP wait is cancelled

Resolved. The long-call wrapper closes its completion channel only after
`Service.Await` returns. The test cancels after entry and requires that
server-side signal within two seconds, then checks the submission is still
running and that `tasks/cancel` was not called.

### 3-original. Medium: the downstream cancellation test never proves an active MCP wait is cancelled

`TestDownstreamAwaitCursorTimeoutCancellationAndErrors` cancels its context
before calling `ClientSession.CallTool`
(`internal/application/downstream_test.go:184-198`). In the pinned MCP SDK,
the IO transport checks `ctx.Done()` before writing a frame
(`github.com/modelcontextprotocol/go-sdk` v1.7.0, `mcp/transport.go:673-677`).
The test can therefore pass without the server receiving the `await` request,
starting a bounded wait, observing a cancellation notification, or propagating
that cancellation into `Service.Await`.

Application-level tests prove that a directly cancelled service call does not
invoke `tasks/cancel`, but Issue #7 specifically requires the downstream MCP
handler boundary to propagate disconnection/cancellation correctly.

Required correction: start an `await` with a live context, synchronize on
evidence that the server handler/application wait has begun, cancel the client
call or close the downstream transport, and assert prompt completion. Also
assert that the accepted submission remains non-terminal and the upstream
fixture observed no `tasks/cancel` request.

### 4. Medium: successful App/System workflows do not cross the built-binary STDIO boundary

Resolved. `TestBinaryStdioCompletesAppAndSystemWorkflows` builds the binary
with the `tamalinkfixture` tag, which is absent from production builds, and
drives separate App and System `serve` processes over raw STDIO. The App flow
covers `input_required` and `input_responses`. The System flow covers replay,
conflict, cursor continuation, and retained terminal reads.

The System flow now requires `status == completed` on both the terminal read
and the retained replay, matching the App flow.

### 4-original. Medium: successful App/System workflows do not cross the built-binary STDIO boundary

The real-binary test launches `bin/tama-link`, performs initialization and
`tools/list`, then exercises only an unauthenticated `submit` and an unknown
submission `await` (`cmd/tama-link/e2e_test.go:180-391`). The production-stack
STDIO test uses an injected keyring but runs `server.New` directly over
`io.Pipe`; it also exercises only `authentication_required` and
`submission_not_found` (`cmd/tama-link/stdio_test.go:22-128`).

Successful System execution, App task/input handling, idempotency conflict,
pending timeout, cursor behavior, cancellation, and retained terminal replay
are covered through an in-memory MCP transport
(`internal/application/downstream_test.go:21-241`). Those tests are valuable,
but they do not satisfy Issue #7's explicit real-binary STDIO acceptance
boundary: binary startup, profile loading, credential/state wiring, worker/task
startup, and newline-delimited framing are not exercised together with a
successful workflow.

Required correction: add a real-binary fixture seam that supplies deterministic
credentials and a scripted upstream without weakening production keyring or
endpoint policy. Launch separate App and System profile processes and drive at
least one successful `submit` -> repeated `await` -> terminal flow through raw
STDIO for each. Include App `input_required`/`input_responses`, System replay,
cursor continuation, and retained terminal reads in that process-level gate.

## Acceptance status

| Issue #7 criterion | Status | Evidence |
| --- | --- | --- |
| Exactly `submit` and `await` | Pass | In-memory, production-stack STDIO, and built-binary tests assert the exact two-tool list. |
| `await.input_responses` schema | Pass | Both in-memory and built-binary catalog checks require the field. |
| Partial, exact, conflicting, and stale input responses | Pass below binary boundary | The downstream in-memory test reaches the real application and upstream fixture. |
| Durable submit, idempotent retry/conflict, pending timeout, terminal replay | Pass below binary boundary | Covered through the downstream MCP server with the in-memory transport. |
| Cursor dedupe and invalid cursor handling | Pass below binary boundary | Downstream tests cover retained cursor filtering and beyond-sequence rejection. |
| Active downstream cancellation | Pass | The test waits for `Service.Await` to return after cancellation, then checks the submission is still running and `tasks/cancel` was not called. |
| Separate profile-selected App/System services | Pass | Mixed profiles are rejected, endpoint kind is checked, and only the selected service is constructed and started. |
| No reachable `not_implemented` handlers | Pass | `server.New` rejects a nil application and the placeholder handlers have been removed. |
| Successful real-binary STDIO App/System workflows | Pass | Separate fixture-tagged processes cross raw STDIO. Both flows require `status == completed`. |
| Thin handlers and stable error translation | Pass | Server handlers decode, call the application interface, and attach structured output without owning persistence, OAuth, transport, or scheduling. |
| Protocol-only stdout and redacted diagnostics | Pass for exercised binary paths | The binary test parses every stdout line as JSON-RPC and checks that the secret marker is absent from stderr and the result. |
| `make check` | Pass | Completed on 2026-09-24 against `0a7463b` plus the current uncommitted changes. |

## Revalidation performed

- `git diff --check` passed for the current working tree.
- The cancellation and built-binary workflow tests passed 20 repetitions under
  the race detector:
  `go test -race ./internal/application ./cmd/tama-link -run 'Test(DownstreamCancellationStopsActiveWait|BinaryStdioCompletesAppAndSystemWorkflows)$' -count=20`.
- `GOCACHE=/tmp/tama-link-go-cache GOLANGCI_LINT_CACHE=/tmp/tama-link-lint-cache make check`
  passed on 2026-09-24, including unit tests, race tests, `go vet`,
  golangci-lint, and the trimmed binary build.

## Closure gate

Findings 1-4 are addressed in the working tree. Issue #7 can close after the
focused downstream and binary tests pass under the race detector and `make check`
plus `git diff --check` pass on the committed head. Live migrated-Tama
acceptance remains Issue #8 and must not be inferred from this fixture review.
