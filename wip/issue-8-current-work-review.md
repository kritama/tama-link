# Issue #8 current-work review

Initial review: 2026-09-24

Status updated: 2026-09-25

Branch: `feature/phase-2-conformance`

Base: `develop` at `791d99e`

Issue: [#8 Phase 2: prove TamaMCP conformance and live Tama acceptance](https://github.com/kritama/tama-link/issues/8)

## Verdict

Four of the five original findings are resolved. The live-acceptance finding
is mitigated: the command and documentation now state unambiguously that it is
an unimplemented, fail-closed scaffold, but the live runner required by Issue
#8 still does not exist. The Compose pin gate now resolves the Compose model
and proves every image or build source that Compose may select pins the
reported revision.

The worktree therefore is not yet ready to satisfy Issue #8. The fixture,
mocked, Compose-pin, and live gates are now distinguished honestly, `make check`
passes, and the configured cross-build targets compile. Issue #8 remains open,
and live runtime acceptance remains outstanding behind the Tama
System/App/PubSub migration.

## Findings

### 1. Resolved: the conformance gate hid required `Mcp-Param-*` headers

The pinned TamaMCP specification requires each `tools/call` request to mirror
schema-declared argument values in `Mcp-Param-*` headers. The positive fixture
`tools/call mirrored parameter headers` sends `Mcp-Param-Enabled`,
`Mcp-Param-Region`, and `Mcp-Param-Shard`; its paired negative fixture proves
that omitting one produces JSON-RPC `-32020`.

At the initial review, Tama Link had no way to emit those headers.
`CallToolParams` carried only the tool name, arguments, capabilities, and
response limit (`internal/upstream/calltool.go:9-27`), while
`setStandardHeaders` wrote only the protocol, method, and name headers
(`internal/upstream/headers.go:13-32`). The observer also rejected every
`Mcp-Param-*` header (`internal/upstream/conformance/observe.go:48-51`). The
replay server did not validate the captured request; it returned the fixture's
success response unconditionally
(`internal/upstream/conformance/replay.go:22-32`). The positive fixture
therefore passed with a request that a real TamaMCP endpoint would reject.

This is resolved. `catalog.ParamHeaders` now extracts and validates reviewed
`x-mcp-header` mappings from the digested schema. The Tama 2026 adapter passes
those mappings to the upstream client, which renders string, boolean, and
safe-range integer values with the required Base64 sentinel and exact decimal
handling. The conformance observer asserts the emitted headers, and the
missing-header fixture is exercised as a negative endpoint behavior instead of
being replayed as the corrected client's expected success response.

### 2. Mitigated, still incomplete: `make live-accept` cannot pass the live gate

`TestLiveAcceptance` checks six environment variables and then always calls
`t.Fatal(unimplementedChecks())` (`internal/acceptance/live/live_test.go:17-45`).
It never uses either endpoint, starts Tama Link, performs OAuth, exercises an
App or System profile, restarts Link or Tama, observes notifications or polling,
or writes and validates the evidence record. The command therefore remains a
blocker marker rather than the repeatable live acceptance command described in
Issue #8 and `wip/phase-2-acceptance.md`.

The misleading acceptance risk is resolved. The Makefile and
`wip/phase-2-acceptance.md` now explicitly call this an unimplemented scaffold,
state that a pass is impossible in this revision, and distinguish it from the
fixture, mocked, and Compose-pin gates. The test still fails closed even when
the reserved environment is supplied.

The implementation gap remains. Once the migrated topology exists, the
unconditional failure must be replaced with a black-box runner that performs
every required observation and writes evidence only after they all pass. Until
then, Issue #8 cannot close.

### 3. Resolved: the Compose pin gate permitted false pins

The initial check accepted an unrelated topology whose revision-like values
existed only in the environment. The first remediation parsed the Compose
model and rejected that case, but still accepted mutable images when requested
revisions appeared only in unrelated build metadata. This counterexample
passed during the first 2026-09-25 re-review:

```yaml
services:
  tama:
    image: ghcr.io/example/tama:stable
    build:
      context: .
      args:
        UNRELATED: abcdef1
  tama-mcp:
    image: ghcr.io/example/tama-mcp:stable
    build:
      context: .
      args:
        UNRELATED: abcdef0
  provider:
    image: ghcr.io/example/provider:stable
    build:
      context: .
      args:
        UNRELATED: abcdef2
```

```sh
TAMA_LINK_COMPOSE_FILE=/tmp/tama-link-compose-review.yml \
TAMA_LINK_COMPOSE_TAMAMCP_SHA=abcdef0 \
TAMA_LINK_COMPOSE_TAMA_SHA=abcdef1 \
TAMA_LINK_COMPOSE_PROVIDER_SHA=abcdef2 \
make compose-accept
```

The next remediation stopped scanning arbitrary build metadata, always used
`docker compose config`, and accepted only exact image tags/digests or remote
git build refs. A second re-review found that a service declaring both a
mutable image and a pinned remote build could still pass: without
`pull_policy: build`, Compose may pull the mutable image instead of building
the pinned source. The symmetric pinned-image/local-build fallback was also
ambiguous.

This is resolved. The resolved model now retains whether a build exists and
its `pull_policy`. An image-only or build-only service must pin its selected
source. A service with both sources must pin both, unless
`pull_policy: build` guarantees the pinned build source is selected. Local
contexts, build arguments, mutable image tags, and ambiguous image/build
precedence fail closed. Regression tests cover the original unrelated
topology, unrelated build arguments, mutable-image/pinned-build precedence,
pinned-image/local-build fallback, explicit build selection, and two pinned
possible sources.

### 4. Resolved: the evidence validator omitted Issue #8 observations

At the initial review, `RequiredChecks` omitted both ambiguous initial-call
replay and System read-only recovery
(`internal/acceptance/evidence.go:16-27`), even though Issue #8 and the
acceptance document required those observations. `Validate` could therefore
accept a record that never proved either behavior. The single
`forbidden_methods_observed` boolean also did not distinguish forbidden methods
from the independently forbidden `Mcp-Session-Id` header and `params.task`
field.

This is resolved. `RequiredChecks` now includes `ambiguous_replay` and
`system_recovery`. Structured evidence validates stable task identity, call
attempts versus graph executions, read-only System replay, and separate
forbidden-method, `Mcp-Session-Id`, and `params.task` observations. Regression
tests reject duplicate graph execution, protected mutation, and each forbidden
wire shape.

### 5. Resolved: the invalid-input fixture could pass after network I/O

At the initial review, `assertLocalRejection` pointed the client at
`127.0.0.1:1` and considered any non-protocol, non-authentication error acceptable
(`internal/upstream/conformance/drive.go:51-65`). If validation regresses and
the invalid `tasks/update` request reached the network, the resulting transport
error still passed the test. It therefore did not prove the request was
rejected locally.

This is resolved. `assertLocalRejection` now installs a fail-on-call transport,
asserts that it was not invoked, and checks the expected local validation
error. A transport failure can no longer make this fixture green.

## Validation performed

- Verified the three vendored fixture SHA-256 values against their exact
  GitHub commit contents: core and Tasks at
  `6b5db00018d2774834db5a0f00eed5b9b55e1d2e`, subscriptions at
  `5c80c29e90c49438fbcc331db5c00f9f8f93ee21`.
- Re-ran focused catalog, upstream, conformance, adapter, and acceptance tests
  successfully with loopback sockets enabled.
- Re-ran `make check` successfully: unit tests, race detector, `go vet`,
  golangci-lint with zero issues, and the trimmed build all passed.
- Built all five CI targets successfully with `CGO_ENABLED=0`: Linux amd64 and
  arm64, macOS amd64 and arm64, and Windows amd64.
- Confirmed `make live-accept` fails as the documented unimplemented scaffold.
- Confirmed the Compose gate rejects mutable images with pinned remote builds
  when default pull-first behavior could select the image.
- Confirmed the Compose gate accepts the same pinned remote builds when
  `pull_policy: build` guarantees their selection.
- Confirmed `git diff --check` passes.

The successful static and mocked gates do not establish Compose startup or
live runtime acceptance.
