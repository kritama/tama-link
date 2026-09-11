# Phase 0 Implementation Review

Status: resolved; retained as historical review evidence

Review date: 2026-09-11

Resolution date: 2026-09-12

Reviewed branch: `feature/phase-0-repository-foundation`

Reviewed foundation commit: `e63fa86aef2065103fea18e595feaad83ca8df0d`

Reviewed catalog commit: `e108d61f75205f752fe605dcbace0fb4cbc1fe04`

This report records the Phase 0 review findings against
`../tama-link-specification.md`, `plan.md`, `../../AGENTS.md`, and
`../../CONTRIBUTING.md`. The `internal/catalog` package was initially untracked
and changing while this report was prepared, then was committed as `e108d61`.
The validation notes below distinguish the initial foundation review from the
latest refresh after that commit.

## Resolution decision

Phase 0 and Phase 1 are complete. Development may proceed to Phase 2.

The findings below describe the reviewed historical snapshots. They were
resolved by the Phase 0 remediation and Phase 1 storage commits through
`21d617c`. The current implementation has a lossless raw result model,
tri-state task support, output drift checks, precise canonical JSON, bounded
projection, separate hard ceilings, profile/catalog integration, encrypted
durable state, secure credential integration, and leased worker recovery.

On 2026-09-12, `make check` passed with writable Go and golangci-lint caches,
and all five required `CGO_ENABLED=0` target builds passed. The subprocess test
selector was also moved out of mutable environment state and regression-tested
to prevent recursive `store.test` spawning.

## What was implemented successfully

- The CLI has `serve`, `login`, `logout`, and `version` command surfaces.
- `serve --profile <name>` is required and a missing profile fails cleanly.
- `version --json` emits a machine-readable version document.
- The downstream STDIO MCP server advertises exactly `submit` and `await`.
- The STDIO end-to-end test completes an initialize/list-tools interaction.
- Submission states and transition guards exist, including absorbing terminal
  states.
- Progress events have monotonic sequence validation and submission identity
  checks.
- Version 1 size, timeout, event, and retention values have been introduced.
- Host builds and cross-builds passed for Linux amd64/arm64, Darwin
  amd64/arm64, and Windows amd64 with `CGO_ENABLED=0`.
- The catalog has focused descriptor, digest, drift, and projection tests,
  although PH0-R15 identifies important missing boundary cases.
- The hand-written Go modules are small and separated by responsibility; none
  observed in this review approached the repository's 300-line review signal.
- The selected Go SDK version supports the downstream MCP features attributed
  to it in the plan, including MCP `2026-07-28`, `server/discover`,
  `subscriptions/listen`, stateless request `_meta`, and MRTR input responses.

## Historical blocking findings (resolved)

### PH0-R1 — P1 — Terminal result representation is lossy

Location: `internal/contract/await.go`, `Content` and `Result`.

The current result model supports only text content:

```go
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
```

It therefore cannot preserve a generic MCP `CallToolResult`. It drops image,
audio, resource-link, and embedded-resource content; content annotations;
content-level `_meta`; and safe result-level `_meta`. It also declares
`StructuredContent` as `map[string]any`, even though MCP permits any valid JSON
value, including an array or primitive.

This contradicts the specification's requirement that a captured completed
result preserve `isError`, all content blocks, structured content, and safe
`_meta`, without truncating a terminal result.

Required resolution:

- define a lossless normalized JSON representation for every allowed MCP tool
  result content block;
- allow `structured_content` to contain any valid JSON value;
- preserve reviewed safe `_meta` at the result and content-block boundaries;
- add round-trip fixtures for text, image, audio, resource link, embedded
  resource, annotations, object/array/primitive structured content, and
  `isError: true`; and
- settle this shape before designing the encrypted terminal-result storage
  schema.

### PH0-R2 — P1 — Task support incorrectly collapses a three-state contract

Locations: `internal/catalog/descriptor.go` and
`internal/catalog/drift.go`.

`Descriptor.TaskSupport` and `LiveTool.TaskSupport` are booleans. Current Tama's
Anubis MCP tool model uses three states:

```text
forbidden | optional | required
```

The live wire representation emits `execution.taskSupport` for `optional` and
`required`, while absence means `forbidden`. A boolean cannot distinguish a
tool that accepts task augmentation from one that requires it. This can select
an invalid execution form or allow a contract change through drift checking.

Evidence in the Tama checkout:

- `deps/anubis_mcp/lib/anubis/server/component/tool.ex` defines
  `:forbidden | :optional | :required`;
- its encoder writes `execution.taskSupport` for `optional` and `required`; and
- `deps/anubis_mcp/test/support/tasks_stub_server.ex` contains fixtures for all
  three behaviors.

Required resolution: replace the boolean with a validated string enum in both
pinned and live descriptors, normalize an absent live execution declaration to
`forbidden`, include the full value in the descriptor digest, compare it during
drift verification, and test all transitions.

### PH0-R3 — P1 — Output schema drift is not checked

Location: `internal/catalog/drift.go`, `LiveTool` and `Descriptor.Drift`.

