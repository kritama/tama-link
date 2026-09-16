# Phase 2.3 Re-review — Issue #6

Status: changes required; issue #6 is not ready to close

Review date: 2026-09-16

Reviewed branch: `feature/phase-2-current-tama-adapters`

Reviewed head: `741ab11`

Reviewed remediation commit:

- `741ab11` — remediate the Phase 2.2 review findings for the issue #6 slice.

At review time the local branch was three commits ahead of
`origin/feature/phase-2-current-tama-adapters`; the remote feature branch still
pointed to `f6dfd7e2cbbf7e69ed30829994b5bf1fcf79859d`. This review therefore applies
to the local remediation commit whether or not it is subsequently pushed.

Tracking issues:

- [#6 — Phase 2: implement synchronous replayable System execution](https://github.com/kritama/tama-link/issues/6)
- [#8 — TamaMCP conformance and live Tama acceptance](https://github.com/kritama/tama-link/issues/8)
- [#9 — Phase 2 tracker](https://github.com/kritama/tama-link/issues/9)

## Decision

The remediation correctly resolves most Phase 2.2 findings. Task-backed work
is now rejected before durable acceptance, production wiring uses the canonical
database and credential namespaces, guarded and unsupported strategies have
no-trace rejection coverage, and synchronous task-shaped results use the
distinct `ErrUnexpectedTaskResult` path. The recurring durable sweep is a
reasonable design for recovering prompt-dispatch queue overflow.

Issue #6 is nevertheless not ready to close. The new schema boundary validates
keyword names but not the shapes and constraints of supported keyword values,
allowing malformed schemas to disable intended restrictions. Its runtime
implementation also diverges from standard JSON Schema string-length and
number semantics. The new worker saturation regression completes all work
through startup recovery before attempting to saturate the dispatch queue, so
it does not test the condition it claims to cover. Finally, the credential
probe was changed from the specified short fixed fail-fast window to a
two-minute default with an arbitrary environment override.

No implementation or GitHub changes were made during this re-review.

## Confirmed remediations

### Strategy gate before durable acceptance

`internal/application.Submit` now gates `upstream_task`, `local_guarded`, and
`unsupported` strategies before `CreateSubmission`. Rejected work therefore
creates no submission row, claims no idempotency key, and makes no upstream
request. `TestSubmitRejectionsLeaveNoTrace` covers the durable and upstream
side-effect boundary.

### Canonical profile state layout

`cmd/tama-link.buildApp` now derives its database path and credential namespace
through `profile.DatabasePath` and `profile.CredentialNamespace` via
`stateLayout`. `TestBuildAppKeepsProfilesIsolated` constructs two application
instances whose profiles share the same state reference names and demonstrates
distinct database files, credential prefixes, and operation catalogs.

### Distinct unexpected-task classification

`internal/adapter/tama2026.Connection.ExecuteLocal` now wraps
`ErrUnexpectedTaskResult` rather than `ErrCatalogMismatch`. Direct adapter and
application tests pin both the sentinel and the dedicated stable
`operation_contract_mismatch` message.

### Guarded and unsupported fixtures

The application fixture now contains task-backed, guarded, and unsupported
descriptors in addition to the replayable System operation. The rejection
suite covers every non-replayable strategy and verifies the no-trace contract.

### Durable sweep design

The worker service now periodically re-derives runnable `local_replayable`
submissions from SQLite and offers them to the prompt-start queue. SQLite
leases remain the final cross-process single-winner guard. This addresses the
architectural cause of silently dropped queue entries, but the regression test
does not yet prove the behavior, as described below.

## Priority 1 findings

### P1. Supported JSON Schema keywords can still fail open

Locations:

- `internal/catalog/validate.go`, `CheckSchemaVocabulary`, lines 46–93; and
- `internal/catalog/descriptor.go`, `checkSchema`, lines 216–246.

`CheckSchemaVocabulary` verifies that every member name is in the allowed
vocabulary and recurses through the schema-bearing keywords. It does not
validate the required type or value restrictions of those members.

For JSON `null`, unmarshalling into a map, slice, or pointer often succeeds and
produces a nil value. Consequently, profiles accept malformed schemas such as:

```json
{"type":"object","properties":null}
{"type":"object","properties":{"name":null}}
{"type":"object","required":null}
{"type":"string","minLength":null}
{"type":"number","minimum":null}
{"type":"array","items":null}
{"enum":null}
```

Several of these values are then treated as if the constraint were absent:
`properties: null` and a null property schema do not restrict the member,
`required: null` requires nothing, `minLength: null` and `minimum: null` are
ignored, `items: null` accepts every array item, and `enum: null` performs no
enum check. Negative length or item-count limits and an empty enum similarly
need explicit schema-validity decisions.

This contradicts the remediation claim that unsupported or unenforced schema
behavior cannot reach runtime. The profile must validate both the supported
vocabulary and the meta-shape of every supported keyword before accepting the
descriptor.

Required change:

- validate every supported keyword's JSON type and legal range at profile
  load;
- reject null or malformed schema-bearing children rather than interpreting
  them as empty schemas;
- validate nested property, item, and `additionalProperties` schemas through
  the same rules;
- reject invalid enum, required, length, item-count, and numeric-bound forms;
  and
- add fail-closed fixtures for null, wrong-type, negative, empty, duplicate,
  and recursively malformed values as applicable to the selected schema
  dialect.

### P1. The queue-saturation test completes every submission before dispatch

Location: `internal/worker/service_test.go`,
`TestSaturatedQueueStillExecutesEverySubmission`, lines 20–46.

The test creates all 300 runnable submissions and then calls `svc.Start`.
`Start` invokes `Runner.Recover` synchronously, which lists and executes every
pending replayable submission. Only after that recovery call returns does the
test call `Dispatch` 300 times.

The dispatched IDs therefore refer to submissions that are already terminal.
The executor count and completed-state assertions are satisfied entirely by
startup recovery; the in-memory queue need not saturate, no dispatch needs to
be dropped, and the recurring sweep need not rediscover anything.

Required change:

- arrange for the tested submissions to be accepted after startup recovery,
  or use a controlled worker/state seam that can deterministically fill the
  prompt queue;
- prove that at least one prompt dispatch was not accepted by the saturated
  queue;
- prove that the dropped submission remains durably runnable; and
- prove that a later recurring sweep, without a process restart, schedules it
  and reaches the expected terminal state.

## Priority 2 findings

### P2. Supported scalar validation diverges from JSON Schema semantics

Locations:

- `internal/catalog/validate.go`, `validateString`, lines 305–325; and
- `internal/catalog/validate.go`, `isJSONNumber`, lines 564–588.

`minLength` and `maxLength` compare Go's `len(string)`, which counts UTF-8
bytes. JSON Schema string lengths count Unicode code points. For example,
`"é"` contains one code point but two UTF-8 bytes, so the current validator:

- incorrectly accepts it against `minLength: 2`; and
- incorrectly rejects it against `maxLength: 1`.

The number recognizer accepts only an optional sign, decimal digits, and one
decimal point. It rejects valid JSON exponent forms such as `1e3`, and the
exact bound parser has the same restriction. A profile may therefore pass
vocabulary validation but reject valid inputs or bounds from the supported
`number`, `integer`, `minimum`, and `maximum` vocabulary.

Required change:

- count Unicode code points for string-length assertions;
- parse the complete JSON number grammar, including exponent notation;
- normalize exponent-form values into the exact decimal comparison without
  routing through `float64`; and
- add multibyte string and exponent-form input and bound regressions.

The chosen `pattern` dialect should also be documented and validated at
profile load. Go's RE2 syntax is not identical to the ECMAScript-compatible
regular-expression behavior normally associated with JSON Schema.

### P2. The credential availability probe no longer has a short fixed bound

Location: `internal/credential/credential.go`, lines 48–71 and 95–123.

The specification requires a complete set/read/remove probe to finish within
a short fixed window. A backend that waits on interactive input is considered
unavailable, and `serve` must fail fast rather than wait for a keyring prompt.

The remediation changes the previous short bound to a two-minute production
default and accepts any positive duration through
`TAMA_LINK_KEYRING_PROBE_TIMEOUT`. This intentionally gives a human time to
answer an unlock prompt, which is the opposite of the documented headless and
non-interactive startup contract. A typo or overly large configured duration
can delay MCP startup far beyond the intended bound.

The hermetic STDIO test improvement does not require changing production
semantics: the test can inject or arrange an unavailable backend while the
runtime retains a reviewed fixed maximum.

Required change:

- restore a short, fixed production probe bound;
- keep test timing injectable through a test-only seam rather than an
  effectively unbounded production environment variable; and
- preserve the clear unavailable error without treating an interactive unlock
  prompt as a supported serve-startup path.

## Independent edge-test evidence

An isolated review test was overlaid into `internal/catalog` without modifying
the repository. It asserted fail-closed behavior for the malformed schemas
listed above and standard semantics for a multibyte string and exponent-form
number. The test failed with:

- every malformed keyword-value schema accepted;
- `minLength: 2` accepting the one-code-point string `"é"`;
- `maxLength: 1` rejecting that same string; and
- `type: number` rejecting the valid JSON number `1e3`.

The temporary overlay was removed after the diagnostic run.

## Validation evidence

The following completed successfully on exact head `741ab11`:

- focused remediation tests, repeated five times, for catalog validation,
  application rejection, unexpected task results, worker saturation, and
  production profile wiring;
- `make check`:
  - `go test ./...`;
  - `go test -race ./...`;
  - `go vet ./...`;
  - `golangci-lint run ./...` with zero issues; and
  - the trimmed host binary build;
- uncached `go test -count=1 ./...`;
- uncached `go test -race -count=1 ./...`; and
- `git diff --check 6d31f07..HEAD`.

The worktree remained clean after validation. The green repository gate does
not detect the schema edge cases or the ineffective saturation-test setup.

## Re-review requirements

Before issue #6 is closed:

1. Validate the meta-shape and legal values of every supported schema keyword
   recursively and add fail-closed malformed-schema fixtures.
2. Correct Unicode string-length and exponent-form number behavior within the
   supported schema vocabulary.
3. Replace the saturation regression with one that demonstrably drops at least
   one prompt dispatch and proves sweep-based recovery without startup recovery
   or a process restart.
4. Restore the credential probe's short fixed fail-fast production bound.
5. Run `make check`, the uncached unit and race suites, and
   `git diff --check` on the exact remediation head.
6. Keep issue #6 open pending re-review; complete migrated-Tama live acceptance
   separately under issue #8.

## Remediation (this phase)

Status: remediated in this worktree; pending re-review.

1. **Schema meta-shape validation** — `CheckSchemaVocabulary` now validates
   the JSON type and legal value of every supported keyword, recursively:
   null members are rejected instead of reading as absent; `type` names come
   from the closed set without duplicates; `required` names are unique,
   non-empty, and declared when `properties` is present; `enum` is
   non-empty; count constraints are non-negative integers within a reviewed
   bound (1e6); `minimum`/`maximum` accept the full JSON number grammar;
   `pattern` must be non-empty and compile; annotation keywords are typed;
   and inverted bound pairs are rejected. The pattern dialect (Go RE2) is
   documented in code and specification. Fail-closed fixtures cover null,
   wrong-type, negative, empty, duplicate, inverted, huge, and recursively
   malformed values (`TestCheckSchemaVocabularyRejectsMalformedKeywordValues`).
2. **Standard scalar semantics** — `minLength`/`maxLength` now count Unicode
   code points; `isJSONNumber` accepts the complete JSON number grammar
   including exponent form; `normalizeNumber` applies the exponent by
   shifting the decimal point with zero padding so every comparison, bound,
   and `type: integer` check runs on exact normalized decimal digits, never
   `float64`. Regressions: multibyte string lengths and exponent-form
   values and bounds (`TestValidateAgainstSchemaStringLengthCountsCodePoints`,
   `TestValidateAgainstSchemaNumbersExponentForm`).
3. **Effective saturation regression** — the test now starts the service
   before any submission exists (startup recovery is a no-op), accepts 512
   submissions (twice the queue depth), burst-dispatches them, and asserts
   through a new `DroppedDispatches` counter that at least one prompt
   dispatch was actually dropped. While a gate holds every prompt-started
   execution, it waits until the executor has observed all 512 distinct IDs;
   because Dispatch accepted fewer than 512 and the in-flight guard absorbs
   duplicates, only the recurring durable sweep could have delivered the rest.
   The test then proves every submission reaches completed without a
   restart. Writing this test exposed a real bug: the sweep re-offered
   in-flight (running) IDs that head-of-line-blocked the queue and starved
   the dropped IDs; the sweep now skips IDs this process is already
   executing, and the requirement is documented in the specification.
4. **Fixed probe bound** — the credential availability probe is back to a
   short (5s) fixed bound compiled into the binary; the environment
   override is removed. Test timing uses a same-package seam, and the
   unavailable error states that an interactive unlock prompt is not a
   supported serve-startup path. The specification says the window cannot be
   extended at runtime.
5. `make check`, uncached `go test -count=1 ./...` and
   `go test -race -count=1 ./...`, and `git diff --check` are green on the
   remediation head.

Issue #6 remains open pending re-review; migrated-Tama live acceptance stays
under issue #8.
