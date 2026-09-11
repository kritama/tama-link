# Tama Link Implementation Plan

Status: implementation plan (supersedes nothing; refines `../tama-link-specification.md`)

This plan sequences the work required to satisfy the Tama Link specification and
records the concrete decisions the spec leaves open. It is grounded in the
surrounding repositories this proxy actually integrates with:

- `tama-mcp` — next-generation MCP `2026-07-28` server library (Phase 4 target);
- `tama-oauth` — protocol library for the OAuth mechanics (PKCE S256,
  `private_key_jwt`, RFC 7662 introspection, refresh-token rotation);
- `memovee` — the authorization server (issuer `https://app.localhost`) and the
  host of the local Tama topology;
- `memovee/tama/graph/AGENT-INSTRUCTIONS.md` — the authoritative caller contract
  for the **current** Tama release (`0.14.0-server`), which is the Phase 2
  integration target;
- `memovee-cli` — will own binary pinning and profile creation (not yet
  implemented).

The authoritative contract is `../tama-link-specification.md`. Where this plan
resolves an open question from that spec, it says so explicitly.

## Current state

Phase 0 of the spec is ~95% complete in this repository:

- Go module (`go 1.25.0`), `github.com/modelcontextprotocol/go-sdk v1.6.1`;
- STDIO MCP server exposing exactly `submit` and `await`
  (`internal/server/server.go`);
- placeholder handlers that fail with `not_implemented`;
- a unit test proving the two-tool catalog (`internal/server/server_test.go`);
- `make check` = fmt-check + test + race + vet + lint + build;
- CI with Go validation and a 5-target cross-build
  (linux amd64/arm64, darwin amd64/arm64, windows amd64), all `CGO_ENABLED=0`.

Still missing against the Phase 0 list: `serve --profile <name>` flag wiring and
`version --json`.

## Key architectural decisions (resolving spec open questions)

### D1. The upstream side is a reviewed hand-rolled JSON-RPC client, not the Go SDK

The spec requires "a reviewed stable release of the official MCP Go SDK." That
is achievable **only for the downstream (STDIO) side**. For the upstream side it
is not achievable, because no released Go SDK supports the MCP Tasks protocol
that current Tama speaks:

- `go-sdk` stable `v1.7.0` (and even pre-release `v1.8.0-pre.2`) expose no
  `params.task` on `CallToolParams`, and no `tasks/get`, `tasks/result`, or
  `tasks/cancel` methods; `ClientSession` has no generic raw-request API.
- Current Tama's caller contract is entirely task-based: `tools/call` with
  `params.task`, then `tasks/get` / `tasks/result` / `tasks/cancel`, with
  **session-scoped** task IDs.

Decision: Phase 2 builds a small, reviewed, fixture-tested JSON-RPC 2.0 client
over streamable HTTP (`internal/upstream`) for the task protocol. The official
Go SDK remains the downstream STDIO server and is sufficient there (it provides
tool registration, `GetProgressToken`, and `ServerSession.NotifyProgress`).

This is a deliberate, documented deviation from the literal spec wording and
must be recorded as a spec amendment (gap G1).

### D2. Local persistence is SQLite via a pure-Go driver

Use `modernc.org/sqlite` (pure Go) so the existing `CGO_ENABLED=0` cross-builds
keep working. The state store holds the minimum durable state the spec permits
and adds the **canonical upstream request** (see D3).

### D3. Persist the canonical upstream request, not just the task ID

Current Tama task IDs are valid only in the MCP session that created them. After
any new session (Tama Link restart, Tama restart, token rotation), the only
recovery path is to re-issue the same idempotent upstream `message` arguments to
re-attach to the durable Submission and obtain a new session-scoped task ID.

Therefore the state store must persist, per submission:

- the local `submission_id` and `client_request_id`;
- the upstream tool name and the **validated, canonicalized arguments** (the
  exact idempotent upstream request);
- the current upstream task ID (may be stale);
- normalized state, timestamps, progress cursor and bounded events;
- the terminal result or structured failure within retention limits;
- the negotiated protocol version and adapter version.

This resolves spec acceptance criterion #10 (process restart recovers accepted
non-terminal submissions) and is a spec gap (G2).

### D4. Credentials live only in the platform keyring

Refresh tokens and client-registration records go to the platform credential
store via `99designs/keyring` (SecretService on Linux, Keychain on macOS, DPAPI
on Windows). Nothing secret is written to the SQLite state store, logs, or JSON
output. Access tokens are held in memory only.

### D5. OAuth is auth-code + PKCE S256 with an ephemeral loopback redirect

Memovee's authorization server mandates PKCE S256 and supports exact resource
binding. The authorization flow opens an **ephemeral 127.0.0.1 loopback
listener** for the single redirect of one authorization attempt. This is a
scoped, documented exception to the spec's "must not open a TCP listener" rule,
which is reinterpreted as "no *persistent* control-surface listener" (gap G3).

### D6. Terminal results are captured immediately and owned locally

