# Tama Link Implementation Plan

Status: implementation plan (supersedes nothing; refines `../tama-link-specification.md`)

This plan sequences the work required to satisfy the Tama Link specification and
records the concrete decisions the spec leaves open. It is grounded in the
surrounding repositories this proxy actually integrates with:

- `tama-mcp` — the MCP `2026-07-28` server library and upstream wire contract
  for Phase 2; this plan is reconciled to specification commit
  `6b5db00018d2774834db5a0f00eed5b9b55e1d2e`;
- `tama-oauth` — protocol library for the OAuth mechanics (PKCE S256,
  `private_key_jwt`, RFC 7662 introspection, refresh-token lifecycle);
- `memovee` — the authorization server (issuer `https://app.localhost`) and the
  host of the local Tama topology;
- `upmaru/tama#123` and the later endpoint migration — the Tama-owned Ecto,
  durable runner, Phoenix PubSub, OAuth composition, and application routing
  needed to expose the TamaMCP contract;
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
  URL validation, portable profile-isolated database references and credential
  namespaces, effective-limit resolution, and canonical whole-profile digest
  enforcement whenever a profile raises a default limit;
- `internal/server`: profile-driven MCP server wiring the catalog projection,
  instructions, and the unconstrained submit `tool` field (retries of
  reconciled-away tools must reach the application);
- `internal/store`: encrypted SQLite state (D2/D3/D4) with complete-input
  idempotency, compare-and-set transitions, bounded progress, atomic terminal
  capture, lease-guarded worker writes, payload-free D12 GC, and exact schema
  and encryption-format validation; first-open metadata is atomic under
  concurrent access, incomplete declared schemas fail closed, SQLite sidecars
  are secured before WAL is enabled, and ciphertext is bound to its submission
  and semantic field;
- `internal/credential`: a profile-scoped platform keyring restricted to the
  secure OS backend, with missing and unavailable states kept distinct and
  fail-closed store integration;
- `internal/worker`: owned lease renewal, cancellation, safe terminal failure,
  single-winner execution, and restart recovery for `local_replayable` work;
- the separate-process storage test suite (concurrent initialization, competing
  idempotent inserts, busy timeout, exclusive claim, lease recovery after
  death, terminal capture surviving GC, refresh leasing, WAL recovery,
  integrity), plus worker cancellation and recovery coverage.

Phase 2 is next. The downstream `submit` and `await` handlers deliberately
remain placeholders until the MCP `2026-07-28` System and App adapter is wired;
Phase 1's no-upstream exit criteria are satisfied independently of that work.

## Key architectural decisions (resolving spec open questions)

### D1. The upstream side speaks only MCP 2026-07-28

Tama's replacement MCP server is the TamaMCP runtime. Its upstream contract is
stateless MCP `2026-07-28`; it deliberately rejects legacy initialization,
protocol sessions, `Mcp-Session-Id`, client-requested task augmentation,
`tasks/result`, and `tasks/list`.

Phase 2 uses a reviewed stable release of the official Go MCP SDK for the core
`2026-07-28` transport, `server/discover`, `tools/list`, `tools/call`, and
`subscriptions/listen` behavior it exposes. The pinned `go-sdk v1.7.0` is the
current implementation baseline. Its public client API has no Tasks extension
types, and its core subscription type cannot request task IDs. Before coding,
issue #2 must re-audit the selected stable SDK. If those gaps remain, add one
focused `internal/upstream/tasks` extension layer for `tasks/get`,
`tasks/update`, `tasks/cancel`, task-ID subscriptions, and
`notifications/tasks`. That layer must reuse the SDK's core wire conventions
and must be checked against TamaMCP's pinned fixtures; it must not recreate a
legacy session client or fork general MCP behavior.

Every upstream request carries `MCP-Protocol-Version`, `Mcp-Method`, conditional
`Mcp-Name`, and the required per-request `_meta` protocol version, Link client
information, and capabilities. Task capability is declared on each applicable
request, never inferred from discovery or a previous call.

### D2. Local persistence is SQLite via a pure-Go driver

Use `modernc.org/sqlite` (pure Go) so the existing `CGO_ENABLED=0` cross-builds
keep working. Each profile uses an isolated database. The main database and WAL
sidecars are validated or securely created without following links inside the
private profile directory. Every state-path component is resolved while its
parent directory handle is pinned and links are rejected. On Windows, child
directories, the database, and both sidecars are opened relative to their
pinned parent handles; all ancestor handles remain open while SQLite uses the
absolute path. Existing declared schemas are validated before use and are never
repaired with
`CREATE TABLE IF NOT EXISTS`. The state store holds the minimum durable state
the spec permits and adds the **canonical upstream request** (see D3). SQLite
is authoritative for Tama Link's
client-facing submission lifecycle and for locally executed System operations;
it does not replace Tama's existing durable App graph submission.

