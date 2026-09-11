# Tama Link Implementation Plan

Status: implementation plan (supersedes nothing; refines `../tama-link-specification.md`)

This plan sequences the work required to satisfy the Tama Link specification and
records the concrete decisions the spec leaves open. It is grounded in the
surrounding repositories this proxy actually integrates with:

- `tama-mcp` — next-generation MCP `2026-07-28` server library (Phase 4 target);
- `tama-oauth` — protocol library for the OAuth mechanics (PKCE S256,
  `private_key_jwt`, RFC 7662 introspection, refresh-token lifecycle);
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

Phase 0 is complete: `serve --profile <name>`, reserved `login`/`logout`
stubs, `version [--json]`, the two-tool STDIO server with placeholder
`not_implemented` handlers, and an end-to-end test that builds the real
binary and completes an initialize/`tools/list` handshake.

Phase 1 is complete. The durable-domain implementation now includes:

- `internal/contract`: stable tool inputs/outputs, raw lossless MCP content
  blocks, structured result validation, and the error taxonomy;
- `internal/submission`: state machine with transition guards and event
  sequence invariants;
- `internal/limits`: version 1 defaults with separate hard ceilings;
- `internal/catalog`: pinned descriptors, canonical digests, drift
  verification, and bounded submit-description projection;
- `internal/profile`: versioned profile loading with duplicate-key and secure
  URL validation, profile-isolated state paths and credential namespaces, and
  effective-limit resolution;
- `internal/server`: profile-driven MCP server wiring the catalog projection,
  instructions, and the bounded submit `tool` enum;
- `internal/store`: encrypted SQLite state (D2/D3/D4) with complete-input
  idempotency, compare-and-set transitions, bounded progress, atomic terminal
  capture, lease-guarded worker writes, payload-free D12 GC, and schema and
  encryption-format migrations; first-open metadata is atomic under concurrent
  access and ciphertext is bound to its submission and semantic field;
- `internal/credential`: a profile-scoped platform keyring restricted to the
  secure OS backend, with missing and unavailable states kept distinct and
  fail-closed store integration;
- `internal/worker`: owned lease renewal, cancellation, safe terminal failure,
  single-winner execution, and restart recovery for `local_replayable` work;
- the separate-process storage test suite (concurrent migration, competing
  idempotent inserts, busy timeout, exclusive claim, lease recovery after
  death, terminal capture surviving GC, refresh leasing, WAL recovery,
  integrity), plus worker cancellation/recovery and legacy encryption migration
  coverage.

Phase 2 is next. The downstream `submit` and `await` handlers deliberately
remain placeholders until the current Tama System and App adapters are wired;
Phase 1's no-upstream exit criteria are satisfied independently of that work.

## Key architectural decisions (resolving spec open questions)

### D1. The upstream side is a reviewed hand-rolled JSON-RPC client, not the Go SDK

The spec requires "a reviewed stable release of the official MCP Go SDK." That
is achievable **only for the downstream (STDIO) side**. For the upstream side it
is not achievable, because no released Go SDK supports the MCP Tasks protocol
that current Tama speaks:

- The pinned stable `go-sdk v1.7.0` exposes no `params.task` on
  `CallToolParams`, and no `tasks/get`, `tasks/result`, or `tasks/cancel`
  methods for the 2025-11-25 experimental Tasks extension that current Tama
  speaks.
- Current Tama App's caller contract is entirely task-based: `tools/call` with
  `params.task`, then `tasks/get` / `tasks/result` / `tasks/cancel`, with
  **session-scoped** task IDs.

Decision: Phase 2 builds a small, reviewed, fixture-tested JSON-RPC 2.0 client
over streamable HTTP (`internal/upstream`) for the task protocol. The official
Go SDK remains the downstream STDIO server and is sufficient there (it provides
tool registration, `GetProgressToken`, and `ServerSession.NotifyProgress`).

