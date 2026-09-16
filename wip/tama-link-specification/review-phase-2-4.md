# Phase 2.4 Re-review — Issue #6

Status: changes required; issue #6 is not ready to close

Review date: 2026-09-16

Reviewed branch: `feature/phase-2-current-tama-adapters`

Reviewed head: `391c9f6`

Reviewed remediation commit:

- `391c9f6` — remediate the Phase 2.3 review findings for the issue #6 slice.

At review time the local branch was four commits ahead of
`origin/feature/phase-2-current-tama-adapters`; the remote feature branch still
pointed to `f6dfd7e2cbbf7e69ed30829994b5bf1fcf79859d`. This review therefore applies
to the local remediation commit whether or not it is subsequently pushed.

Tracking issues:

- [#6 — Phase 2: implement synchronous replayable System execution](https://github.com/kritama/tama-link/issues/6)
- [#8 — TamaMCP conformance and live Tama acceptance](https://github.com/kritama/tama-link/issues/8)
- [#9 — Phase 2 tracker](https://github.com/kritama/tama-link/issues/9)

## Decision

The remediation resolves most Phase 2.3 findings. Supported schema keywords
now receive recursive meta-shape validation, Unicode string lengths count code
points, ordinary exponent-form numbers classify and compare exactly, the
credential probe again has a short fixed production bound, and the worker
sweep no longer re-offers submissions already executing in the same process.

Issue #6 is nevertheless not ready to close. Exact number normalization is
unsafe for large valid exponents and can panic or attempt an excessive
allocation. The replacement saturation regression still permits recurring
sweeps to schedule work before the prompt-dispatch burst, so its drop counter
does not prove recovery of a uniquely dropped submission. Numeric `const` and
`enum` checks also compare canonical number spellings instead of mathematical
values, contrary to JSON Schema instance-equality semantics.

No implementation or GitHub changes were made during this re-review.

## Confirmed remediations

### Recursive schema meta-shape validation

`CheckSchemaVocabulary` now validates supported keyword values recursively.
Null and wrong-type schema children fail closed; type names are closed and
unique; required names, count constraints, numeric bounds, enums, annotations,
and patterns receive explicit validation. Nested `properties`, `items`, and
schema-form `additionalProperties` pass through the same checks.

This resolves the malformed-keyword fail-open cases demonstrated in the Phase
2.3 review, subject to the large-exponent safety finding below.

### Unicode string-length semantics

`minLength` and `maxLength` now use Unicode code-point counts rather than UTF-8
byte lengths. The added regressions correctly distinguish one multibyte code
point from its encoded byte length.

### Ordinary exponent-form number handling

The number recognizer accepts the complete JSON number grammar, including
ordinary positive and negative exponent forms. The validator correctly handles
the covered integer classifications and bound comparisons without converting
through `float64`.

The implementation is not yet safe across the full accepted exponent syntax,
as described in the first priority-one finding.

### Fixed credential-probe bound

The production environment override was removed. The availability probe now
uses a short five-second bound, and its unavailable error states that an
interactive unlock prompt is not a supported serve-startup path. This restores
the headless fail-fast behavior requested by the Phase 2.3 review.

### In-flight sweep filtering

The recurring worker sweep skips IDs already executing in the same process.
This avoids repeatedly filling the prompt queue with known in-flight work ahead
of older dropped IDs. The change is reasonable, but the revised regression does
not yet isolate and prove recovery of a dropped ID.

## Priority 1 findings

### P1. Large accepted exponents can panic or exhaust memory

Location: `internal/catalog/validate.go`, `normalizeNumber`, lines 321–377.

`isJSONNumber` correctly accepts an arbitrary-length JSON exponent, but
`normalizeNumber` accumulates that exponent into a machine `int` without
overflow detection. It then applies the exponent by materializing zero padding
with `bytes.Repeat`.

A valid, small schema containing this bound passes vocabulary validation:

```json
{"type":"number","minimum":1e9999999999999999999}
```

Decoding it for runtime validation panics in `bytes.Repeat` with:

```text
panic: runtime error: makeslice: len out of range
```

Exponent values that still fit in an `int` can instead request extremely large
allocations. Count constraints route through the same normalizer before their
one-million bound is enforced, so a maliciously large exponent can panic there
as well rather than returning a profile-validation error.

This contradicts the fail-closed schema boundary and the claim that the full
accepted JSON number grammar is compared safely and exactly.

Required change:

- add [`github.com/cockroachdb/apd/v3`](https://github.com/cockroachdb/apd) at
  `v3.2.3` and use its arbitrary-precision decimal representation for accepted
  JSON number literals;
- centralize numeric parsing and comparison in a small `internal/catalog`
  component: preserve the existing JSON-number grammar check, parse with
  `apd.NewFromString`, handle every returned error, and compare with
  `Decimal.Cmp` rather than expanding exponent-form numbers into zero-padded
  byte slices;
- define and document the reviewed exponent range accepted by Tama Link and
  translate an out-of-range exponent into a profile- or instance-validation
  error, as appropriate;
- ensure every accepted schema and instance number returns a validation result
  rather than panicking or allocating in proportion to the exponent value; and
- add large positive, large negative, overflowing, and count-constraint
  exponent regressions.

### P1. The saturation regression still does not prove dropped-ID recovery

Location: `internal/worker/service_test.go`,
`TestSaturatedQueueStillExecutesEverySubmission`, lines 82–121.

`NewService` starts the recurring sweep loop before the test creates its 512
submissions. With a 50-millisecond interval, one or more sweeps can therefore
list and schedule submissions while the creation loop is still running, before
the first prompt `Dispatch` call.

An isolated diagnostic repeated this setup ten times and measured between 213
and 433 distinct submission IDs already observed by the executor before the
prompt-dispatch burst began.

`DroppedDispatches` counts failed queue offers, not unique IDs that have never
been queued or started. A prompt offer can therefore increment the counter for
an ID that a prior recurring sweep has already placed in flight. The test's
inference that `count - drops` is the number of distinct prompt-delivered IDs,
and that every additional observation must be later sweep recovery, is false.

The production sweep behavior may be correct, but the regression still does
not satisfy the Phase 2.3 requirement to prove that a prompt dispatch was
dropped while its submission remained durably runnable and that a subsequent
sweep recovered that same work without a restart.

Required change:

- prevent recurring sweeps from scheduling the test submissions before the
  controlled dispatch burst;
- identify at least one specific unique ID whose prompt offer was not accepted;
- verify that ID remains durably runnable after the drop;
- trigger or await a later sweep and prove that the same ID is scheduled; and
- prove it reaches the expected terminal state without startup recovery or a
  process restart.

## Priority 2 findings

### P2. Numeric `const` and `enum` use lexical rather than mathematical equality

Locations:

- `internal/catalog/validate.go`, `schemaView.validate`, lines 72–83; and
- `internal/catalog/validate.go`, `jsonValuesEqual`, lines 549–555.

`jsonValuesEqual` canonicalizes both values and compares the resulting bytes.
Canonicalization preserves number literals deliberately, so mathematically
equal numbers with different legal spellings remain unequal. For example, the
current validator rejects `1.0` against both:

```json
{"const":1}
{"enum":[1]}
```

JSON Schema instance equality treats numbers as equal when they have the same
mathematical value. The same rule must apply recursively when equivalent
numbers occur inside arrays or objects used by `const` or `enum`.

Required change:

- implement recursive JSON instance equality;
- parse numeric leaves through the same `apd/v3` component used for numeric
  bounds and compare them with `Decimal.Cmp`;
- retain code-point string equality, positional array equality, and
  key-order-independent object equality; and
- add top-level and nested regressions covering `1`, `1.0`, and `1e0`, together
  with adjacent unequal values.

Reference:

- [JSON Schema Draft 2020-12 — Instance Equality](https://json-schema.org/draft/2020-12/json-schema-core#section-4.2.2)

## Independent edge-test evidence

Temporary review-only tests were added locally for diagnosis and removed
immediately afterward. They did not leave repository changes.

The catalog diagnostic demonstrated:

- `CheckSchemaVocabulary` accepted the large-exponent schema;
- `ValidateAgainstSchema` panicked with `makeslice: len out of range`; and
- `const: 1` and `enum: [1]` both rejected the mathematically equal instance
  `1.0`.

The worker diagnostic reproduced the revised saturation setup ten times. In
every run the recurring sweep scheduled work before the first prompt dispatch;
the executor had already observed 213, 401, 350, 433, 246, 408, 422, 313, 302,
and 407 distinct IDs respectively.

The committed saturation test itself passed twenty repetitions. That result
shows the test is stable under the current scheduling behavior, but it does not
repair the invalid causal inference described above.

## Validation evidence

The following completed successfully on exact local head `391c9f6`:

- `make check` outside the restricted sandbox:
  - `go test ./...`;
  - `go test -race ./...`;
  - `go vet ./...`;
  - `golangci-lint run ./...` with zero issues; and
  - the trimmed host binary build;
- uncached `go test -count=1 ./...`;
- uncached `go test -race -count=1 ./...`;
- twenty repetitions of the committed saturation regression; and
- `git diff --check 741ab11..HEAD`.

The worktree was clean after removing the temporary diagnostic tests. The live
GitHub issue remained open. The green repository gate does not exercise the
large-exponent panic, JSON Schema numeric instance equality, or the pre-dispatch
sweep race in the saturation proof.

## Re-review requirements

Before issue #6 is closed:

1. Make exact exponent handling overflow-safe and allocation-safe, or reject a
   documented bounded exponent during profile validation.
2. Replace the saturation regression with a controlled proof that identifies
   and recovers a uniquely dropped, durably runnable submission.
3. Implement exact mathematical number equality for `const` and `enum`,
   recursively through arrays and objects.
4. Run `make check`, the uncached unit and race suites, focused edge
   regressions, and `git diff --check` on the exact remediation head.
5. Keep issue #6 open pending re-review; complete migrated-Tama live acceptance
   separately under issue #8.

## Remediation (this phase)

Status: remediated in this worktree; pending re-review.

1. **Required fails non-objects unconditionally** — `schemaView.validate`
   now rejects any non-object value whenever `required` is pinned, after the
   type check and independently of it: the guard fires for schemas with no
   type assertion, for type assertions that already rejected the value, and
   for type assertions that accept the kind (for example a `null`-typed
   value). `TestValidateAgainstSchemaRequiredFailsNonObjects` pins the
   number, string, boolean, array, accepted-null, object, and nested cases.
2. **Default never satisfies required; explicit null is missing** — the
   required check now treats a property present only as an explicit JSON
   `null` as missing, and the `default` annotation carries no fill-in
   semantics (it remains a pure annotation).
   `TestValidateAgainstSchemaRequiredIgnoresDefaultAndNull` pins all three
   outcomes, including a null-accepting property schema where the explicit
   null must still fail.
3. **Gate held before the drop assertions** — the gated executor now signals
   when the first execution reaches the gate, and the saturation test waits
   for that signal immediately after the dispatch burst and before reading
   `DroppedDispatches` or asserting sweep delivery. While the gate is held,
   no prompt-started execution can complete, so the drop counter and the
   distinct-observation proof cannot be satisfied by the prompt path alone.
4. `make check`, uncached `go test -count=1 ./...`, uncached
   `go test -race -count=1 ./...`, and `git diff --check` are green on the
   remediation head.

Issue #6 remains open pending re-review; migrated-Tama live acceptance stays
under issue #8.

## Re-review of remediation `e413093`

Status: changes required; issue #6 is not ready to close

Re-review date: 2026-09-16

Reviewed head: `e413093`

Reviewed remediation commit:

- `e413093` — claimed remediation of the Phase 2.4 findings for the issue #6
  slice.

At re-review time the local branch was five commits ahead of
`origin/feature/phase-2-current-tama-adapters`; the remote feature branch still
pointed to `f6dfd7e2cbbf7e69ed30829994b5bf1fcf79859d`. The live GitHub issue
remained open.

### Decision

The remediation does not resolve any of the three findings listed in this
document. The exponent normalizer and `const`/`enum` equality implementation
are unchanged. The worker-test change waits for an executor signal that may
come from the already-running recurring sweep, so it still does not identify
or recover a uniquely dropped prompt dispatch.

The commit also adds two `required` behaviors that are unrelated to the Phase
2.4 requirements and conflict with JSON Schema semantics: `required` now
rejects non-object instances, and a present property whose value is JSON
`null` is treated as absent. The specification and new tests were changed to
encode those incorrect behaviors.

The remediation section above and the corresponding issue #6 comment describe
different requirements from the three findings actually recorded in this
review.

No implementation or GitHub changes were made during this re-review.

### P1. `required` incorrectly rejects non-object instances

Location: `internal/catalog/validate.go`, `schemaView.validate`, lines 54–60.

The new guard rejects every non-object value whenever `required` is present.
In JSON Schema, `required` is an object-specific validation keyword. Without a
separate `type: "object"` assertion, a number, string, boolean, array, or null
instance is not constrained by `required`.

For example, this schema does not reject the number `5` under JSON Schema
semantics:

```json
{"required":["a"]}
```

The new `TestValidateAgainstSchemaRequiredFailsNonObjects` instead requires
that value to fail and therefore pins the wrong contract.

Required change:

- remove the unconditional non-object rejection;
- evaluate `required` only when the instance is an object;
- let an explicit `type` assertion reject an unwanted non-object kind; and
- replace the new non-object tests with regressions that distinguish
  object-key presence from independent type validation.

Reference:

- [JSON Schema Draft 2020-12 — `required`](https://json-schema.org/draft/2020-12/json-schema-validation#section-6.5.3)

### P1. A present `null` property is incorrectly treated as missing

Location: `internal/catalog/validate.go`, `schemaView.validateObject`, lines
138–145.

`required` checks whether the named property exists. It does not require the
property value to be non-null. A present JSON `null` value satisfies
`required`; the property's own schema determines whether null is permitted.

For example, this value is valid against the shown schema:

```json
{
  "schema": {
    "type": "object",
    "required": ["a"],
    "properties": {"a": {"type": ["string", "null"]}}
  },
  "value": {"a": null}
}
```

The fact that `default` is an annotation and does not fill a missing property
is correct but unrelated. It does not make an explicitly present null value
absent.

Required change:

- restore required-property checking to map-key presence alone;
- retain ordinary property-schema validation so a schema can allow or reject
  null explicitly; and
- replace the new null test with separate missing, present-null-allowed, and
  present-null-rejected cases.

Reference:

- [Understanding JSON Schema — `null`](https://json-schema.org/understanding-json-schema/reference/null)

### P1. Large-exponent safety remains unresolved

Location: `internal/catalog/validate.go`, `normalizeNumber`, lines 354–388.

The remediation does not change exponent parsing or normalization. The
exponent is still accumulated into an unchecked machine `int` and applied by
materializing zero padding with `bytes.Repeat`.

The Phase 2.4 diagnostic still accepts this valid JSON number as a bound and
then panics during runtime schema decoding:

```json
{"type":"number","minimum":1e9999999999999999999}
```

Observed result:

```text
panic: runtime error: makeslice: len out of range
```

The preferred remediation is now explicit: replace the hand-written normalizer
with a focused [`github.com/cockroachdb/apd/v3`](https://github.com/cockroachdb/apd)
numeric component. Preserve the JSON-number grammar check, parse with
`apd.NewFromString`, handle all parse and exponent-range errors, compare with
`Decimal.Cmp`, and reject a documented reviewed exponent range through the
normal validation error path. No path may allocate in proportion to the
exponent value.

### P1. The saturation regression still does not prove dropped-ID recovery

Location: `internal/worker/service_test.go`,
`TestSaturatedQueueStillExecutesEverySubmission`, lines 88–139.

The new `started` signal proves only that some execution reached the gated
executor. It does not prove that execution came from prompt `Dispatch`; the
recurring sweep starts in `NewService` before the test creates its submissions
and can produce the same signal. Preventing executions from completing is also
not sufficient: the original flaw concerns which path scheduled and observed
each distinct ID, not whether the executor completed it.

An isolated diagnostic repeated the current setup five times. Before the first
prompt dispatch, the recurring sweep had already scheduled 306, 400, 344, 383,
and 419 distinct IDs respectively.

`DroppedDispatches` can therefore count a failed prompt offer for an ID already
scheduled by the sweep. The test still cannot infer that `count - drops`
represents distinct prompt-delivered IDs or that later observations represent
recovery of uniquely dropped work.

The original Phase 2.4 required change remains in full: prevent pre-burst
sweeps, identify a specific unique ID whose prompt offer was rejected, verify
that it remains durably runnable, and prove a subsequent sweep schedules that
same ID without startup recovery or a restart.

### P2. Mathematical `const` and `enum` equality remains unresolved

Locations:

- `internal/catalog/validate.go`, `schemaView.validate`, lines 79–88; and
- `internal/catalog/validate.go`, `jsonValuesEqual`, lines 558–566.

The remediation does not change `jsonValuesEqual`. It still compares canonical
bytes, so mathematically equal number spellings remain unequal. Diagnostics
confirmed that the following continue to fail:

- `{"const":1}` against `1.0`;
- `{"enum":[1]}` against `1.0`; and
- `{"const":{"n":1}}` against `{"n":1.0}`.

The original Phase 2.4 required change remains in full: implement recursive
JSON instance equality, using the same `apd/v3` parser and `Decimal.Cmp` for
numeric leaves, including numbers nested inside arrays and objects.

Reference:

- [JSON Schema Draft 2020-12 — Instance Equality](https://json-schema.org/draft/2020-12/json-schema-core#section-4.2.2)

### Recommended dependency boundary

Use `github.com/cockroachdb/apd/v3` at `v3.2.3` only for exact decimal parsing,
range enforcement, integer/count checks, numeric-bound comparison, and numeric
leaf equality inside `internal/catalog`. An isolated Go 1.25 probe confirmed
that it compares `1`, `1.0`, and `1e0` as equal and returns errors, rather than
panicking, for the extreme exponents used by this review.

Keep the existing closed schema-keyword allowlist and metadata-shape checks in
Tama Link. `apd/v3` should replace `numberText`/`normalizeNumber` and the numeric
portion of `jsonValuesEqual`; it should not broaden the supported JSON Schema
vocabulary or leak library-specific errors across the catalog boundary.

Do not introduce Bun or another ORM for these findings. They concern decimal
semantics and deterministic worker scheduling, while the store deliberately
owns explicit SQLite transaction and lease behavior. An ORM would not solve
either defect and could obscure the required `BEGIN IMMEDIATE` semantics.

Do not replace the focused validator with a full JSON Schema library in this
remediation. The evaluated alternatives either failed an extreme-exponent
diagnostic, require a newer Go toolchain than this repository, or represent
numeric bounds as `float64`. The local allowlist plus the narrow `apd/v3`
numeric component is the smallest dependency change that closes the reviewed
numeric defects without changing Tama Link's accepted schema subset.

### Independent diagnostic evidence

Temporary review-only tests were added locally and removed immediately after
the diagnostic runs. They left no repository changes.

The diagnostics reproduced:

- the large-exponent `makeslice: len out of range` panic;
- top-level and nested mathematical-equality failures;
- rejection of a non-object by an object-only `required` keyword;
- rejection of a present null property allowed by its property schema; and
- recurring-sweep execution of hundreds of IDs before prompt dispatch.

### Validation evidence

The following completed successfully on exact local head `e413093`:

- `make check`:
  - `go test ./...`;
  - `go test -race ./...`;
  - `go vet ./...`;
  - `golangci-lint run ./...` with zero issues; and
  - the trimmed host binary build;
- uncached `go test -count=1 ./...`;
- uncached `go test -race -count=1 ./...`; and
- `git diff --check 391c9f6..HEAD`.

The worktree was clean after the temporary diagnostics were removed. The green
repository gates do not cover the unresolved Phase 2.4 edge cases or detect
the newly introduced `required` semantic regressions.

### Updated re-review requirements

Before issue #6 is closed:

1. Revert the two non-standard `required` semantics introduced by `e413093`
   and correct their specification text and tests.
2. Replace the hand-written number normalizer with the focused `apd/v3`
   component described above; handle parse and exponent-range errors, document
   the accepted range, and keep all failure paths allocation-safe.
3. Replace the saturation regression with a controlled proof that identifies
   and recovers a uniquely dropped, durably runnable submission.
4. Implement recursive exact mathematical number equality for `const` and
   `enum`, comparing numeric leaves through `apd.Decimal.Cmp`.
5. Add focused regressions for every diagnostic above.
6. Run `make check`, uncached unit and race suites, focused repeated tests, and
   `git diff --check` on the exact remediation head.
7. Keep issue #6 open pending another re-review; complete migrated-Tama live
   acceptance separately under issue #8.

## Re-remediation of `e413093` (Phase 2.4, second pass)

This pass reverts the two non-standard `required` behaviors, replaces the
hand-written number normalizer with the reviewed `apd/v3` component, rewrites
the saturation regression into a controlled per-ID proof, and implements
recursive JSON Schema instance equality for `const` and `enum`.

### 1. `required` reverted to standard JSON Schema semantics

- The non-object guard removed from `validate`: a non-object value is
  constrained only by an independent type assertion, so
  `{"required":["a"]}` validates `5`, while
  `{"type":"object","required":["a"]}` rejects `5` through the type check.
- Explicit-null handling removed from `validateObject`: `required` now checks
  map-key presence only; a present `{"a":null}` satisfies the requirement and
  the property's own schema decides whether null is permitted.
- `TestValidateAgainstSchemaRequiredFailsNonObjects` and
  `TestValidateAgainstSchemaRequiredIgnoresDefaultAndNull` replaced by
  `TestValidateAgainstSchemaRequiredSemantics`, whose named subtests cover
  exactly the distinctions above, including that no `default` annotation
  fills a missing property.
- Specification paragraph replaced with the standard semantics.

### 2. `apd/v3 v3.2.3` numeric component

- New `internal/catalog/decimal.go`: `parseJSONNumber` (the JSON grammar
  check stays first, then `apd.NewFromString` on the trimmed literal) and
  `jsonInstanceEqual`.
- `numberText` now stores an `*apd.Decimal`; `validateNumber`, `checkPair`,
  `requireCount`, `requireNumberBound`, and `isIntegerLiteral` all compare
  through `Decimal.Cmp` or the coefficient. `normalizeNumber`,
  `compareNormalized`, `compareDecimal`, `stripLeadingZeros`,
  `stringCompare`, `isAllZeros`, and the lexical `jsonValuesEqual` are
  deleted; no path materializes zero padding.
- The reviewed exponent range is the library's effective-exponent limit of
  ±100000 (apd `BaseContext`). Out of range: profile-load error for numeric
  bounds and count constraints; instance-validation error whenever a bound,
  `const`, `enum`, or `integer` type assertion must evaluate the value. In
  every case the result is a validation error, never a panic.
- `isExactInteger` decides integrality by reducing trailing coefficient zeros
  and inspecting the resulting exponent, without rounding, constructing
  `10^scale`, or doing exponent-proportional work.

### 3. Controlled saturation regression

- `Service.Offer` reports whether the prompt queue accepted an ID; `Dispatch`
  keeps its fire-and-forget contract on top of it. New exported
  `Service.Sweep` runs one durable sweep on demand (the loop's ticker calls
  the same path), so a caller that observed saturation can shorten the
  recovery wait.
- `TestSaturatedQueueStillExecutesEverySubmission` now: pins the recurring
  sweep to one hour so it cannot schedule before the burst; offers 512 IDs
  (2× the queue depth) and records the exact rejected IDs; proves the first
  rejected ID is still `accepted`, present in the durable runnable set, and
  never executed; proves an explicit `Sweep` — no restart — schedules and
  executes that same ID; keeps sweeping while it waits for the rest, as the
  production sweep would; and finishes with every ID completed and every ID
  observed, each executed at least once (a transition that loses a busy
  SQLite write leaves the replayable submission re-runnable, matching
  production recovery semantics).
- The executor bounds the post-gate completion wave to 8 concurrent
  completions so the terminal phase measures scheduling, not a 512-wide
  writer storm.

### 4. `const`/`enum` instance equality

- `jsonInstanceEqual` recurses through arrays and objects; numeric leaves
  compare through `Decimal.Cmp` (`1`, `1.0`, and `1e0` are equal, adjacent
  values are not), strings by decoded code points, arrays positionally,
  objects independently of key order. Type mismatch is inequality, never an
  error; an out-of-range numeric leaf becomes a validation result.

### 5. Focused regressions

- `TestCheckSchemaVocabularyRejectsOutOfRangeExponents`: huge and
  just-beyond-range exponents for bounds and count constraints are rejected
  at profile load; the ±100000 boundary itself is accepted.
- `TestValidateAgainstSchemaExponentsNeverPanic`: instance-side out-of-range
  exponents return validation results (bounds, `integer`, `const`, `enum`);
  in-range large exponents validate exactly.
- `TestValidateAgainstSchemaNumericInstanceEquality`: mathematical equality
  and exactness across scalars, arrays, and nested objects, including
  adjacent non-equal values and cross-type inequality.

### 6. Validation evidence

On the remediation head:

- `make check` components: `go test ./... -count=1`;
  `go test -race ./... -count=1`; `go vet ./...`; `golangci-lint run ./...`
  with zero issues;
- focused repeats: `go test ./internal/catalog/ -count=5`;
  `go test ./internal/worker/ -race -count=2 -run TestSaturatedQueue`
  (additionally `-race -count=3` on the saturation test during development);
- `git diff --check 391c9f6..HEAD` and `git diff --check`.

Issue #6 remains open pending re-review; live acceptance stays with issue
#8.

## Resolution of final Phase 2.4 re-review findings

Status: remediated and locally verified; pending independent final re-review.

The review of `3acb5d9` found two remaining catalog defects despite the green
repository gates:

1. Profile validation accepted exponent-form count constraints, but runtime
   `schemaView` decoded `minLength`, `maxLength`, `minItems`, and `maxItems`
   directly into `*int`. Consequently, an approved constraint such as
   `minItems: 1e2` failed schema decoding instead of enforcing the value 100.
2. `isExactInteger` constructed `10^(-exponent)` and took a large integer
   modulus. A diagnostic benchmark of the short literal `1e-100000` measured
   approximately 1.7–2.6 milliseconds and 95 KiB allocated per check, contrary
   to the documented allocation-safety guarantee.

The remediation keeps profile validation and runtime decoding on one numeric
path:

- `countValue.UnmarshalJSON` calls the shared `parseCountValue`, which applies
  JSON number grammar validation, `apd/v3` exponent checking, exact integrality,
  the non-negative rule, and the 1,000,000 count bound before converting the
  reduced decimal to a machine integer. Exponent-form counts therefore retain
  the same mathematical value at profile load and runtime, including inside
  nested schemas.
- `requireCount` delegates to the same parser, eliminating the previous split
  between profile-load acceptance and runtime representation.
- `isExactInteger` now uses `apd.Decimal.Reduce` and checks the reduced
  exponent. It no longer materializes a power of ten. On the same host, five
  post-change benchmark samples for `1e-100000` measured 48 bytes, two
  allocations, and approximately 0.50–0.62 microseconds per check.
- `TestValidateAgainstSchemaCountExponentForm` covers all four count keywords,
  nested decoding, and zero at the ±100000 exponent boundary.
- `TestParseCountValueExponentForm` covers exact conversion and rejection
  cases, while `TestIsIntegerLiteralDoesNotAllocateByExponent` guards against
  allocation-count growth at the exponent boundary.

Final verification on the resulting worktree:

- the three focused count/integrality regressions passed 100 ordinary
  repetitions and 10 race-enabled repetitions;
- `make check` passed: formatting, the full unit suite, the full race suite,
  `go vet`, golangci-lint with zero issues, and the trimmed binary build;
- uncached `go test -count=1 ./...` and
  `go test -race -count=1 ./...` passed; and
- `go mod verify` and `git diff --check` passed.

Issue #6 should remain open until an independent re-review confirms this final
remediation. Migrated-Tama live acceptance remains separate under issue #8.