### D3. Persist the owner-bound task ID and canonical upstream request

TamaMCP task IDs are opaque, globally unique, durable, and independent of an
HTTP connection or process. Every task request is authenticated independently,
and Tama binds lookup to an application-defined owner derived from the validated
principal and protected resource. After restart, Tama Link retrieves the same
task ID through `tasks/get`; it does not open a protocol session or replace a
stale session-scoped task ID.

The canonical upstream request remains required for Link-side idempotency and
for the narrow ambiguous-acceptance case where the initial `tools/call` may have
reached Tama but its task handle was not received. Replaying that request is
allowed only while Tama's application adapter preserves the existing Message
Submission idempotency contract and proves that replay cannot duplicate graph
work.

Therefore the state store must persist, per submission:

- the local `submission_id` and `client_request_id`;
- the upstream tool name, selected execution strategy, descriptor digest, and
  the **validated, canonicalized arguments** (the exact recoverable request);
- the durable upstream task ID once received and the owner/profile binding
  needed to make an independently authenticated lookup;
- normalized state, timestamps, progress cursor and bounded events;
- the terminal result or structured failure within retention limits;
- the accepted response, result, event, and retention limits that govern the
  submission for its complete lifecycle;
- the negotiated protocol version and adapter version.

This resolves restart recovery without making protocol sessions part of local
state.

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

The listener port is selected before the authorization URL exists. That exact
redirect URI is carried in the authorization request, matched against the
observed callback, and resent verbatim in the token exchange; the
authorization-code grant requires the two values to be equal, so any mismatch
is rejected before a token request is sent.

Client registration is revalidated against the auth-method secret
requirement on load, and the RFC 7591 `client_secret_expires_at` is
persisted and enforced: a record that lacks the secret its method requires,
or whose secret has expired, is absent for readiness and refresh, and the
next login re-registers. A replacement registration issues a new client ID,
so the orphaned credential from the previous client and the new record are
committed under one refresh-lease epoch, and a completion that started from
the previous record re-reads the stored record before the exchange and again
before the fenced commit: it either commits before the replacement (the
grant is then retired) or aborts, never pairing the new client with a grant
issued under the old one. The registration mutation holds the local lock
that orders completions, refreshes, and logouts — a same-owner lease claim
never advances the epoch, so the lease alone cannot order in-process
mutations — renews the epoch across the whole mutation, outliving caller
cancellation because the final record write takes no context and cannot
be aborted, and checks the epoch on both sides of that write: before it,
and after it, where a lost epoch removes the stale record the write may
have stored, so the replacement can never be read as a ready pair.

### D6. Terminal results are captured immediately and owned locally

TamaMCP includes the complete state-specific payload, including a terminal
`CallToolResult`, in `tasks/get` and `notifications/tasks`. Tama Link captures a
terminal result into the local store **the moment it is observed** through
either path, so the result survives Tama Link restarts, upstream retention, and
client disconnects. Repeated `await` after terminal capture is served entirely
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
output schema, annotations, expected task/execution strategy, declarative
bindings, and digest.

At runtime Tama Link authenticates, calls `server/discover`, reads the complete
`tools/list` result, and verifies the pinned descriptors against the live
catalog. The effective catalog is always the live catalog intersected with the
profile allowlist. New upstream tools are never exposed automatically, and
security-relevant drift fails closed with `operation_contract_mismatch`.

TamaMCP selects task execution from its application-owned tool policy and the
Link's per-request capabilities; the `2026-07-28` `tools/list` response does not
advertise the old `execution.taskSupport` field. The profile still pins Link's
expected strategy. The adapter verifies the Tasks capability through
`server/discover` and enforces the `tools/call` `resultType`; it must not
interpret absent legacy task metadata as `forbidden`.

The legacy downstream `submit` schema does not constrain the tool name to
the current catalog: an exact retry whose tool the profile later removed
must reach the application, where idempotency reconciliation precedes the
catalog check that enforces the profile for genuinely new work. Bounded
deterministic operation signatures in the description advertise the approved
operations. Runtime validation against the selected client-visible and
upstream schemas is authoritative. Rich tagged-union schema projection is a
later negotiated optimization, not a correctness dependency.