Current Tama task TTL is short (caller examples use `ttl: 60000` ms). `await`
therefore captures the upstream terminal result into the local store **the
moment it is observed**, so the result survives Tama Link or Tama restarts and
client disconnects. Repeated `await` after terminal return is served entirely
from local state.

## Package layout

```
cmd/tama-link/            serve, doctor, version (CLI wiring)
internal/version/         build version string
internal/server/          downstream STDIO MCP server; submit/await handlers
internal/contract/        stable tool input/output + error types (compat API)
internal/submission/      normalized state machine, idempotency, progress model
internal/profile/         profile loading, validation, per-tool operation schemas
internal/store/           SQLite durable state, migrations, GC
internal/credential/      platform keyring wrapper (secrets only)
internal/oauth/           discovery, auth-code+PKCE, single-flight refresh
internal/upstream/        reviewed JSON-RPC/streamable-HTTP client (task protocol)
internal/adapter/tama014/ current Tama (0.14.0) adapter: submit/await/normalize
internal/limits/          size, rate, timeout, retry, retention bounds
```

## Phase 0 — finish the repository foundation

- Wire `serve --profile <name>` (profile flag required for `serve`);
- add `version --json`;
- update usage text;
- confirm `make check` and CI pass.

Exit: `tama-link serve --profile x` starts the two-tool STDIO server and fails
cleanly if the profile is missing; `version --json` emits deterministic JSON.

## Phase 1 — normalized domain, profile, and durable state

- `internal/submission`: state machine
  (`accepted -> queued -> running -> {succeeded, failed, cancelled, expired}`)
  with transition guards; terminal states are absorbing. Error taxonomy types.
  Progress event model with monotonic `sequence` and dedupe by
  (submission, sequence).
- `internal/profile`: load a named profile from a defined search path (see G6),
  versioned JSON schema, origin/URL validation, per-tool operation allowlist
  **including argument schemas**, and timeouts/limits/retention.
- `internal/store`: SQLite store (D2/D3). Tables for submissions and an
  idempotency index (`client_request_id` → canonical input hash → submission).
  Atomic writes, migrations, `0600` permissions, no symlink traversal, and GC
  that never removes a non-terminal submission.
- `internal/credential`: keyring wrapper (D4).
- Tests: state transition matrix (including no re-entry from terminal),
  idempotency hit and conflict, restart recovery, atomic write safety.

Exit: a submission can be created, made idempotent, transitioned, persisted,
and recovered after an in-process store reopen, with no upstream involved.

## Phase 2 — current Tama (0.14.0) adapter

- `internal/upstream`: reviewed JSON-RPC 2.0 client over streamable HTTP
  (initialize handshake, `Mcp-Session-Id`, `application/json` and SSE
  responses, bearer auth) supporting `tools/call` (with `params.task`),
  `tasks/get`, and `tasks/cancel`. Fixture-driven tests.
- `internal/oauth`: RFC 9728 protected-resource discovery → RFC 8414 AS
  metadata, dynamic client registration, auth-code + PKCE S256 with the
  ephemeral loopback redirect (D5), and single-flight refresh with rotation
  (D4).
- `internal/adapter/tama014`:
  - `submit` → validate arguments against the profile operation schema →
    inject generated identifiers into upstream arguments (G7) → task-augmented
    `tools/call` → persist submission + task ID → return `accepted`;
  - `await` → bounded long-poll via `tasks/get` using the upstream poll
    interval → normalize state/progress → capture terminal result locally
    immediately (D6);
  - restart recovery / re-attach (D3): reopen the upstream session and re-issue
    the persisted idempotent request to obtain a fresh session-scoped task ID;
    never reuse a stale task ID.
- Record and validate the negotiated protocol version at connect; unsupported
  combinations fail closed with `protocol_mismatch`.
- Result normalization + size validation; explicit upstream→normalized error
  and state mapping table (G4).
- Fixture tests from `memovee/tama/graph/AGENT-INSTRUCTIONS.md` and
  `memory-contract.v1.json`; live integration against the local `memovee/tama`
  compose stack (Tama `0.14.0-server` + Memovee AS).

Exit: the full `submit` → repeated `await` → terminal workflow works against a
live local Tama, survives Tama Link restart, and never duplicates upstream work.

## Phase 3 — client progress and acceptance

- Downstream MCP progress-token support: read `_meta.progressToken` on `await`,
  emit rate-limited `notifications/progress` correlated to that request
  (Go SDK `GetProgressToken` + `ServerSession.NotifyProgress`).
- `doctor --profile <name> [--json]` (read-only): profile validation, metadata
  fetch, credential presence, state integrity, non-mutating upstream reachability.
- Disconnect, restart, timeout, and live-OAuth acceptance tests.
- Client-package acceptance: Codex, OpenCode, and a plain MCP inspector complete
  the `submit`/repeated-`await` workflow.

Exit: acceptance criteria #1–#15 of the spec are demonstrated by automated and
live tests.