`go-sdk v1.7.0` is the initial pin: it is the newest reviewed stable release,
preserves full backward compatibility with clients negotiating
`2025-11-25` or earlier, and natively implements the `2026-07-28` protocol
(stateless per-request `_meta`, `server/discover`, `subscriptions/listen`,
MRTR `inputResponses`) plus custom JSON-RPC method registration. The Phase 4
upstream adapter therefore builds on the official SDK directly, using custom
methods only for the `tasks/*` extension surface.

This is a deliberate, documented deviation from the original spec wording and
is now recorded in the authoritative specification.

### D2. Local persistence is SQLite via a pure-Go driver

Use `modernc.org/sqlite` (pure Go) so the existing `CGO_ENABLED=0` cross-builds
keep working. Each profile uses an isolated database. The state store holds the
minimum durable state the spec permits and adds the **canonical upstream
request** (see D3). SQLite is authoritative for Tama Link's client-facing
submission lifecycle and for locally executed System operations; it does not
replace Tama's existing durable App graph submission.

### D3. Persist the canonical upstream request, not just the task ID

Current Tama task IDs are valid only in the MCP session that created them. After
any new session (for example, after a Tama Link or Tama restart), the only
recovery path is to re-issue the same idempotent upstream `message` arguments to
re-attach to the durable Submission and obtain a new session-scoped task ID.

Therefore the state store must persist, per submission:

- the local `submission_id` and `client_request_id`;
- the upstream tool name, selected execution strategy, descriptor digest, and
  the **validated, canonicalized arguments** (the exact recoverable request);
- the current upstream task ID (may be stale);
- normalized state, timestamps, progress cursor and bounded events;
- the terminal result or structured failure within retention limits;
- the negotiated protocol version and adapter version.

This resolves the restart-recovery acceptance criterion and is now recorded in
the authoritative specification.

### D4. Credentials live only in the platform keyring

Refresh tokens and client-registration records go to the platform credential
store via `99designs/keyring` (SecretService on Linux, Keychain on macOS, DPAPI
on Windows). Nothing secret is written to the SQLite state store, logs, or JSON
output. Access tokens are held in memory only.

Because canonical arguments and terminal results can contain private App or
System data, sensitive SQLite blobs are encrypted with a random profile-scoped
key stored in the same platform credential backend. Only non-sensitive lookup
indexes, hashes, state labels, leases, and timestamps remain plaintext.

### D5. OAuth is auth-code + PKCE S256 with an ephemeral loopback redirect

Memovee's authorization server mandates PKCE S256 and supports exact resource
binding. The authorization flow opens an **ephemeral 127.0.0.1 loopback
listener** for the single redirect of one authorization attempt. This is a
scoped, documented exception to the original "must not open a TCP listener"
rule, which the authoritative specification now states as "no persistent
control-surface listener."

### D6. Terminal results are captured immediately and owned locally

Current Tama task TTL is short (caller examples use `ttl: 60000` ms). `await`
therefore captures the upstream terminal result into the local store **the
moment it is observed**, so the result survives Tama Link or Tama restarts and
client disconnects. Repeated `await` after terminal return is served entirely
from local state.

### D7. One profile means one endpoint, process identity, and state namespace

One `tama-link serve --profile <name>` process connects to exactly one upstream
MCP endpoint. `/mcp/app` and `/mcp/system` use separate profiles, SQLite files,
credential namespaces, catalogs, and instruction snapshots. Clients register
the same binary twice under distinct MCP server names:

```text
tama-app    -> tama-link serve --profile tama-app
tama-system -> tama-link serve --profile tama-system
```

The downstream tool names remain `submit` and `await`; the client registration
name supplies the outer namespace. A single process must not combine the two
profiles or add a profile selector to `submit`.

### D8. Profiles pin an approved operation descriptor catalog

The Memovee CLI or client installer writes a deterministic snapshot containing
the selected upstream instructions and an allowlisted descriptor for every
operation: name/title, description, upstream and client-visible input schemas,
output schema, annotations, task support, declarative bindings, execution
strategy, and digest.

At runtime Tama Link authenticates, initializes the upstream server, reads all
`tools/list` pages, and verifies the pinned descriptors against the live
catalog. The effective catalog is always the live catalog intersected with the
profile allowlist. New upstream tools are never exposed automatically, and
security-relevant drift fails closed with `operation_contract_mismatch`.