The idempotency identity is the client-visible request: the tool name and
the canonical arguments plus the client thread ID in one of three shapes —
its string value, an explicit JSON null when the accepted operation could
map the source but the request carried no value, or an omitted field when
it could not. A retry cannot know which shape the accepted request took,
so recovery matches candidates against the stored identity: a retry that
carries a value matches its value-shape or the omitted-field identity, and
a retry that carries no value matches the explicit-null or the omitted-
field identity. Adding a value after an accepted request omitted one from a
mappable source conflicts; changing a value the accepted operation never
mapped reconciles. The same candidate matching applies to the atomic create
path: when a concurrent process claims the key and the insert loses, the
loser reconciles the winner's row against the candidates instead of
reporting a conflict for an otherwise exact request.

### D9. Operation descriptors select the execution strategy

The initial strategies are:

- `upstream_task`: `/mcp/app` `message`; Link subscribes for hints, reconciles
  through owner-bound `tasks/get`, and captures the detailed terminal state;
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
within implementation hard ceilings and a reconciled profile digest. The
digest is SHA-256 over canonical JSON for the complete non-secret profile with
the digest field omitted. Tama Link verifies every supplied digest and requires
one whenever any effective limit exceeds its version 1 default. Lowered limits
apply only to new acceptance: exact retries are canonicalized within the hard
ceilings and reconciled against the durable idempotency index first, so a
profile update cannot strand already accepted work. Each accepted submission
retains its response, result, event, and retention limits across restarts.

If Tama Link observes a completed upstream operation but cannot store its
normalized result within the limit, the local submission becomes terminal
`failed` with `result_too_large`. Safe details record that the upstream outcome
was completed but the result was not captured. Tama Link never stores or
returns a truncated terminal result. If a non-replayable synchronous mutation
loses its response before its outcome is known, it remains `outcome_unknown`
rather than being mislabeled `result_too_large`.

### D13. Task polling, subscriptions, expiry, and refresh are explicit

The upstream task supplies its bounded `ttlMs` and `pollIntervalMs`. Tama Link
uses `subscriptions/listen` for prompt task snapshots and `tasks/get` as the
source-of-truth recovery path. There is no replay guarantee: after every stream
disconnect, overflow, authorization expiry, or policy invalidation, Link
reauthorizes, calls `tasks/get`, and opens a new subscription for the still
authorized task IDs. Subscription acknowledgement must precede notifications,
and an unacknowledged task remains on the polling path.

Expiry of non-terminal upstream work is represented by the task's durable
failed state and bounded expiration error. Link captures that state locally;
it must not infer expiry from a lost stream or elapsed client wait alone.

Tama Link coordinates OAuth refresh attempts with a profile-scoped SQLite
lease, re-reads the keyring value after acquiring the lease, and atomically
stores a replacement refresh token when returned. One `invalid_grant` becomes
`authentication_required`; it is not retried in a loop. A subscription closes
no later than credential expiry and is reopened only after successful
reauthorization.

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

### D16. `await` carries request-correlated input responses

The TamaMCP task states include `input_required`, and `tasks/update` accepts a
map of input-request IDs to responses. Tama Link preserves exactly two
downstream tools by extending `await` with optional `input_responses` and by
returning the outstanding `input_requests` in an `input_required` pending
response.

One `await` call submits at most one `tasks/update` before it begins its bounded
wait. Partial response maps are allowed. Tama Link records each accepted
input-request ID and canonical response: an exact replay is idempotent, a
different response for the same ID is `idempotency_conflict`, and a response
for a task or request that is not outstanding is `invalid_request`.
`tasks/update` acknowledgement is eventually consistent, so Link continues
with `tasks/get` or subscription reconciliation rather than assuming the task
already left `input_required`.

Link declares only the per-request input capabilities that this downstream
contract and selected profile can actually relay. It must not advertise an
elicitation or sampling sub-capability merely because the upstream server can
produce that input request.

## Package layout