## Phase 4 — newer (2026-07-28) MCP adapter

- A second upstream adapter implementing the stateless per-request `_meta`
  protocol from `tama-mcp`: `tools/call` with task policy, `tasks/get` polling,
  optional `subscriptions/listen` SSE as an optimization, and `tasks/update`
  for `input_required`.
- Downstream `submit`/`await` contract unchanged.
- Define the `input_required` contract behavior (G11) before implementation.
- Expand the published compatibility matrix only after live client tests.

Exit: the 2026-07-28 path is selectable by profile/adapter version and passes
the same acceptance suite; the 0.14.0 path remains the default.

## Gaps that must be resolved (spec amendments)

These are ambiguities or contradictions in `../tama-link-specification.md` that
change architecture. Each needs a short spec amendment before the affected
phase starts.

1. **G1 (blocking) — "official MCP Go SDK" is not achievable upstream.**
   No released Go SDK supports the Tasks protocol current Tama speaks. The spec
   must explicitly permit a minimal reviewed hand-rolled JSON-RPC/streamable-HTTP
   upstream client (D1), with the official SDK retained for the downstream STDIO
   server.

2. **G2 — session-scoped task IDs vs. restart recovery.** The spec persists an
   "upstream opaque correlation identifier" but never defines re-attach
   semantics, and "permitted persisted values" omits the canonical upstream
   request that recovery requires (D3). Add an explicit recovery section.

3. **G3 — "no TCP listener" contradicts browser OAuth.** The PKCE flow needs an
   ephemeral 127.0.0.1 loopback listener for one redirect (D5). Reinterpret the
   rule as "no persistent control-surface listener" and explicitly permit the
   single-use ephemeral loopback listener, or define an out-of-band code-entry
   fallback. Also state what triggers the flow.

4. **G4 — upstream→normalized state mapping is undefined.** Current Tama
   transport status is only `completed`/`failed`, and a `completed` transport
   can carry a domain result whose outcome is `failed`. Add the explicit table:
   transport `completed` (any domain outcome) → `succeeded` with the full
   envelope as `result`; transport `failed` → `failed` +
   `upstream_execution_failed`; task gone/expired → `expired`; `cancelled`
   unreachable in the initial adapter (reserved); `queued` likely indistinguishable
   from `accepted` initially.

5. **G5 — the current upstream has no progress channel.** There are no progress
   tokens or structured steps in the current caller contract. Initial `await`
   progress is state-transition events only, with `current`/`total`/`label`
   omitted. Mark the spec's rich pending-output example as the *rich-adapter*
   shape so Phase 2 tests are not written against fabricated data.

6. **G6 — profile location/format/discovery is unspecified.** `serve
   --profile <name>` needs a defined search path (e.g. XDG config), file format,
   and schema version. The "planned non-secret profile" list also omits
   **per-operation argument schemas**, which `submit`'s validation requirement
   demands.

7. **G7 — `client_request_id` ↔ upstream idempotency binding is unspecified.**
   Current Tama has no generic idempotency parameter; identity is
   issuer+actor+client+recipient+thread+message `identifier`+exact content.
   Define per-tool binding: which argument field carries the generated key,
   which fields form the conflict identity, and how an upstream conflict maps to
   `idempotency_conflict`.

8. **G8 — `result_too_large` behavior is undecided.** If a terminal result
   exceeds the size limit at capture, does the submission become terminal
   `failed`/`result_too_large` (conflating successful work with failure) or is
   it stored truncated (violating "returned exactly as stored")? Decide, and set
   concrete size/retention defaults.

9. **G9 — the target upstream may not accept production ingress yet, and TTL
   semantics are unverified.** The caller contract notes the graph is
   unavailable for normal production ingress until remember/recall complete, and
   `task.ttl` (1 min in examples) could be result-retention or a lifetime, which
   drives how aggressively `await` must capture terminals (D6). Name the fixture
   environment (local `memovee/tama` compose, Tama `0.14.0-server`), pin the
   supported release, and verify handshake protocol version + ttl semantics
   against a live instance during Phase 2.

10. **G10 — refresh-token rotation handling.** Memovee rotates refresh tokens on
    every use and revokes the family on replay. Require single-flight refresh
    with atomic credential-store update and `invalid_grant` →
    `authentication_required` (no retry loops).

11. **G11 (Phase 4) — `input_required` has no client-facing path.** The
    2026-07-28 profile has an `input_required` task state answered via
    `tasks/update`, but the two-tool contract has no way for a client to answer
    an input request. Decide before Phase 4 (e.g. hold as running with a
    structured message).

## Suggested order of attack

1. Land spec amendments for G1–G4 (they change architecture).
2. Phase 0 finish + Phase 1 (no upstream dependency).
3. Phase 2 against the local `memovee/tama` stack (resolve G9 live).
4. Phase 3 acceptance, then Phase 4 once `tama-mcp` Phases 2–3 and the Tama
   `/mcp/app` migration land.