The legacy downstream `submit` schema advertises the allowed operation names as
an enum and emits bounded deterministic operation signatures in its
description. Runtime validation against the selected client-visible and
upstream schemas is authoritative. Rich tagged-union schema projection is a
later negotiated optimization, not a correctness dependency.

### D9. Operation descriptors select the execution strategy

The initial strategies are:

- `upstream_task`: current `/mcp/app` `message`; Link polls and reattaches to
  Tama's server-side durable submission;
- `local_replayable`: read-only or proven-idempotent `/mcp/system` operations;
  a leased Link worker makes an ordinary `tools/call` and may replay it after an
  interrupted lease;
- `local_guarded`: a synchronous mutation with a reviewed conflict/read-back
  reconciliation contract; automatic replay is forbidden until reconciliation;
- `unsupported`: reject before any upstream mutation.

The existing Tama `mcp_submissions` table remains App graph infrastructure. It
is not generalized for System inspection. The initial profile marks
`reflection.comments.review` as `unsupported`; D15 defines the evidence needed
before a later profile may mark it `local_guarded`.

### D10. Instructions have explicit provenance

Tama Link owns only the cross-operation workflow: submit once, retain the local
identifier, await until terminal, honor polling guidance, and never infer an
endpoint from arguments. Each profile supplies the pinned bounded upstream
server instructions and operation descriptions. Product skills add domain
judgment such as when to remember information or how to perform Reflection
review; they do not duplicate schemas or protocol invariants.

### D11. App argument projection uses declarative bindings only

An operation may expose a client-visible schema different from the upstream
schema. Bindings use reviewed sources and JSON Pointer targets rather than
arbitrary executable transformations. For App `message`, a profile may bind:

```text
client_request_id        -> /identifier
client_context.thread_id -> /thread/identifier
```

The client-visible `arguments` then contains `recipient` and `content`. A
passthrough profile may instead expose `identifier` and `thread.identifier`
directly. Tama Link must never invent a conversation identity; the installer,
plugin, skill-driven caller, or explicit arguments must supply it. If an
operation binding requires `client_context.thread_id`, omission is an
`invalid_request`; there is no process-wide or profile-wide fallback thread.

For the projected App profile, client integrations own one stable opaque value
per host conversation and reuse it for every related submission. Codex and
OpenCode fixtures must demonstrate this behavior. A plain MCP client supplies
the value explicitly, while the passthrough profile remains available for
clients that already own the complete upstream `thread.identifier` argument.

### D12. Oversized results fail locally and are never truncated

The initial default limits are:

- 1 MiB for validated canonical arguments, with a maximum JSON nesting depth of
  32;
- 16 MiB for one upstream response body and 8 MiB for the normalized terminal
  result stored by Tama Link;
- 16 KiB for one progress event, retaining at most 128 events and at most 1 MiB
  of event data per submission; and
- seven days for terminal payload retention, followed by a payload-free
  `submission_expired` tombstone retained for 30 days after completion.

A profile may lower these defaults. Raising them requires explicit values
within implementation hard ceilings and a reconciled profile digest.

If Tama Link observes a completed upstream operation but cannot store its
normalized result within the limit, the local submission becomes terminal
`failed` with `result_too_large`. Safe details record that the upstream outcome
was completed but the result was not captured. Tama Link never stores or
returns a truncated terminal result. If a non-replayable synchronous mutation
loses its response before its outcome is known, it remains `outcome_unknown`
rather than being mislabeled `result_too_large`.

### D13. Current Tama task expiry and refresh behavior are explicit

In current Tama `0.14.0-server`, `task.ttl` is measured from task creation and
bounds the lifetime of both pending access and terminal-result retrieval.
Expired task rows are removed, and a terminal Submission does not complete an
already expired task (`lib/tama/mcp/task/expiration.ex` and
`lib/tama/mcp/task/persistence.ex`). The App adapter therefore polls at the
advertised interval, retrieves a terminal result immediately, and reattaches
through the canonical idempotent request when a session or task expires.