```
cmd/tama-link/            serve, doctor, version (CLI wiring)
internal/version/         build version string
internal/server/          downstream STDIO MCP server; submit/await handlers
internal/contract/        stable tool input/output + error types (compat API)
internal/submission/      normalized state machine, idempotency, progress model
internal/catalog/         descriptors, schema projection, drift verification
internal/profile/         profile loading, validation, bindings, instructions
internal/store/           encrypted SQLite state, schema validation, leases, GC
internal/worker/          leased local replayable/guarded execution
internal/credential/      platform keyring wrapper (secrets only)
internal/oauth/           discovery, auth-code+PKCE, single-flight refresh
internal/upstream/        official SDK core plus focused Tasks/subscription extension
internal/adapter/tama2026/ MCP 2026-07-28 submit/await/normalize adapter
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
  Sensitive blobs encrypted with a profile key. Atomic writes, initialization,
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
  first opens, competing idempotent inserts, busy timeout, exclusive worker
  claim, lease recovery after process death, terminal capture racing with GC,
  refresh leasing, WAL recovery, and `PRAGMA integrity_check`.

Exit: a submission can be created, made idempotent, transitioned, persisted,
and recovered after an in-process store reopen, with no upstream involved. The
storage subset of the separate-process suite passes before Phase 1 is complete.

## Phase 2 — TamaMCP 2026 upstream adapter

- `internal/upstream`: use the official Go SDK for the stateless MCP
  `2026-07-28` core and add only the focused Tasks/subscription extension seam
  D1 permits. Support `server/discover`, `tools/list`, `tools/call`,
  `tasks/get`, `tasks/update`, `tasks/cancel`, `subscriptions/listen`, and
  `notifications/tasks`. Reject legacy initialization, `Mcp-Session-Id`,
  client-requested task augmentation, `tasks/result`, and `tasks/list`.
- Every request includes exact `MCP-Protocol-Version`, `Mcp-Method`, conditional
  `Mcp-Name`, and matching per-request `_meta`. Fixture tests cover header/body
  agreement, Base64 sentinel handling, capability failures, supported response
  content types, body bounds, cancellation, and the fixed protocol error codes.
- `internal/oauth`: RFC 9728 protected-resource discovery → RFC 8414 AS
  metadata, dynamic client registration, auth-code + PKCE S256 with the
  ephemeral loopback redirect (D5), and provider-token refresh coordinated
  across processes as specified by D13.
- `internal/adapter/tama2026`:
  - authenticate → `server/discover` → read the complete `tools/list` result →
    verify the profile catalog and instructions before executing an operation;
  - App `submit` → validate client-visible arguments → apply declarative
    bindings → validate complete upstream arguments → `tools/call` with the
    per-request Tasks capability → persist the returned owner-bound task ID;
  - `await` → optionally send one idempotent `tasks/update` → reconcile through
    `tasks/get` using `pollIntervalMs` → use authorized task-ID subscriptions as
    a prompt-update optimization → normalize and immediately capture the
    complete detailed terminal result from either path (D6/D13/D16);
  - restart recovery (D3): authenticate as the same profile owner and query the
    persisted task ID; replay the canonical `tools/call` only for the proven
    ambiguous initial-acceptance case where no task ID was received.
- `internal/worker` System path:
  - claim a `local_replayable` submission with a transactional lease;
  - issue an ordinary `/mcp/system` `tools/call` and capture the complete MCP
    result in the same local submission;
  - replay an interrupted read-only/idempotent operation after lease expiry;
  - reject guarded mutations in the initial profile (D15).
- Record and validate the discovered protocol/server capabilities and adapter
  version; unsupported combinations fail closed with `protocol_mismatch`.
- Result normalization + size validation; explicit upstream→normalized error
  and state mapping table: a captured `CallToolResult` is `completed` and
  preserves its `isError`; transport/protocol/local failures are `failed`.
- Import and run the relevant package-provided TamaMCP conformance fixtures,
  including task states, owner isolation, headers, subscriptions, reconnect,
  and rejection of removed legacy methods. Run live integration only against a
  Tama build that has completed the TamaMCP persistence, runner, PubSub, System,
  and App migration.

Exit: the full `submit` → repeated `await` → terminal workflow works against
separate live App and System profiles. App recovery does not duplicate graph
work; a Link restart resumes the same owner-bound task ID; System read-only
recovery safely replays interrupted local work; an `input_required` task can be
answered through `await`; and dropped subscription notifications recover
through `tasks/get`.

### Phase 2 execution work breakdown

GitHub issue [#9](https://github.com/kritama/tama-link/issues/9) tracks the
phase and its dependency order:

1. Build the MCP `2026-07-28` core and Tasks/subscription transport
   ([#2](https://github.com/kritama/tama-link/issues/2)) and OAuth services
   ([#3](https://github.com/kritama/tama-link/issues/3)) as independent
   foundations.
2. Establish the shared TamaMCP discovery, capability, and pinned
   catalog boundary ([#4](https://github.com/kritama/tama-link/issues/4)).
3. Implement owner-bound App task execution, subscriptions, input updates, and
   restart recovery
   ([#5](https://github.com/kritama/tama-link/issues/5)) and replayable System
   execution ([#6](https://github.com/kritama/tama-link/issues/6)) on that
   boundary.
4. Wire the thin downstream `submit` and `await` handlers
   ([#7](https://github.com/kritama/tama-link/issues/7)).
5. Close the phase only after fixture and live acceptance against the pinned
   local Memovee/Tama topology
   ([#8](https://github.com/kritama/tama-link/issues/8)).

Phase 2 is gated by the package and application layers it consumes. TamaMCP
Phase 2 is complete; task subscriptions remain tracked by
`kritama/tama-mcp#9`. Live Link acceptance additionally waits for the
Tama-owned adapters and endpoint migration beginning with `upmaru/tama#123`.

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