Pinned descriptors include `OutputSchema`, but the live-tool projection omits
it and drift verification never compares it. A server can therefore change its
declared output contract without triggering `operation_contract_mismatch`.
That breaks the plan's fail-closed catalog rule and is particularly risky when
terminal results are normalized and persisted according to the old contract.

Required resolution: carry the live `outputSchema`, compare it canonically with
the pinned `OutputSchema`, report `output_schema` drift, and add fixture tests
for equivalent and changed schemas.

### PH0-R4 — P1 — JSON canonicalization loses numeric precision

Location: `internal/catalog/drift.go`, `canonical`.

The implementation decodes arbitrary JSON into `any` with `json.Unmarshal`.
JSON numbers therefore become `float64`. Distinct integer bounds above the
IEEE-754 exact-integer range can collapse to the same value before hashing or
comparison, allowing security-relevant schema drift to go undetected.

Required resolution:

- decode with `json.Decoder.UseNumber` or adopt a reviewed canonical JSON
  implementation;
- require exactly one complete JSON value and reject trailing data;
- decide and document how duplicate object keys are handled, preferably
  rejecting them at profile-validation time; and
- add regression tests with distinct integers above `2^53`, exponent forms,
  trailing tokens, and duplicate keys.

### PH0-R5 — P2 — Description clipping can exceed its bound and corrupt UTF-8

Location: `internal/catalog/projection.go`, `clip`.

The function first keeps `n` bytes and then appends `...`, so its output can be
`n + 3` bytes despite the stated bound. Its rune-boundary check inspects only
the final byte. If the cut contains the leading byte of a multi-byte rune but
not its continuation bytes, that leading byte can remain and the result is
invalid UTF-8.

Required resolution: reserve space for the omission marker, cut on a verified
UTF-8 boundary, define behavior for `n` smaller than the marker, and test ASCII,
two/three/four-byte runes, malformed input, and exact boundary sizes.

The original review also found that the declared 32-signature limit was not
applied. Catalog commit `e108d61` now applies it inside `SubmitDescription`, so
that part is partially addressed. `Signatures` still
describes its return value as bounded while returning every operation; either
enforce the same contract there or narrow its documentation and keep it an
unbounded internal projection.

### PH0-R6 — P2 — Retention validation permits an impossible lifecycle

Location: `internal/limits/limits.go`, `Limits.Validate`.

Payload and tombstone retention are validated independently, so a profile can
set `TombstoneRetention < PayloadRetention`. Both values are measured from
completion in the specification: the payload is retained first, and a
payload-free tombstone remains after it. A tombstone that expires before the
payload cannot implement that lifecycle.

Required resolution: enforce
`TombstoneRetention >= PayloadRetention` and test equality, valid longer
tombstone retention, and the invalid reversed ordering.

### PH0-R7 — P1 — The repository validation gate is not green

The initial review of foundation commit `e63fa86` found:

- the then-untracked working tree failed `make check` at `fmt-check` on
  `internal/catalog/projection.go`;
- an isolated archive of tracked `HEAD` passed tests, race tests, vet, and build
  but failed golangci-lint because exported `limits.KiB` has no Go
  documentation; and
- an early untracked catalog snapshot also had no catalog tests and an
  unused `maxSignatures` declaration.

The catalog was subsequently formatted, tested, and committed as `e108d61`;
`maxSignatures` is now applied by `SubmitDescription`. At the latest refresh,
`go test ./...`, `go test -race ./...`, and `go vet ./...` passed. The working
tree also contained an uncommitted documentation comment that addresses the
`KiB` lint finding. However, `make check` still did not finish successfully:
golangci-lint aborted during package loading with `no go files to analyze`.
That last message may be a local lint/cache environment failure rather than a
source defect, but it is not a passing repository gate.

Required resolution: retain the exported-identifier documentation fix, diagnose
or clear the golangci-lint loading failure, then run `make check` from a clean
working tree. Re-run the five `CGO_ENABLED=0` cross-builds after the catalog and
contract changes.

## Historical additional findings (resolved)

### PH0-R8 — P2 — Binding targets are not fully validated as JSON Pointers

Location: `internal/catalog/descriptor.go`, `Binding.Valid`.

Validation currently checks only that a target is non-empty and starts with
`/`. It does not validate RFC 6901 escapes (`~0` and `~1`) or reject malformed
escape sequences. Catalog validation also does not currently reject duplicate
targets whose write order could become observable.

Before bindings are applied to upstream arguments, validate the complete JSON
Pointer grammar, define whether multiple bindings may target the same path, and
add fixtures for escaped keys and malformed targets.

### PH0-R9 — P2 — Default limits and hard ceilings are conflated

Location: `internal/limits/limits.go`.

The package says the version 1 defaults are also the implementation hard
ceilings. The specification says profiles may lower the defaults and may raise
them when explicit values remain within implementation hard ceilings and the
profile digest is reconciled. If default equals ceiling, raising a default is
impossible.

Resolve the contract in one direction:

- preferably define separate `Default` and `HardCeiling` values and validate a
  profile against the latter; or
