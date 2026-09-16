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

- do not expand exponent-form numbers into zero-padded byte slices;
- compare exact numbers using significant digits plus a decimal scale, with
  checked exponent parsing;
- alternatively, define and document a reviewed exponent bound and reject it
  during profile loading before normalization;
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
- compare numbers with the same exact significant-digit and scale model used
  for numeric bounds;
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