Current Tama retains the same refresh token across refresh exchanges and its
tests permit concurrent refreshes (`test/tama/token/exchange_test.exs` and
`test/tama/token/exchange_race_test.exs`). It does not rotate the token on every
use. Tama Link still coordinates refresh attempts with a profile-scoped SQLite
lease, re-reads the keyring value after acquiring the lease, and atomically
stores a replacement if a future compatible server returns one. One
`invalid_grant` becomes `authentication_required`; it is not retried in a loop.

### D14. Encrypted state has a fail-closed lifecycle

A new empty profile database with no state key gets a random profile-scoped key
in the configured secure credential backend. The plaintext database metadata
stores only the encryption format version and a non-secret key identifier.

If an existing database contains encrypted state and its key is missing or the
credential backend is unavailable, `serve` fails with `state_unavailable` and
`doctor` reports the cause. Tama Link must not generate a replacement, delete
the database, overwrite unreadable rows, or fall back to plaintext. `logout`
removes OAuth credentials but never the state-encryption key.

Version 1 has no automatic key rotation. A future rotation command must retain
the old key until every encrypted row is transactionally rewritten and the new
key identifier is committed. Headless Linux is supported only when an
explicitly configured secure credential backend is available; there is no
silent file or environment-variable fallback.

### D15. The initial System profile is read-only/replayable

Current `reflection.comments.review` uses `expected_state_version`, which
prevents a stale duplicate transition, but its read-back surface cannot prove
that this Tama Link submission rather than a concurrent actor performed a
transition after a lost response. It is therefore `unsupported` in the initial
production System profile.

Enabling it later requires an upstream idempotency/correlation value that is
returned by review and retrievable during reconciliation, plus interrupted-call
acceptance tests. The presence of the tool in upstream `tools/list` alone is
not sufficient to allow it.

## Package layout

```
cmd/tama-link/            serve, doctor, version (CLI wiring)
internal/version/         build version string
internal/server/          downstream STDIO MCP server; submit/await handlers
internal/contract/        stable tool input/output + error types (compat API)
internal/submission/      normalized state machine, idempotency, progress model
internal/catalog/         descriptors, schema projection, drift verification
internal/profile/         profile loading, validation, bindings, instructions
internal/store/           encrypted SQLite state, migrations, leases, GC
internal/worker/          leased local replayable/guarded execution
internal/credential/      platform keyring wrapper (secrets only)
internal/oauth/           discovery, auth-code+PKCE, single-flight refresh
internal/upstream/        reviewed JSON-RPC/streamable-HTTP client (task protocol)
internal/adapter/tama014/ current Tama (0.14.0) adapter: submit/await/normalize
internal/limits/          size, rate, timeout, retry, retention bounds
```

## Phase 0 — finish the repository foundation

- Wire `serve --profile <name>` (profile flag required for `serve`);
- reserve `login --profile <name>` and `logout --profile <name>` CLI wiring;
- add `version --json`;
- update usage text;
- confirm `make check` and CI pass.

Exit: `tama-link serve --profile x` starts the two-tool STDIO server and fails
cleanly if the profile is missing; `version --json` emits deterministic JSON.

## Phase 1 — normalized domain, profile, and durable state

- `internal/submission`: state machine
  (`accepted -> queued -> running -> {completed, failed, cancelled, expired,
  outcome_unknown}`)
  with transition guards; terminal states are absorbing. Error taxonomy types.
  Progress event model with monotonic `sequence` and dedupe by
  (submission, sequence).
- `internal/profile` and `internal/catalog`: load a named profile from the
  platform user configuration directory, validate a versioned JSON schema,
  origin/URL, single-endpoint identity, pinned instructions, descriptor
  allowlist, client-visible/upstream schemas, declarative bindings, execution
  strategies, digests, and timeouts/limits/retention.
- `internal/store`: SQLite store (D2/D3). Tables for submissions and an
  idempotency index (`client_request_id` → canonical input hash → submission).
  Sensitive blobs encrypted with a profile key. Atomic writes, migrations,
  `0600` permissions, no symlink traversal, WAL/busy handling, transactional
  worker leases, the D12 payload/tombstone retention policy, and GC that never
  removes a non-terminal submission.
