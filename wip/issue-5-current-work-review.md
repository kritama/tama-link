# Issue #5 current-work review

Date: 2026-09-24

Branch: `feature/phase-2-app-tasks`

Base: `develop` at `54e4f0a`

Issue: [#5 Phase 2: implement owner-bound App tasks, inputs, and subscriptions](https://github.com/kritama/tama-link/issues/5)

## Verdict

The current work implements the main Issue #5 shape: prompt durable acceptance,
server-directed task creation, owner-bound task attachment, leased polling,
input responses through `await`, subscription hints, terminal capture, and
startup rediscovery.

The third revalidation on 2026-09-24 found that all five original implementation
findings are resolved. Finding 2 now serializes input delivery across processes,
renews the lease across slow upstream requests, and makes both task snapshots
and failure-driven terminal transitions contend on that same lease. The focused
race suite covers each identified interleaving.

The fifth revalidation on 2026-09-24 closed the two remaining fixture assertion
gaps. Process B's `tasks/get` must be authenticated, retained terminal replay
goes through `await` with a connector that fails if called, and ambiguous
replay compares the canonical calls and rejects an identifier reused with
different arguments. Live Tama acceptance remains Issue #8.

The focused acceptance suite passes five repeated race-enabled runs, and the
full `make check` gate passes. Local Issue #5 acceptance is complete. Delivery
still requires a committed and pushed branch, a pull request, and exact-head CI
and review evidence.

## Acceptance completion plan

Issue #5 can be closed after the three remaining local gaps below are replaced
with black-box fixture evidence and the repository gate passes. Live validation
against migrated Tama remains owned by Issue #8 and depends on Tama #123; it
must not be reported as complete when only the Issue #5 fixture gate has passed.

### 1. Capture complete terminal failure and cancellation evidence

Preserve the validated upstream terminal document before the adapter projects
it to the safe downstream error taxonomy. A focused implementation would:

- carry a lossless terminal payload on `tama2026.TaskSnapshot`; for `failed`
  and expiry this must include the complete upstream `error` object, and for
  `cancelled` it must retain the complete validated cancellation snapshot;
- add a dedicated encrypted terminal-task blob to the store schema, submission
  scanner/decryptor, schema validator, and garbage collector, bumping the
  fail-closed schema version and using a distinct AEAD purpose from arguments,
  events, input requests, and results;
- write the terminal status, safe stable Link error, encrypted terminal
  evidence, completion timestamps, and cleared outstanding input state in one
  lease-checked transaction; and
- enforce the submission's accepted byte limits before sealing, without
  truncating the evidence or exposing it through `await`, logs, or stdout.

The acceptance tests must use a failure document with extension fields that
would be lost by the current projection and prove all of the following:

- the exact validated failure survives close/reopen with the same state key;
- the SQLite file contains neither the failure message nor its extension
  marker in plaintext;
- the client still receives only the stable safe Link error;
- cancellation survives close/reopen with its complete validated terminal
  evidence; and
- terminal status and evidence cannot be split by a crash or lease loss.

### 2. Replace the in-process restart test with a real process boundary

Extend the existing application process-test harness instead of adding another
test framework. The parent test should own the scripted upstream endpoint and
shared SQLite/key paths, then run these phases:

1. Process A opens the profile, authenticates, submits once, receives and
   persists the opaque task ID, and exits while the task is still working.
2. Process B opens the same database with the same state key but constructs a
   new OAuth/upstream connection. It authenticates independently, calls
   `tasks/get` for the persisted task ID, and captures completion without a
   second `tools/call`.
3. After Process B exits, the parent reopens the store and proves a terminal
   `await` can be replayed entirely from local state with the upstream endpoint
   unavailable.

The fixture should record process IDs, authorization observations, task IDs,
`tools/call` attempts, `tasks/get` calls, and graph executions. Acceptance
requires different process IDs, fresh authenticated requests after restart,
one stable task ID, exactly one initial call/graph execution, at least one
recovery `tasks/get`, and retained terminal replay after another reopen.

### 3. Model genuinely ambiguous initial acceptance

Replace the seeded no-task-ID test with a fault-injection fixture that behaves
like an idempotent Tama Message Submission adapter:

1. On the first canonical `tools/call`, create and count the graph work, bind
   the caller's `client_request_id`/upstream `identifier` to one task ID, then
   drop the response after acceptance so Link never receives the handle.
2. Stop that Link process with its durable submission in `running` and no task
   ID.
3. Start a new Link process. When the pinned descriptor proves the
   `client_request_id` binding, replay the same canonical call. The fixture
   must return the previously created task ID from its idempotency map rather
   than creating graph work again.
4. Complete through `tasks/get` and assert two HTTP call attempts but exactly
   one graph execution, one identifier, one canonical argument document, and
   one terminal local submission.

Run the mirror case without the proven binding: the first call still reaches
the fixture and loses its response, but restart must produce
`outcome_unknown`, make zero replay attempts, and leave the graph-execution
count at one.

### 4. Final local closure gate

Run the new process and terminal-storage tests repeatedly under the race
detector, then run `make check` with writable Go and lint caches and
`git diff --check`. Update this review only after all three gap tests pass.
At that point Issue #5 has the fixture evidence named in its acceptance
criteria and can close.

Full Phase 2/live acceptance is a later, separate gate: after Tama #123 lands,
Issue #8 must repeat the owner-bound recovery flow against real Tama and OAuth,
including Link restart, Tama restart, same-owner `tasks/get`, no duplicate
graph work, and retained terminal replay. Until then, describe the result as
"Issue #5 local acceptance complete," not "live Tama acceptance complete."

## Fourth revalidation: merge readiness

### 6. High: the process-restart fixture does not prove the complete restart contract

Resolved. The fixture rejects an unauthenticated `tasks/get`, and the test
requires a fresh authorized lookup from Process B. After the upstream is
closed, retained replay is `Service.Await` against a connector that fails if
called (`internal/application/task_accept_test.go`).

### 6-original. High: the process-restart fixture does not prove the complete restart contract

`TestTaskRestartCrossesProcessBoundary` proves distinct processes, one
`tools/call`, one graph count, and a recovered `tasks/get`. It checks only that
Process A produced at least one authenticated request
(`internal/application/task_accept_test.go:42-43`); it does not require Process
B's `tasks/get` itself to carry authentication. After shutting down the
upstream, the parent reads the submission directly with `Store.GetSubmission`
(`internal/application/task_accept_test.go:59-70`) instead of invoking
`Service.Await`. The fixture can therefore pass without proving the issue's
"independently authenticated owner" lookup or its requirement that retained
terminal replay be served through the downstream application contract while
the upstream is unavailable.

Required correction: record authorization per method/process (or reject an
unauthenticated `tasks/get`), assert that Process B performs a fresh
authenticated lookup, then reopen the application with an upstream connector
that fails if called and require terminal `await` to return the captured result.

### 7. High: ambiguous-replay acceptance does not compare the canonical calls

Resolved. Every `tools/call` keeps its tool name, canonical arguments, `_meta`,
and protocol headers, excluding the JSON-RPC id. The fixture rejects one
identifier reused with a different canonical hash. The proven-binding test
asserts the dropped call and the replay are identical
(`internal/application/task_accept_test.go`, `internal/application/task_fixture_test.go`).

### 7-original. High: ambiguous-replay acceptance does not compare the canonical calls

The ambiguous fixture counts graph work by `identifier` alone
(`internal/application/task_fixture_test.go:162-174`). The test asserts two
HTTP attempts and one graph count, but it retains only the latest request and
never compares the first accepted call with the replay
(`internal/application/task_accept_test.go:102-125`). A replay with changed
tool arguments or metadata would still pass as long as it reused the same
identifier. That does not prove the live Issue #5 requirement of one canonical
argument document.

Required correction: retain the projected tool name, canonical arguments, and
relevant request metadata for every `tools/call`; make the fake Tama
idempotency map reject one identifier with a different canonical hash; and
assert that the dropped first call and Process B replay are identical apart
from JSON-RPC transport identity.

### Delivery state

The feature branch currently points at the same commit as `develop`
(`54e4f0a`). All Issue #5 implementation and tests are uncommitted, many are
untracked, the branch has no upstream, and no pull request exists. Even after
the two fixture gaps are corrected, it must be committed, pushed, reviewed,
and pass CI at the exact proposed head before merge.

## Revalidation status

| Item | Status | Current evidence |
| --- | --- | --- |
| Finding 1: subscription credential expiry | Resolved | Production passes the OAuth client into the task runner, bounds each stream by `Expiry`, refreshes after expiry, reconciles with `tasks/get`, and resubscribes (`cmd/tama-link/serve.go:196-200`, `internal/application/task_watch.go:25-69,73-120`). The test holds a stream open, fires expiry, and requires refresh plus a second subscription (`internal/application/task_subscribe_test.go:110-151`). |
| Finding 2: duplicate/stale input delivery | Resolved | A durable per-submission delivery lease serializes waits, is renewed during `tasks/update`, cancels the update if renewal loses ownership, revalidates the pending outstanding set immediately before the send, and gates snapshot application on the same lease (`internal/application/task_input.go:43-217,254-269`, `internal/application/task_apply.go:26-44`). Failure-driven terminal transitions now acquire the same lease and defer to the recovery sweep when delivery owns it (`internal/application/task_follow.go:123-180`). Slow-update, snapshot-transition, and failure-transition interleavings pass under the race detector (`internal/application/task_input_race_test.go:16-200`), in addition to same-value, conflicting, mixed, stale, lost-ack, and separate-process coverage. |
| Finding 3: oversized task response retries forever | Resolved | Both task creation and `tasks/get` classify `KindTooLarge` as terminal `result_too_large` (`internal/application/task_run.go:127-146`, `internal/application/task_follow.go:123-137`, `internal/application/classify.go:55-67`). Pre-decode response-bound tests cover both paths (`internal/application/task_test.go:454-499`). |
| Finding 4: subscription erases capability snapshot | Resolved | Subscription normalization now supplies the connection capability snapshot (`internal/adapter/tama2026/task.go:89-107`), and the terminal-via-subscription test asserts that the persisted snapshot remains populated (`internal/application/task_subscribe_test.go:31-49`). |
| Finding 5: Tasks integers narrow through `time.Duration` | Resolved | Milliseconds remain `int64` through adapter and store; timer conversion is range-capped separately (`internal/adapter/tama2026/task.go:26-40,150-160`, `internal/store/task_meta.go:16-26`, `internal/application/task_time.go:8-26`). Boundary and maximum-safe-integer tests cover normalization and persistence (`internal/adapter/tama2026/task_millis_test.go:10-28`, `internal/store/task_meta_test.go:12-46`). |

### Local acceptance implementation status

Implemented on 2026-09-24, subject to Findings 6 and 7 above:

- Terminal failure and cancellation documents are stored in `terminal_evidence_enc` with AEAD purpose `terminal-evidence`, written in the same lease-checked transaction as the terminal status. `await` returns only the stable error. Close/reopen and plaintext-SQLite checks are in `internal/application/task_evidence_test.go`. A lost lease commits neither status nor evidence (`internal/store/terminal_evidence_test.go`).
- Restart crosses a process boundary: process A persists the task ID and exits while working; process B opens the same database and a new connection, authenticates, and completes through `tasks/get` without a second `tools/call`. The parent then reads the terminal result from the reopened store (`internal/application/task_accept_test.go`).
- Ambiguous acceptance drops the first `tools/call` after the fixture counts graph work. Proven bindings replay that identifier and do not create a second graph execution. The unproven mirror becomes `outcome_unknown` with no replay (`TestAmbiguousAcceptanceDoesNotDuplicateGraphWork`).

The authorized-empty-subset, established-stream credential-expiry, and
concurrent/separate-process input-delivery gaps from the original review now
have tests. Lease renewal plus snapshot- and failure-transition exclusion close
the races identified in Finding 2.

## Original findings

The sections below preserve the evidence from the initial review. Their current
status is authoritative only in the revalidation table above.

### 1. High: a subscription is not bounded by the access-token expiry

`oauth.Client` exposes both `Expiry` and `Refresh` specifically for a
subscription owner to close a stream no later than credential expiry and then
refresh it (`internal/oauth/token.go:155-171`). The production wiring reduces
that client to a request-time `TokenProvider` (`cmd/tama-link/serve.go:145-166`),
and the task watcher passes its lease-owned context directly to `WatchTask`
without an expiry deadline (`internal/application/task_watch.go:11-47`). A
healthy, held-open SSE stream can therefore continue past the bearer token's
expiry indefinitely.

This violates specification decision D13 and Issue #5's subscription-expiry
recovery requirement. The existing test named `credential expiry falls back
to tasks/get` returns an immediate HTTP 401; it does not open a stream, advance
to token expiry, or prove forced refresh and resubscription
(`internal/application/task_subscribe_test.go:98-109`).

Required correction: give the subscription owner access to the OAuth expiry
and forced-refresh operations, derive a stream context ending no later than the
current token expiry, then reconcile with `tasks/get`, refresh, and resubscribe.
Cover the sequence with a controlled clock and a stream that remains open past
the original expiry.

### 2. High: concurrent `await` calls can send duplicate or stale `tasks/update` requests

Input response recording is atomic per request ID, but delivery is a separate
read/call/write sequence: `InputResponsePending`, `UpdateTask`, then
`MarkInputResponsesSent` (`internal/application/task_input.go:28-57`). There is
no submission lease, delivery claim, or compare-and-set around that sequence.
Two processes or concurrent waits can both observe `sent_at IS NULL` and both
send the same response upstream. A task transition can also make the request
stale after `Await` loaded its snapshot but before the update is sent.

The mixed partial-replay case also resends already delivered IDs: if one ID in
the caller's map is pending, the code sends the entire canonical map rather
than only the pending subset (`internal/application/task_input.go:32-55`).
That conflicts with the Link contract that exact replays are no-ops and stale
request IDs are rejected locally. The current tests exercise only sequential
replay (`internal/application/task_test.go:188-253`).

Required correction: serialize delivery through a durable per-submission or
per-input claim, revalidate the current outstanding set under that claim, send
only the claimed pending IDs, and commit the acknowledgement with ownership
checking. Add concurrent same-value/different-value waits, mixed sent/pending
maps, stale-transition, lost-ack, and separate-process coverage.

### 3. High: an oversized task response is retried forever instead of becoming terminal

`tasks/get` correctly uses the submission's accepted response bound
(`internal/application/task_follow.go:57-65`). When the body exceeds that
bound, the upstream client returns `KindTooLarge`, which the shared classifier
maps to `result_too_large`. The task runner's `failOrDefer` does not classify
that error; it falls through to `nil`, releases the lease, and lets the sweep
retry forever (`internal/application/task_follow.go:107-120`). The initial
`tools/call` path has the same behavior for a replay-proven App operation
(`internal/application/task_run.go:127-146`).

The existing oversized-result test lowers only `ResultBytes`, so it covers the
post-decode storage bound but not the accepted upstream response bound
(`internal/application/task_test.go:319-339`).

Required correction: terminalize `KindTooLarge` through the stable
`result_too_large` mapping on both task creation and `tasks/get`, and add tests
where `ResponseBytes` is exceeded before the task document can be decoded.

### 4. Medium: subscription snapshots erase the persisted capability snapshot

Polling passes the verified connection capability snapshot into task
normalization, but subscription normalization passes `nil`
(`internal/adapter/tama2026/task.go:68-73,101-106`). Every observation then
writes `string(snap.Capabilities)` to `task_capabilities`
(`internal/application/task_apply.go:73-83` and
`internal/store/task_meta.go:61-67`). The first accepted notification therefore
replaces the required persisted capability snapshot with an empty string.

Required correction: subscription snapshots must retain the connection's
verified capability snapshot, or persistence must preserve the previous value
when a notification contains no new capability data. Add a terminal-via-
subscription assertion that reopens the store and verifies the snapshot.

### 5. Medium: valid Tasks integers are narrowed through `time.Duration` before persistence

The wire validator accepts task integers through the Tasks safe-integer maximum
of `2^53-1`, but both task creation and detailed-state normalization convert
milliseconds by multiplying a `time.Duration`
(`internal/adapter/tama2026/task.go:50-57,153-159`). Values above Go's maximum
duration in milliseconds wrap before `TaskRecord` converts them back with
`Milliseconds` (`internal/store/task_meta.go:61-67`). The persisted `ttlMs` and
`pollIntervalMs` can therefore differ from the validated wire values, and a
wrapped poll interval can strand or over-poll accepted work.

Required correction: keep TTL and poll interval as checked `int64`
milliseconds through the adapter and store boundary. Convert to a duration
only after checking the narrower timer range and applying an explicit policy.
Add adapter/store round-trip tests at the duration boundary and the Tasks
safe-integer maximum.

## Original acceptance evidence gaps

This is the initial gap list. The revalidation section above identifies which
items have since gained coverage and which remain open.

The current tests cover the ordinary happy path, sequential idempotency,
state mapping, polling fallback, local wait cancellation, result storage
overflow, and same-store service restart. They do not yet prove several Issue
#5 criteria:

- `TestTaskRestartResumesSameTask` reuses the same open `Store` and memoized
  connection; it does not close/reopen SQLite or create a newly authenticated
  application process (`internal/application/task_test.go:341-381`).
- The ambiguous-replay test seeds a running row with no task ID and performs
  the first upstream call after recovery. It does not simulate a call that
  reached Tama before Link lost the handle, so it cannot prove that replay
  avoids duplicate graph work (`internal/application/task_test.go:384-410`).
- The authorized-subset test acknowledges `other-task`, which is outside the
  requested set and is rejected as a protocol violation. It does not exercise
  the valid empty authorized subset for the requested task
  (`internal/application/task_subscribe_test.go:66-76`).
- Credential-expiry coverage is the immediate-401 case described in finding 1,
  not an established stream expiring and recovering.
- There is no concurrent or separate-process `tasks/update` delivery test, as
  described in finding 2.
- The normalized store retains only a generic stable failure envelope; the
  complete upstream failed-task error document is discarded during
  normalization (`internal/adapter/tama2026/task.go:174-188`). Issue #5 says
  the complete terminal failure must be captured in encrypted local state, so
  the intended encrypted representation and replay test remain unspecified.

## Validation performed

- Fifth revalidation: the restart, authenticated lookup, terminal `await`,
  canonical replay, conflicting-identifier, and terminal-evidence tests passed
  five consecutive runs under the race detector.
- Fifth revalidation: `GOCACHE=/tmp/tama-link-go-cache
  GOLANGCI_LINT_CACHE=/tmp/tama-link-lint-cache make check` passed, including
  unit tests, race tests, `go vet`, lint (`0 issues`), and the trimmed build.
- Fourth revalidation: the new restart, ambiguous-acceptance, and terminal-
  evidence tests passed five consecutive runs under the race detector.
- Fourth revalidation: `GOCACHE=/tmp/tama-link-go-cache
  GOLANGCI_LINT_CACHE=/tmp/tama-link-lint-cache make check` passed, including
  unit tests, race tests, `go vet`, lint (`0 issues`), and the trimmed build.
- Fourth revalidation: `git diff --check` passed.
- Third revalidation: the focused input-delivery suite, including the new
  failure-transition interleaving, passed five consecutive runs under the race
  detector.
- Third revalidation: the full `make check` gate passed, including unit tests,
  race tests, `go vet`, lint (`0 issues`), and the trimmed build.
- Second revalidation: focused task-input concurrency tests ran three times
  under the race detector and passed, including slow lease renewal, snapshot
  exclusion, concurrent same/different responses, mixed/lost acknowledgement,
  stale input, and separate-process delivery.
- Second revalidation: the full `make check` gate passed after the latest
  changes, including unit tests, race tests, `go vet`, lint (`0 issues`), and
  the trimmed build.
- Revalidation: `go test ./internal/application ./internal/store
  ./internal/adapter/tama2026 ./internal/upstream`: passed.
- Revalidation: `GOCACHE=/tmp/tama-link-go-cache
  GOLANGCI_LINT_CACHE=/tmp/tama-link-lint-cache make check`: passed outside the
  filesystem/network sandbox; this included unit tests, race tests, `go vet`,
  lint (`0 issues`), and the trimmed build.
- `git diff --check`: passed.
- `go test ./internal/application ./internal/store ./internal/adapter/tama2026 ./internal/upstream`: passed.
- `GOCACHE=/tmp/tama-link-go-cache GOLANGCI_LINT_CACHE=/tmp/tama-link-lint-cache make check`: passed outside the filesystem/network sandbox so the `httptest` suites could bind loopback sockets.
- The first unqualified `make check` reached lint after tests, race tests, and
  `go vet` passed, but the linter could not write Go's module stat cache under
  the restricted home directory and reported `no go files to analyze`. The
  documented temporary writable caches resolved that environment-only error;
  the pinned linter then reported `0 issues`.
- A fresh sandboxed full-gate attempt could not bind `httptest` loopback
  listeners (`socket: operation not permitted`). Re-running the same command
  with loopback permission passed tests, race tests, `go vet`, lint, and the
  trimmed build.
- CodeRabbit 0.7.8 was available, but its external review was not run because
  sending private uncommitted and untracked source to a third party was not
  authorized. No CodeRabbit result is used as review evidence.

## Live dependency and delivery status

Verified on 2026-09-24:

- Tama Link #3 (OAuth lifecycle): closed.
- Tama Link #4 (discovery/catalog boundary): closed.
- TamaMCP #9 (task subscriptions): closed.
- Tama Link #5: closed; its local closure gates now pass.
- Tama #123 (application migration): open.
- Tama Link #8 (conformance and live Tama acceptance): open.

Findings 6 and 7 are resolved, so no local Issue #5 acceptance finding remains.
Delivery still requires committing and pushing the branch, opening a pull
request, and verifying exact-head CI and review state. Live migrated-Tama
acceptance remains an external gate and must not be inferred from fixture,
static, or local unit-test results.