Exit: acceptance criteria #1–#22 of the spec are demonstrated by automated and
live tests.

## Phase 4 — production release and migration closure

- Complete the platform credential-backend and crash-recovery matrix for every
  operating system claimed by the release.
- Publish the compatibility matrix covering the exact TamaMCP, Tama, Tama Link,
  OAuth/profile, protocol, OS/architecture, and verified client versions.
- Coordinate final acceptance evidence with Tama before its Anubis endpoint and
  compatibility projections are removed; old and new runtimes must never
  mutate the same task.
- Cut the independent Tama Link release through the configured Git Flow and
  provide the immutable version/checksum consumed by Memovee CLI.

Exit: the supported binary set and compatibility matrix are published, Tama's
legacy runtime can be removed without losing a required Link path, and every
production gate has current evidence.

## Resolved specification amendments

The current specification revision incorporates the decisions previously
tracked as G1-G7: the upstream SDK/extension boundary, durable task-ID and
canonical-request recovery, the OAuth loopback exception and explicit login
command, completed versus failed result semantics, portable progress, profile
isolation and catalog projection, and declarative App idempotency/context
bindings.

The current revision also resolves G8-G10 and G12-G15:

- **G8:** bounded results fail as `result_too_large` and are never truncated;
- **G9:** TamaMCP task TTL/polling values and detailed durable state are
  authoritative; Link captures terminal results locally and reconciles dropped
  subscriptions through `tasks/get`;
- **G10:** the Link refresh lease and replacement-token handling remain correct
  independently of whether the current provider rotates refresh tokens;
- **G12:** the guarded Reflection mutation is excluded until upstream
  correlation makes exact reconciliation possible;
- **G13:** projected App profiles require explicit caller-owned conversation
  identity, with client fixtures required before acceptance;
- **G14:** missing or unavailable encryption keys fail closed and v1 does not
  rotate them automatically; and
- **G15:** the architecture is fixed and a separate-process stress suite is a
  release gate; and
- **G11:** `await.input_responses` carries request-correlated responses to
  `input_required` tasks without adding a third downstream tool (D16).

## Remaining gates and deferred work

1. **TamaMCP subscription dependency:** package Phase 3 issue
   `kritama/tama-mcp#9` must complete the task-ID subscription and notification
   contract before Link can claim its subscription path. Polling work may
   proceed first because `tasks/get` is the recovery source of truth.

2. **Tama application migration:** `upmaru/tama#123` and the subsequent System,
   App, OAuth composition, and Phoenix PubSub endpoint migration must expose the
   new runtime. Link fixture work may proceed against package conformance data,
   but live acceptance waits for that application surface.

3. **TamaMCP adapter live acceptance:** against the named local `memovee/tama`
   Compose environment and an immutable migrated Tama build, verify
   `server/discover`, standard headers, per-request authorization/capabilities,
   owner-bound task recovery, `tasks/update`, subscription acknowledgement and
   reconnect, terminal capture through `tasks/get`, and production ingress.

4. **Client identity acceptance:** Codex, OpenCode, and plain MCP fixtures must
   prove the D11 conversation-identity contract. Until they pass, retain the
   passthrough App profile.

5. **Production storage acceptance:** complete the full D14 and Phase 1
   separate-process, platform-keyring, headless-backend, and crash-recovery
   matrix on every supported operating system before the first production
   release.

None of these gates blocks Phase 0. The storage subset blocks completion of
Phase 1; TamaMCP/Tama migration and live acceptance block completion of Phase 2;
client identity acceptance blocks Phase 3; production certification blocks
Phase 4.

## Suggested order of attack

1. Finish Phase 0 and land the versioned profile/catalog schema.
2. Implement Phase 1, including encryption and multi-process lease tests.
3. Implement the official-SDK core plus the smallest conformance-tested Tasks
   extension seam while TamaMCP Phase 3 and the Tama migration proceed.
4. Implement the System synchronous adapter first; it proves the new stateless
   transport without depending on durable Tasks.
5. Implement App task polling, `input_required`, subscriptions, and restart
   recovery; then complete the migrated-Tama live gate.
6. Complete dual-registration and conversation-identity client acceptance.
7. Begin production release closure only after TamaMCP Phase 3, the Tama
   System/App migration, and client acceptance land.