- explicitly amend both specification documents to say version 1 values can
  only be lowered.

### PH0-R10 — P2 — Request-level await errors do not match their documented shape

Locations: `internal/contract/await.go` and `internal/server/server.go`.

`AwaitOutput` says request-level failures carry only the error, but several
fields are not omitted when empty (`submission_id`, `terminal`, and `events`).
The placeholder handler also sets `terminal: true` for `not_implemented`, even
though no accepted submission exists. This blurs a request/tool failure with a
terminal submission state.

Define one stable error envelope: either make request-level failures a distinct
shape with only `error`, or document and test the required zero-value fields.
Do not label a request failure terminal unless it refers to a real submission.

### PH0-R11 — P3 — CLI commands accept unexpected positional arguments

Locations: `cmd/tama-link/serve.go`, `version.go`, and the shared login/logout
flag parser in `login.go`.

The flag sets do not check `fs.NArg()`. Commands such as `version junk` or
`serve --profile x junk` can therefore accept ignored input. Reject positional
arguments as usage errors and add table-driven command tests.

### PH0-R12 — P2 — Profile existence checking is too coarse for the future loader

Location: `cmd/tama-link/serve.go`, `serveConfig.resolveProfile`.

Every `os.Stat` failure is reported as “not found,” which hides permission and
I/O errors. The check follows symlinks and accepts any non-directory file type.
The full secure loader belongs to Phase 1, but it must preserve the plan's
atomic-write and no-symlink-traversal requirements.

When Phase 1 replaces this existence check, preserve the underlying error
category, reject symlinks and non-regular files, and validate path ownership and
permissions according to the final profile contract.

### PH0-R13 — P3 — README usage and readiness text are stale

Location: `README.md`, development and registration examples.

The README runs `serve` without the now-required profile, calls the client
registration “eventual,” and says `--profile` is not implemented. Update it to
show a real named profile and describe the current foundation accurately.

### PH0-R14 — P3 — The plan's current-state section is stale

Location: `plan.md`, “Current state.”

It says Phase 0 is approximately 95% complete because `serve --profile` and
`version --json` are missing, but both have been implemented. Replace that list
with the actual remaining gate and contract work. Do not mark Phase 0 complete
until `make check` passes from the intended tracked tree.

### PH0-R15 — P2 — Catalog tests do not yet cover its security boundaries

Location: `internal/catalog`.

The earliest untracked snapshot had no tests. Commit `e108d61` added catalog
tests and they now pass, but the suite still lacks the critical regressions
described above: output-schema drift, tri-state task support, high-precision
JSON numbers, trailing/duplicate JSON input, complete JSON Pointer validation,
and UTF-8-safe bounded clipping.

Because catalog projection and drift checking are security boundaries, use
fixture-based public-behavior tests rather than testing only helper details.

### PH0-R16 — P2 — The catalog work is not integrated

Location: `internal/catalog`.

The package is not wired into profile loading or the downstream server. Server
instructions and the `submit` description are still hard-coded. This is
expected as partial Phase 1 work, but it means the descriptor allowlist,
instruction snapshot, operation enum, and live/pinned catalog intersection are
not yet effective behavior.

Do not treat the presence of the package as satisfying the Phase 1 profile and
catalog exit criteria. Integrate it only after its contract issues and fixtures
are resolved.

## Historical validation evidence

The following checks passed on the initial reviewed tree unless otherwise
noted:

| Check | Result |
| --- | --- |
| `go test ./...` | Passed on the latest refresh, including catalog tests |
| `go test -race ./...` | Passed on the latest refresh |
| `go vet ./...` | Passed on the latest refresh |
| `go mod tidy -diff` | Clean |
| Host build | Passed |
| Linux amd64, `CGO_ENABLED=0` | Passed |
| Linux arm64, `CGO_ENABLED=0` | Passed |
| Darwin amd64, `CGO_ENABLED=0` | Passed |
| Darwin arm64, `CGO_ENABLED=0` | Passed |
| Windows amd64, `CGO_ENABLED=0` | Passed |
| golangci-lint | Latest refresh aborted during package loading with `no go files to analyze` |
| `make check` | Failed at lint; see PH0-R7 |
| Pull request / hosted CI | No pull request was found for the branch, so no hosted CI result exists |

The environment emitted harmless read-only Go module stat-cache warnings
during cross-builds; the produced binaries still built successfully.

## Resolved exit checklist

Phase 0 may be closed when all of the following are true:

- [x] PH0-R1 through PH0-R7 are resolved with tests.
- [x] PH0-R8 through PH0-R10 and PH0-R15 are resolved before their affected
      contracts are persisted or exposed.
- [x] README and `plan.md` describe the current CLI and phase status.
- [x] The catalog work is tracked.
- [x] `make check` passes from the intended tracked tree.
- [x] The five `CGO_ENABLED=0` target builds pass after the final changes.
- [x] Phase 0 is marked complete in `plan.md`.

The durable local submission model can now be created, made idempotent,
transitioned, reopened, and recovered as required by `plan.md`. Phase 2 upstream
adapter implementation is the next development stage.