- `internal/credential`: keyring wrapper and D14 state-key lifecycle (D4).
- `internal/worker`: multi-process-safe claim, lease renewal, recovery, and
  terminal capture for local execution strategies.
- Tests: state transition matrix (including no re-entry from terminal),
  idempotency hit and conflict, encrypted-blob round trip, missing-key failure,
  size/retention boundaries, and atomic write safety. A helper subprocess suite
  must start separate OS processes against one database and cover simultaneous
  migrations, competing idempotent inserts, busy timeout, exclusive worker
  claim, lease recovery after process death, terminal capture racing with GC,
  refresh leasing, WAL recovery, and `PRAGMA integrity_check`.

Exit: a submission can be created, made idempotent, transitioned, persisted,
and recovered after an in-process store reopen, with no upstream involved. The
storage subset of the separate-process suite passes before Phase 1 is complete.

## Phase 2 — current Tama (0.14.0) adapters

- `internal/upstream`: reviewed JSON-RPC 2.0 client over streamable HTTP
  (initialize handshake, `Mcp-Session-Id`, `application/json` and SSE
  responses, bearer auth) supporting `tools/call` (with `params.task`),
  `tasks/get`, and `tasks/cancel`. Fixture-driven tests.
- `internal/oauth`: RFC 9728 protected-resource discovery → RFC 8414 AS
  metadata, dynamic client registration, auth-code + PKCE S256 with the
  ephemeral loopback redirect (D5), and current stable-token refresh coordinated
  across processes as specified by D13.
- `internal/adapter/tama014`:
  - common connect → initialize → read all `tools/list` pages → verify the
    profile catalog and instructions before executing an operation;
  - App `submit` → validate client-visible arguments → apply declarative
    bindings → validate complete upstream arguments → task-augmented
    `tools/call` → persist submission + task ID;
  - `await` → bounded long-poll via `tasks/get` using the upstream poll
    interval → normalize state/progress → capture terminal result locally
    immediately and before the task lifetime expires (D6/D13);
  - restart recovery / re-attach (D3): reopen the upstream session and re-issue
    the persisted idempotent request to obtain a fresh session-scoped task ID;
    never reuse a stale task ID.
- `internal/worker` System path:
  - claim a `local_replayable` submission with a transactional lease;
  - issue an ordinary `/mcp/system` `tools/call` and capture the complete MCP
    result in the same local submission;
  - replay an interrupted read-only/idempotent operation after lease expiry;
  - reject guarded mutations in the initial profile (D15).
- Record and validate the negotiated protocol version at connect; unsupported
  combinations fail closed with `protocol_mismatch`.
- Result normalization + size validation; explicit upstream→normalized error
  and state mapping table: a captured `CallToolResult` is `completed` and
  preserves its `isError`; transport/protocol/local failures are `failed`.
- Fixture tests from `memovee/tama/graph/AGENT-INSTRUCTIONS.md` and
  `memory-contract.v1.json`; live integration against the local `memovee/tama`
  compose stack (Tama `0.14.0-server` + Memovee AS).

Exit: the full `submit` → repeated `await` → terminal workflow works against
separate live App and System profiles. App recovery does not duplicate graph
work; System read-only recovery safely replays interrupted local work.

## Phase 3 — client progress and acceptance

- Downstream MCP progress-token support: read `_meta.progressToken` on `await`,
  emit rate-limited `notifications/progress` correlated to that request
  (Go SDK `GetProgressToken` + `ServerSession.NotifyProgress`).
- `login --profile <name>`: explicit user-visible browser authorization with
  PKCE and a single-use loopback callback; `serve` never opens a browser;
- `logout --profile <name>`: selected-profile credential removal/revocation
  without deleting durable submissions;
- `doctor --profile <name> [--json]` (read-only): profile validation, metadata
  fetch, credential presence, state integrity, non-mutating upstream reachability.
