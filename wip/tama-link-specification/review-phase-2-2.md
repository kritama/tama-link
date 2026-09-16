# Phase 2.2 Implementation Review — Issue #6

Status: changes required; issue #6 is not ready to close

Review date: 2026-09-16

Reviewed branch: `feature/phase-2-current-tama-adapters`

Reviewed local head: `6d31f07dfced1afaef3eb0fa72b1ef3713aa7288`

Reviewed commits since the previous Phase 2.1 remediation:

- `c83668b` — Phase 2 workflow foundation for `input_required`, input replay,
  and schema validation; and
- `6d31f07` — synchronous TamaMCP `2026-07-28` System execution for issue #6.

At review time the local branch was two commits ahead of
`origin/feature/phase-2-current-tama-adapters`; the remote feature branch still
pointed to `f6dfd7e2cbbf7e69ed30829994b5bf1fcf79859d`. This review therefore applies
to the local commits above whether or not they are subsequently pushed.

Tracking issues:

- [#6 — Phase 2: implement synchronous replayable System execution](https://github.com/kritama/tama-link/issues/6)
- [#8 — TamaMCP conformance and live Tama acceptance](https://github.com/kritama/tama-link/issues/8)
- [#9 — Phase 2 tracker](https://github.com/kritama/tama-link/issues/9)

## Decision

The implementation is substantial and directionally sound. It introduces a
cohesive application boundary, executes `local_replayable` operations through
the durable leased worker, captures complete synchronous results, safely
replays interrupted work after lease expiry, rejects task-shaped results as an
operation contract violation, and wires the complete profile-scoped runtime
into `serve`. The exact reviewed head passes the complete repository gate.

Issue #6 is nevertheless not ready to close. Four priority-1 correctness
findings remain: rejected task-backed submissions are persisted as orphaned
work, production wiring bypasses the canonical profile state namespaces,
runtime JSON Schema validation is not authoritative, and a saturated worker
queue can silently strand accepted work. One priority-2 classification defect
uses the wrong adapter sentinel for an unexpected task result.

The issue comment also overstates acceptance coverage. The named rejection
test does not exercise guarded or unsupported descriptors, and the existing
profile-isolation tests do not exercise the production `buildApp` wiring where
the state-path defect exists.

No implementation or GitHub changes were made during this review.

## What is sound

- `internal/application` keeps downstream handlers thin and centralizes
  catalog lookup, client and upstream argument validation, declarative
  bindings, durable acceptance, execution dispatch, long-polling, cursor
  replay, and stable boundary error mapping.
- `internal/adapter/tama2026.Connection.ExecuteLocal` permits only descriptors
  marked `local_replayable`, sends an ordinary synchronous `tools/call`
  without the Tasks capability, rejects a task-shaped response, and preserves
  the complete normalized MCP result.
- `internal/worker.Runner` claims and renews a per-submission SQLite lease,
  revalidates the durable submission before execution, prevents stale workers
  from publishing terminal results, and deliberately replays interrupted
  read-only or proven-idempotent operations after lease expiry.
- Separate-process tests demonstrate one live lease winner and restart replay
  after a process crashes during execution.
- Result-size enforcement atomically records `result_too_large` without
  retaining or returning the oversized result.
- Guarded and unsupported strategies are rejected by the application service
  before an upstream call on the implemented path.
- The upstream implementation retains the stateless MCP `2026-07-28`
  transport boundary and does not introduce initialization, protocol sessions,
  `Mcp-Session-Id`, or another legacy fallback.

## Priority 1 findings

### P1. A disabled task-backed submission is persisted before `not_implemented`

Location: `internal/application/submit.go`, `Submit`, lines 51–74.

`Submit` calls `CreateSubmission` before it branches on the descriptor's
execution strategy. For an `upstream_task` descriptor, it therefore creates an
accepted durable submission and then returns a request-level `not_implemented`
error.

The caller receives no submission ID, the accepted row is never dispatched,
and startup recovery ignores it because the current recovery path lists only
`local_replayable` work. The row remains non-terminal indefinitely and its
`client_request_id` remains claimed. A later retry receives the same hidden
submission rather than a clean rejection or a newly enabled task workflow.

This is reachable because `catalog.Callable` advertises task-backed operations;
only `unsupported` descriptors are removed from the downstream `submit` enum.

Required change:

- reject `upstream_task` in the strategy gate before `CreateSubmission` while
  issue #5 remains unimplemented;
- once issue #5 is enabled, replace that gate with the complete durable task
  acceptance and dispatch path; and
- add a regression proving a disabled task-backed call creates no submission,
  claims no idempotency key, and performs no upstream request.

### P1. Production wiring bypasses canonical profile isolation

Location: `cmd/tama-link/serve.go`, `buildApp`, lines 70–81.

The production runtime constructs its keyring with only `p.Name` and builds the
database path as:

```text
<config-dir>/state/<database-reference>.db
```

It does not use the canonical helpers already defined by the profile package:

- `profile.DatabasePath`, which includes the profile name as an outer
  directory and resolves the platform state directory; and
- `profile.CredentialNamespace`, which combines the profile name with the
  configured credential reference.

Consequently, two named profiles that both use a common database reference
such as `default` resolve to the same SQLite file. Their different keyring
prefixes ordinarily cause the second profile to fail opening that shared file
rather than allowing the required App and System processes to coexist. The
configured `state.credentials` reference is ignored entirely, and durable
state is placed under the configuration root rather than the canonical state
root.

The existing `TestStateLocationsRemainProfileIsolated` proves that the helper
functions produce isolated values, while the production code bypasses those
helpers. The credential package's isolation tests likewise cover the wrapper,
not `buildApp`.

Required change:

- resolve the database through `profile.DatabasePath`;
- construct the keyring with `profile.CredentialNamespace`;
- keep explicit test or managed-installation overrides without collapsing the
  profile-name directory; and
- add a production-wiring regression using two profile names with identical
  database and credential references, proving distinct databases, credential
  namespaces, catalogs, and instructions.

### P1. Runtime JSON Schema validation is not authoritative

Location: `internal/catalog/validate.go`, `ValidateAgainstSchema`, lines 9–38.

The validator implements a hand-written subset of JSON Schema and deliberately
ignores every unknown keyword. The profile validator does not reject schemas
that use unsupported assertion keywords. A pinned schema may therefore rely on
`oneOf`, `allOf`, `not`, `dependentRequired`, `minProperties`, `uniqueItems`,
`contains`, `exclusiveMinimum`, or another assertion that Tama Link silently
does not enforce.

Such a value can pass both local validation steps and be durably accepted even
though it violates the pinned operation contract. Deferring the rejection to
the upstream server does not satisfy the specification's requirement that
runtime validation against the selected client-visible and upstream schemas is
authoritative; it converts a downstream `invalid_request` into accepted work
and a later execution failure.

Required change:

- use a reviewed validator for the complete JSON Schema dialect permitted in
  operation descriptors; or
- define an explicit supported schema vocabulary and reject a profile at load
  time when an unsupported assertion keyword appears, including recursively;
- preserve exact number semantics and the existing canonicalization
  guarantees; and
- add fixtures proving unsupported assertions cannot be silently ignored and
  that representative composition, object, array, string, and numeric
  constraints produce deterministic validation results.

### P1. A full worker queue silently strands accepted work

Location: `internal/worker/service.go`, `Dispatch`, lines 52–58.

`Dispatch` performs a non-blocking send to a channel with capacity 256 and
silently drops the submission ID in its `default` branch. The comment says a
full queue defers to startup recovery or a later dispatch, but startup recovery
runs only once before the server accepts new calls, and no periodic durable
sweep exists. A later dispatch of another submission does not rediscover the
dropped ID.

The durable row therefore prevents data loss but does not guarantee progress:
an accepted submission can remain in `accepted` indefinitely until the process
is restarted. This contradicts the promise that successful `submit` means Tama
Link has durably accepted responsibility for executing the operation.

Required change:

- make every durable acceptance discoverable by a continuing worker, for
  example through guaranteed enqueueing combined with cancellation or a
  recurring durable `ListRunnable` sweep;
- retain lease acquisition as the final single-winner guard across processes;
- surface or retry scheduling failures instead of discarding them; and
- add a saturation regression proving more than the in-memory queue capacity
  eventually reaches execution or an explicit terminal state without a
  process restart.

## Priority 2 finding

### P2. `ExecuteLocal` wraps the wrong unexpected-task sentinel

Locations:

- `internal/adapter/tama2026/local.go`, `ExecuteLocal`, lines 34–35; and
- `internal/application/classify.go`, `classify`, lines 39–44.

The adapter defines `ErrUnexpectedTaskResult`, and the application classifier
has a dedicated stable message for it. `ExecuteLocal` instead wraps
`ErrCatalogMismatch` when a synchronous call returns a task.

Both errors currently map to `operation_contract_mismatch`, so the broad
application test passes. The caller nevertheless receives the misleading
catalog-drift message, the promised distinct sentinel is not observable on
this execution path, and the test cannot detect a regression between the two
causes.

Required change:

- wrap `ErrUnexpectedTaskResult` from `ExecuteLocal`; and
- add a direct adapter regression asserting `errors.Is` plus an application
  assertion for the dedicated safe message.

## Acceptance-coverage gaps

The implementation comment on issue #6 maps guarded and unsupported rejection
to `TestSubmitRejectedCases`. That test currently covers only:

- an unknown tool;
- a missing `client_request_id`;
- client-schema failure; and
- non-object arguments.

The application fixture contains one `local_replayable` descriptor and one
`upstream_task` descriptor. It contains no `local_guarded` or `unsupported`
descriptor, so the required pre-upstream rejection boundary is not exercised.
The test should also assert that rejection creates no durable submission and
does not claim the request ID.

The issue also requires App and System state, catalogs, credentials, owner
bindings, and instructions to remain isolated. Existing tests separately
exercise profile path helpers, credential prefixes, and server instructions,
but no test instantiates the production runtime for two profiles. That gap is
why the `buildApp` namespace bypass remains green.

Live Tama acceptance remains correctly deferred to issue #8 and does not block
fixing or deterministically testing the local findings above.

## Validation evidence

The following completed successfully on exact local head
`6d31f07dfced1afaef3eb0fa72b1ef3713aa7288`:

- `make check`:
  - `go test ./...`;
  - `go test -race ./...`;
  - `go vet ./...`;
  - `golangci-lint run ./...` with zero issues; and
  - the trimmed host binary build.
- `git diff --check f6dfd7e..HEAD`.

The worktree was clean after validation. The green gate confirms that the
implemented and currently covered paths are internally consistent; it does not
resolve the correctness and acceptance-coverage findings above.

## Re-review requirements

Before issue #6 is closed:

1. Reject unimplemented task-backed work before durable acceptance.
2. Use the canonical database path and credential namespace in production
   wiring and prove two-profile isolation through that wiring.
3. Make pinned-schema validation authoritative or reject unsupported schema
   assertions at profile load.
4. Replace lossy worker dispatch with a scheduling mechanism that guarantees
   eventual discovery of every accepted replayable submission.
5. Return the distinct unexpected-task sentinel and verify its stable mapping.
6. Add guarded and unsupported rejection fixtures that prove no durable or
   upstream side effect occurs.
7. Run `make check` and `git diff --check` on the exact remediation head.
8. Keep issue #6 open pending re-review; complete the migrated-Tama live gate
   separately under issue #8.

## Remediation (this phase)

Status: remediated in this worktree; pending re-review.

1. `upstream_task` and non-replayable strategies are rejected by the submit
   strategy gate before durable acceptance. `TestSubmitRejectedCases` covers
   `not_implemented` for task-backed tools, `operation_not_allowed` for
   guarded and unsupported tools, and
   `TestSubmitRejectionsLeaveNoTrace` proves no submission row, no
   idempotency claim, and no upstream request occur.
2. `serve` now resolves the state through `stateLayout` (profile `DatabasePath`
   and `CredentialNamespace`) and opens credentials through the injectable
   `credentialOpener`. `TestBuildAppKeepsProfilesIsolated` builds the full
   application for two profiles from the same config file and asserts
   distinct databases, credential namespaces, catalogs, and owner prefixes.
3. `CheckSchemaVocabulary` now rejects any unsupported JSON Schema assertion
   keyword in pinned operation schemas at profile load, recursively through
   `properties`, `items`, and `additionalProperties`; boolean
   `additionalProperties` is honored at runtime. Tests cover nested rejection,
   descriptor-level rejection, and acceptance of the full enforced vocabulary.
4. The worker service loop now includes a recurring durable sweep
   (`sweepRunnable`, default 5s) that re-derives runnable replayable
   submissions from the store; the in-memory queue is a prompt-start aid
   only. `TestSaturatedQueueStillExecutesEverySubmission` submits more
   work than the queue depth (300 > 256) and asserts every submission
   reaches `completed` without a restart. Supporting semantics: lease
   release is best-effort with bounded retries after a terminal transition,
   and a busy-timeout claim or transition is a transient condition that
   startup recovery defers to the sweep (`store.ErrBusy`).
5. `ExecuteLocal` now wraps `ErrUnexpectedTaskResult`;
   `TestExecuteLocalRejectsTaskResult` pins the adapter contract and
   `TestSubmitUnexpectedTaskResultFailsContractMismatch` pins the stable
   downstream mapping `operation_contract_mismatch` with the dedicated
   message.
6. The fixture profile and fake upstream now include guarded and unsupported
   tools; the rejection tests above also assert the no-trace contract for
   every non-replayable strategy.
7. `make check` (fmt, test, race, vet, lint zero issues, build) passes on the
   remediation head. The hermetic STDIO e2e defaults to a dead D-Bus socket
   for the spawned serve process (no keyring prompt can appear, matching
   headless CI) and opts into the real platform keyring with
   `TAMA_LINK_E2E_KEYRING=1`.

Open until re-review confirms the above; live acceptance remains under
issue #8.