- Disconnect, restart, timeout, and live-OAuth acceptance tests.
- Client-package acceptance: Codex, OpenCode, and a plain MCP inspector register
  the same binary twice and complete the `submit`/repeated-`await` workflow
  against both profile types. App fixtures prove that each integration supplies
  one stable `client_context.thread_id` per conversation and never falls back to
  a profile-global value.

Exit: acceptance criteria #1–#20 of the spec are demonstrated by automated and
live tests.

## Phase 4 — newer (2026-07-28) MCP adapter

- A second upstream adapter built on the pinned `go-sdk v1.7.0`, which natively
  speaks `2026-07-28`: stateless per-request `_meta`, `tasks/get` polling via
  custom method registration, optional `subscriptions/listen` as an
  optimization, and MRTR `inputResponses` for `input_required`.
- Downstream `submit`/`await` contract unchanged.
- Define the `input_required` contract behavior (G11) before implementation;
  the SDK's MRTR mechanism is the upstream transport for an `await`
  `input_response`.
- Expand the published compatibility matrix only after live client tests.

Exit: the 2026-07-28 path is selectable by profile/adapter version and passes
the same acceptance suite; the 0.14.0 path remains the default.

## Resolved specification amendments

The current specification revision incorporates the decisions previously
tracked as G1-G7: the reviewed upstream JSON-RPC exception, canonical-request
recovery, the OAuth loopback exception and explicit login command, completed
versus failed result semantics, initial state-only progress, profile isolation
and catalog projection, and declarative App idempotency/context bindings.

The current revision also resolves G8-G10 and G12-G15:

- **G8:** bounded results fail as `result_too_large` and are never truncated;
- **G9:** current Tama task TTL is a task/result-access lifetime measured from
  creation; live compatibility remains an acceptance gate;
- **G10:** current Tama uses stable refresh tokens; the Link refresh lease and
  replacement-token handling remain future-compatible;
- **G12:** the guarded Reflection mutation is excluded until upstream
  correlation makes exact reconciliation possible;
- **G13:** projected App profiles require explicit caller-owned conversation
  identity, with client fixtures required before acceptance;
- **G14:** missing or unavailable encryption keys fail closed and v1 does not
  rotate them automatically; and
- **G15:** the architecture is fixed and a separate-process stress suite is a
  release gate.

## Remaining gates and deferred work

1. **G11 (Phase 4) — `input_required` has no client-facing path.** The
   2026-07-28 profile has an `input_required` task state answered via
   `tasks/update`, but the two-tool contract has no way for a client to answer
   an input request. The preferred direction is an optional, request-correlated
   `input_response` on `await`, preserving exactly two downstream tools. Finalize
   its schema and idempotency behavior against `tama-mcp` fixtures before Phase
   4; do not represent it as ordinary running progress indefinitely.

2. **Current-adapter live acceptance:** against the named local `memovee/tama`
   Compose environment and pinned Tama `0.14.0-server`, verify initialization,
   negotiated protocol, the 60-second task lifetime, reattachment, terminal
   capture, and production ingress availability.

3. **Client identity acceptance:** Codex, OpenCode, and plain MCP fixtures must
   prove the D11 conversation-identity contract. Until they pass, retain the
   passthrough App profile.

4. **Production storage acceptance:** complete the full D14 and Phase 1
   separate-process, platform-keyring, headless-backend, and crash-recovery
   matrix on every supported operating system before the first production
   release.

None of these gates blocks Phase 0. The storage subset blocks completion of
Phase 1; current-adapter and client-identity acceptance block completion of
Phases 2 and 3 respectively; G11 blocks only Phase 4.

## Suggested order of attack

1. Finish Phase 0 and land the versioned profile/catalog schema.
2. Implement Phase 1, including encryption and multi-process lease tests.
3. Implement the System read-only adapter first; it proves local execution
   without depending on upstream Tasks.
4. Implement the App task adapter against the local `memovee/tama` stack and
   complete the current-adapter live gate.
5. Complete dual-registration and conversation-identity client acceptance.
6. Begin Phase 4 only once `tama-mcp` Phases 2–3 and the Tama
   `/mcp/app` migration land.
