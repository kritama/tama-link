# Tama Link Compatibility Proxy Specification

Status: implementation handoff

This document defines why Tama Link exists and the contract the first complete
implementation must satisfy. It is authoritative for the proxy boundary,
client-facing tools, upstream adaptation, durable state, progress, security,
configuration, testing, and release compatibility.

## Context and decision

Tama performs durable work whose lifetime can exceed one model turn, one MCP
request, or one client process. Modern MCP specifications provide asynchronous
task facilities, but coding clients adopt protocol revisions and optional
features at different times. Codex, OpenCode, Pi, and future clients therefore
cannot be assumed to expose MCP Tasks, task progress, elicitation, or MCP Apps
UI consistently.

Blocking inside Tama until a durable operation finishes would create the wrong
semantics:

- a client timeout or disconnect could be mistaken for execution failure;
- a completed Tama operation could lose its result delivery path;
- retries could create duplicate work without explicit correlation;
- client-specific timeout behavior would leak into Tama; and
- upgrading Tama's MCP library would remain coupled to the least-capable client.

Implementing the workaround independently in every client plugin would also be
incorrect. Each plugin would need to duplicate OAuth, protocol negotiation,
task correlation, polling, retries, progress normalization, terminal failure
semantics, and upgrade logic.

The accepted design is a local Go proxy named Tama Link. It is the single
compatibility boundary between a client-facing ordinary tool surface and both
task-backed and synchronous Tama MCP operations. Client integrations stay
deliberately thin.

## Goals

Tama Link must:

1. run locally as a single cross-platform Go binary;
2. expose an MCP server over STDIO by default;
3. expose exactly two client-facing MCP tools, `submit` and `await`;
4. submit work once and return a stable local submission identifier;
5. preserve correlation independently of a client connection;
6. wait for, poll, and retrieve terminal upstream results without conflating
   submission acceptance with graph completion;
7. preserve terminal failures as terminal results rather than endless pending
   responses;
8. normalize progress so basic clients can poll it and richer plugins can
   render it live;
9. negotiate supported MCP revisions on the upstream and downstream sides;
10. contain protocol-version and client-compatibility logic outside Tama;
11. protect OAuth credentials, tokens, result data, and logs; and
12. support deterministic diagnosis and compatibility testing;
13. adapt approved operations from either Tama `/mcp/app` or `/mcp/system`
    without translating one endpoint's operations into the other; and
14. project each profile's approved upstream catalog and instructions into the
    two-tool client-facing surface.

## Non-goals

Tama Link is not:

- an agent, model runner, or prompt framework;
- a replacement for Tama's execution graph or durable state;
- a Memovee authorization server or a reimplementation of `tama_oauth`;
- the installer for Memovee or Tama containers;
- a generic public MCP gateway or arbitrary URL proxy;
- the owner of client UI, skills, or host configuration;
- a reason to weaken Tama's OAuth or protected-resource validation;
- a shared secret store for Memovee, Tama, and client plugins; or
- permitted to claim success before an upstream terminal result exists.

Cancellation of a client-side `await` call initially cancels only that wait. It
must not cancel durable Tama work. A future upstream cancellation feature must
be introduced deliberately and must not add a third client-facing tool without
a contract revision.

## Ownership boundaries

| Component | Owns |
| --- | --- |
| Tama | Authorization enforcement, graph execution, durable App submission/task state, upstream tool behavior and results |
| Tama Link | Protocol adaptation, OAuth client behavior, local durable correlation and execution, polling, progress normalization, catalog projection, client-facing tool contract |
| Memovee CLI | Installing/upgrading the Tama Link binary, creating non-secret profiles, orchestrating the local product topology |
| `memovee-codex` | Codex MCP registration, skills, optional hooks, Codex-compatible progress presentation |
| `memovee-opencode` | OpenCode tool wrapper/configuration and native progress or panel rendering |
| Other client packages | Installation and presentation behavior specific to that client |

No client package may independently implement Tama task semantics. Client
packages consume the Tama Link contract and may only adapt configuration,
invocation, and presentation.

## Process and transport model

The initial server command is:

```text
tama-link serve --profile <name>
```

STDIO is the default and initially required downstream transport. Standard
output is reserved exclusively for MCP JSON-RPC frames. Diagnostics go to
standard error and must be structured, bounded, and redacted.

The initial implementation must not open a persistent TCP listener. OAuth login
may open one ephemeral single-use listener on `127.0.0.1` for an authorization
code callback. A persistent loopback HTTP, Unix-domain socket, or named-pipe
control surface requires a separate threat model and authentication design.

Tama Link has two logical sides:

```text
downstream client <-> client-facing MCP server
                         |
                         v
                 durable link state
                         |
                         v
upstream Tama   <-> negotiated MCP client/adapter
```

Disconnecting the downstream client must not reinterpret accepted upstream
work as failed or completed.

## Profile and client registration model

One Tama Link process serves exactly one named profile, and one profile selects
exactly one upstream MCP endpoint. A profile must not combine `/mcp/app` and
`/mcp/system` catalogs, credentials, instructions, or durable state.

A client connects to multiple Tama profiles by registering the same binary as
multiple named STDIO MCP servers:

```text
tama-app    -> tama-link serve --profile tama-app
tama-system -> tama-link serve --profile tama-system
```

Each process advertises exactly `submit` and `await`; the client registration
name provides the outer namespace. A client package or Memovee CLI installation
may create both registrations, but must preserve their distinct process
arguments and identities.

Each profile has a separate SQLite database and credential namespace. Profile
names must satisfy a restrictive portable identifier grammar and must never be
used as unchecked filesystem paths. Implementations use the platform user
configuration and state directories, with explicit overrides permitted for
tests and managed installations.

## Client-facing tool contract

The MCP tool catalog must contain exactly `submit` and `await`. Administrative
CLI commands such as `version` or `doctor` are not MCP tools.

Tool names and top-level field names are a compatibility API. Changing them
requires a versioned contract and migration plan.

### `submit`

Purpose: accept one allowed Tama operation, submit it at most once for the
provided idempotency key, and return promptly after durable acceptance.

Input schema:

```json
{
  "tool": "string, required",
  "arguments": "object, optional, defaults to {}",
  "client_request_id": "string, optional",
  "client_context": {
    "thread_id": "string, optional"
  }
}
```

Requirements:

- `tool` identifies an operation allowed by the selected profile. It must not
  supply an endpoint, origin, executable, or arbitrary transport destination.
- `arguments` is passed only after validation against the selected upstream
  operation's client-visible contract. Tama Link then applies only the
  declarative profile bindings and validates the complete upstream arguments
  against the pinned upstream contract.
- `client_request_id` is an opaque idempotency key scoped to the profile. When
  omitted, Tama Link generates one before the first upstream mutation.
- `client_context` carries client-owned correlation values that an operation
  binding may require but ordinary MCP does not provide automatically. It is
  not passed upstream unless a reviewed profile binding maps a field.
- If the selected operation has a required binding from
  `client_context.thread_id`, omission is `invalid_request`. Tama Link must not
  substitute a profile-global or process-global conversation identity.
- Repeating the same `client_request_id` with equivalent canonical input must
  return the original submission.
- Lowering a profile limit must not make an already accepted idempotent request
  unrecoverable. Tama Link canonicalizes and reconciles an existing request
  within implementation hard ceilings before applying current profile limits;
  those current limits govern only genuinely new acceptance.
- Reusing it with different input must fail with `idempotency_conflict`.
- Success means Tama Link durably accepted responsibility for the operation in
  its local store, not that Tama accepted or completed it.
- The response must not block until graph completion.

For the initial App `message` projection, the profile may bind
`client_request_id` to upstream `identifier` and
`client_context.thread_id` to upstream `thread.identifier`. Such bindings must
be explicit; Tama Link must not invent a conversation identity or assume that
an MCP request contains the host's conversation identifier. A passthrough
profile may instead expose those upstream fields directly in `arguments`.

Success output:

```json
{
  "submission_id": "sub_opaque",
  "status": "accepted",
  "client_request_id": "opaque-idempotency-key",
  "submitted_at": "RFC3339 timestamp",
  "next_poll_ms": 1000
}
```

`submission_id` is a Tama Link identifier. Raw upstream task identifiers and
tokens must not be treated as stable client API values.

### `await`

Purpose: long-poll one known submission and return either its current durable
state or its terminal result as an ordinary MCP tool response.

Input schema:

```json
{
  "submission_id": "string, required",
  "cursor": "string, optional",
  "timeout_ms": "integer, optional"
}
```

Requirements:

- `timeout_ms` is bounded by configuration. The initial recommended default is
  20 seconds and the maximum is 30 seconds.
- Returning because the wait budget elapsed is a successful pending response,
  not a tool error.
- `cursor` is opaque and allows the caller to request only progress events
  after the last observed sequence.
- A client may call `await` repeatedly until `terminal` is true.
- Cancellation or disconnection stops the active wait promptly without
  changing durable upstream work.
- Concurrent waits for the same submission must not duplicate upstream work.
- A successful terminal result must be returned exactly as stored after
  normalization and size validation.
- A failed, cancelled, or expired upstream operation must return a terminal
  state with a stable structured error.

Pending output:

```json
{
  "submission_id": "sub_opaque",
  "status": "running",
  "terminal": false,
  "cursor": "event_cursor",
  "progress": {
    "current": 2,
    "total": 4,
    "label": "Indexing memories",
    "message": "Processing 18 posts"
  },
  "events": [],
  "next_poll_ms": 1000
}
```

This is the rich-adapter shape. The initial current-Tama App adapter has no
structured upstream progress channel, so it emits state-transition events and
omits unknown `current`, `total`, and `label` values. System operations may
likewise expose only queued/running state unless the upstream tool provides
structured progress.

Successful terminal output:

```json
{
  "submission_id": "sub_opaque",
  "tool": "message",
  "status": "completed",
  "terminal": true,
  "cursor": "event_cursor",
  "result": {
    "is_error": false,
    "content": [],
    "structured_content": {}
  },
  "completed_at": "RFC3339 timestamp"
}
```

Failed terminal output:

```json
{
  "submission_id": "sub_opaque",
  "status": "failed",
  "terminal": true,
  "cursor": "event_cursor",
  "error": {
    "code": "upstream_execution_failed",
    "message": "Safe user-facing summary",
    "retryable": false
  },
  "completed_at": "RFC3339 timestamp"
}
```

## State model

The normalized submission states are:

```text
accepted -> queued -> running -> completed
                            |-> failed
                            |-> cancelled
                            |-> expired
                            |-> outcome_unknown
```

The `succeeded` label is not used because successful result retrieval and a
successful upstream domain outcome are different facts. `completed`, `failed`,
`cancelled`, `expired`, and `outcome_unknown` are terminal. State must never
move from a terminal value back to a pending value. Repeated terminal reads
must return the same normalized result unless retention has expired, after
which they return the terminal `submission_expired` error.

`completed` means Tama Link captured an upstream MCP `CallToolResult`. The
captured result preserves `isError`, content blocks, structured content, and
safe `_meta`; a completed operation may therefore contain `is_error: true`.
Normalized content blocks and safe `_meta` are retained as validated raw JSON,
not projected through a fixed Go union, so extension fields and content added
by a compatible MCP revision are not discarded. Endpoint adapters validate the
wire shape before storage.
`failed` is reserved for transport, protocol, authentication, local execution,
or result-capture failure. `outcome_unknown` is reserved for the ambiguous
result of a non-replayable synchronous mutation and must never be silently
retried or reported as success.

If Tama Link observes completion but the normalized result exceeds the
configured storage limit, the local submission becomes `failed` with
`result_too_large`. Safe error details may record that the upstream outcome was
completed and its result was not captured. Terminal results are never truncated.

The implementation must explicitly distinguish:

- downstream request identity;
- local submission identity;
- upstream Tama task or submission identity;
- progress-event sequence; and
- terminal-result delivery.

An upstream `processed` or accepted marker is not automatically equivalent to
graph completion.

## Durable local state

Tama Link must retain the minimum state required to survive a client restart
and safely correlate an accepted submission with its upstream operation.

Permitted persisted values include:

- profile name and configuration digest;
- local submission and idempotency identifiers;
- operation name, execution strategy, descriptor digest, and validated
  canonical upstream arguments required for recovery;
- upstream opaque correlation identifier;
- normalized state and timestamps;
- progress cursor and bounded recent events;
- terminal result or structured failure within retention limits;
- the accepted response, result, event, and retention limits that govern the
  submission for its complete lifecycle; and
- compatible protocol and adapter versions.

The state store must not contain OAuth authorization codes, access or refresh
tokens, private JWK members, setup credentials, client assertions, database
passwords, or unredacted request headers. Secret material belongs in the
platform credential store or another explicitly selected secure backend.
Canonical arguments, results, progress messages, and other sensitive state
blobs must be encrypted at rest with a profile-scoped key held in the platform
credential store. Non-sensitive indexes, digests, state labels, and timestamps
may remain plaintext.

A new empty profile database may generate its random state-encryption key in
the configured secure credential backend. An existing database whose key is
missing or unavailable must fail closed with `state_unavailable`; Tama Link must
not generate a replacement, overwrite unreadable rows, delete state, or fall
back to plaintext. Logout never removes the state key. Version 1 performs no
automatic state-key rotation, and headless environments are supported only with
an explicitly available secure credential backend.

Writes must be atomic and safe against symlink traversal. Local state and
configuration permissions must be restrictive. Retention and garbage
collection must never remove a non-terminal submission merely because the
downstream client disconnected.

The store must tolerate multiple Tama Link processes opening the same profile.
SQLite uses WAL mode, bounded busy handling, transactional idempotency, and
lease-based worker ownership. The database file and its `-wal` and `-shm`
sidecars are opened or created without following links inside the validated
private profile directory before WAL is enabled. On Windows, each child
directory, the database, and both sidecars are opened relative to the already
pinned parent handle; retained handles prevent pathname replacement while
SQLite uses the absolute path. First-open initialization, job claiming,
terminal capture, garbage collection, and OAuth refresh coordination must be
safe across processes, not merely goroutines. An existing database whose
declared schema is missing a required durable table or column fails closed;
startup must not silently recreate the missing object and lose its durable
index or ownership state. For the initial release there is only one schema and
encryption format; mismatched version metadata fails closed rather than
invoking a speculative upgrade path. A migration is introduced only after a
released format creates a real compatibility boundary.

## Operation catalog and instruction projection

Every profile contains a pinned, deterministic snapshot of its approved
upstream operations. Each operation descriptor contains at least:

```text
name and title
description
upstream input schema
client-visible input schema
output schema
annotations and task support
declarative argument bindings
execution strategy
descriptor digest
```

The catalog snapshot allows Tama Link to initialize and advertise useful tools
before interactive OAuth is available. On an authenticated upstream connection,
Tama Link performs initialization and a complete paginated `tools/list`, then
intersects the live catalog with the profile allowlist. A live tool that is not
in the profile is never exposed automatically. A pinned operation whose
security-relevant descriptor has drifted fails closed with
`operation_contract_mismatch` until the profile is reconciled.

The downstream `submit` schema always constrains `tool` to the approved names.
For legacy clients, `arguments` remains an object and the generated `submit`
description includes bounded deterministic operation signatures. Tama Link
performs the authoritative per-operation validation at execution time. A later
downstream protocol adapter may use a tagged `oneOf` schema when the negotiated
client supports full JSON Schema composition; correctness must not depend on
that richer presentation.

Server instructions are composed from two clearly separated sources:

1. Tama Link supplies the submit-once, await-until-terminal workflow and local
   safety constraints.
2. The trusted profile supplies a pinned bounded copy of the selected upstream
   server instructions, verified against live initialization when connected.

Product skills own domain judgment such as when and what to remember or how to
conduct Reflection review. Skills must not be the sole source of operation
schemas, idempotency rules, or polling correctness.

## Upstream adapters and protocol evolution

The core state machine must not depend directly on one MCP revision. An
upstream adapter translates between the normalized Tama Link operations and
the protocol spoken by Tama.

Each operation selects exactly one execution strategy:

| Strategy | Initial use | Recovery |
| --- | --- | --- |
| `upstream_task` | `/mcp/app` `message` | Reconnect, reissue the canonical idempotent request, and attach a fresh session-scoped task ID |
| `local_replayable` | Read-only or proven-idempotent `/mcp/system` tools | Execute as an ordinary upstream call from a leased local worker; replay safely after an interrupted lease |
| `local_guarded` | Synchronous mutation with a reviewed conflict/reconciliation contract | Reconcile before retry; otherwise terminate as `outcome_unknown` |
| `unsupported` | Unsafe synchronous mutation | Reject before upstream execution |

The initial `/mcp/system` profile uses `local_replayable` for inspection tools.
It marks `reflection.comments.review` as `unsupported`: the current
`expected_state_version` guard prevents a stale duplicate transition, but the
read-back surface cannot prove which actor performed a transition after a lost
response. A later profile may enable it only after upstream idempotency or
correlation is returned and retrievable during reconciliation. The existing
Tama App submission table remains authoritative for graph execution and is not
generalized for System calls.

Initial adapter work must cover the protocol and durable-result contract in the
currently supported Tama release. Later adapters may cover standard MCP Tasks
and the newer MCP protocol without changing the downstream `submit`/`await`
tool contract.

For current Tama `0.14.0`, the task adapter uses `tasks/get` for status and
polling guidance and `tasks/result` to retrieve the terminal MCP
`CallToolResult`. It must also understand `tasks/cancel`, while downstream wait
cancellation remains local and must not cancel Tama's durable Submission or
graph execution. A terminal `tasks/get` response is not a substitute for
`tasks/result`.

At connection time Tama Link records and validates:

- negotiated protocol version;
- server identity and declared capabilities;
- selected task/result adapter;
- protected-resource and authorization-server metadata; and
- profile compatibility bounds.

Unsupported combinations fail closed with `protocol_mismatch`. Tama Link must
not guess task support from a product name or user agent.

The downstream STDIO server must use a reviewed stable release of the official
MCP Go SDK. Because the released SDK used by the initial implementation does
not expose the current Tama task request/lookup surface or a raw request API,
the current upstream adapter may use a minimal reviewed and fixture-tested
JSON-RPC/Streamable HTTP implementation. Pre-release support for a newer
protocol must not enter the default build until the application and client
compatibility matrix is proven.

## Progress contract

Progress is first-class normalized data, not terminal output text parsed by
plugins.

Each progress event contains:

```json
{
  "submission_id": "sub_opaque",
  "sequence": 7,
  "timestamp": "RFC3339 timestamp",
  "state": "running",
  "step": {
    "current": 2,
    "total": 4,
    "label": "Indexing memories"
  },
  "message": "Processing 18 posts"
}
```

Sequences are strictly increasing within one submission. Consumers deduplicate
by submission and sequence. Unknown totals are omitted, not represented as
zero.

Progress reaches clients through two layers:

1. Every `await` response contains the current snapshot and events after the
   supplied cursor. This is the required portable behavior.
2. If a downstream MCP request contains `_meta.progressToken`, Tama Link may
   send rate-limited `notifications/progress` associated with that request.
   Absence of a token means the client did not request protocol progress.

A progress token indicates protocol interest, not guaranteed rich UI. Client
packages may map normalized events to native host features:

- OpenCode may use tool progress callbacks or a reactive panel;
- Codex may show separate `submit`/`await` calls, agent commentary, and static
  hook status messages; and
- clients without progress UI still receive deterministic polling responses.

Client presentation must never be required for correctness.
Version 1 defines no additional local event interface; `await` snapshots and
standard MCP progress notifications are the complete portable event surface
until client acceptance demonstrates a concrete missing capability.

## Authentication and profiles

A profile selects trusted upstream configuration. Tool arguments must never
select or override the upstream origin.

The planned non-secret profile contains:

```text
profile version
canonical profile digest
Tama protected-resource origin
MCP endpoint
expected authorization-server issuer
allowed upstream operations
client-visible and upstream operation schemas
declarative argument bindings and execution strategies
pinned upstream server instructions and descriptor digests
compatibility bounds
profile-isolated state and credential-store references
timeouts, size limits, and retention policy
```

Database references are portable lowercase filenames: one to 64 lowercase
letters, digits, or hyphens, beginning and ending with a letter or digit. They
must not use Windows reserved device names. Credential-store references remain
opaque names but may not contain path separators or traversal components.

The Memovee CLI owns creation and reconciliation of its profile. Tama Link
validates profiles but does not start containers or create root users.

The profile digest is `sha256:` followed by the lowercase SHA-256 digest of the
canonical JSON encoding of the complete non-secret profile, excluding the
`digest` field itself. Canonicalization ignores insignificant whitespace and
object-key order while preserving JSON number literals. If a digest is present,
Tama Link always verifies it. A profile that raises any version 1 default limit
must include a matching digest; omission or mismatch fails closed until the
profile owner reconciles and rewrites the profile.

OAuth behavior must follow protected-resource metadata and authorization-server
discovery. Browser authorization and consent remain user-visible. Tokens must
be stored only through the configured secure credential backend, redacted from
errors, and excluded from logs and local submission state.

`tama-link login --profile <name>` is the explicit interactive entry point for
browser authorization. An MCP tool call that lacks usable credentials returns
`authentication_required`; it must not unexpectedly open a browser from the
STDIO server. Logout and revocation behavior must be explicit and must not
delete durable non-terminal submissions.

Authentication failure must not trigger an unbounded retry loop. An `await`
operation may refresh authorization when standards and policy permit, but must
return an actionable terminal or retryable error when user interaction is
required.

The current Tama `0.14.0-server` profile uses a stable refresh token across
refresh exchanges. Tama Link coordinates refresh through a profile-scoped
cross-process lease, re-reads the credential after acquiring it, and safely
stores a replacement if a future compatible server returns one. `invalid_grant`
maps to `authentication_required` without an automatic retry loop.

## Validation and limits

The implementation must define and test bounds for:

- tool argument size and nesting;
- upstream response and terminal result size;
- progress event rate and retained event count;
- concurrent submissions and waits;
- request, connection, and long-poll timeouts;
- retry count and backoff;
- redirect count and destination validation; and
- terminal result retention.

The version 1 defaults are 1 MiB and 32 levels for canonical arguments, 16 MiB
for one upstream response body, 8 MiB for one stored normalized terminal
result, 16 KiB per progress event, 128 retained events and 1 MiB total event
data per submission, seven days for terminal payloads, and 30 days from
completion for payload-free expiry tombstones. Profiles may lower these values;
raising them requires explicit values within implementation hard ceilings and
a reconciled profile digest.

All network destinations come from a validated profile. Redirects, discovered
metadata, JWKS locations, and authorization endpoints require the same SSRF and
origin review expected of an OAuth/MCP client.

Standard output must never contain logs. Logs must not contain credentials,
authorization headers, private keys, raw assertions, complete sensitive
arguments, or unbounded upstream response bodies.

## Stable error taxonomy

At minimum, client-facing errors use these codes:

```text
invalid_request
operation_not_allowed
operation_contract_mismatch
idempotency_conflict
submission_not_found
submission_expired
authentication_required
authorization_failed
protocol_mismatch
upstream_unavailable
upstream_execution_failed
outcome_unknown
result_too_large
state_unavailable
internal
not_implemented
```

Errors contain a safe message and `retryable` boolean. Diagnostic detail may
include bounded reason enums and correlation IDs, never secrets. A pending
long-poll timeout is not an error.

## Command surface

The planned administrative command surface is:

```text
tama-link serve --profile <name>
tama-link login --profile <name>
tama-link logout --profile <name>
tama-link doctor --profile <name> [--json]
tama-link version [--json]
```

Only `serve` is needed for normal MCP operation. `login` performs explicit
interactive authorization. `logout` removes or revokes only the selected
profile's credentials after checking policy; it does not delete profile state.
`doctor` is read-only. Memovee-owned profile setup is normally performed by the
Memovee CLI.

Human output may use progress and color when attached to a terminal. JSON and
non-interactive output must be deterministic, ANSI-free, and free of secret
values.

## Compatibility and release policy

Tama Link has an independent Git Flow release lifecycle. A release builds one
binary per supported operating-system and architecture pair. Memovee CLI pins
a tested Tama Link version and checksum; it does not rebuild Tama Link.

Each release publishes a compatibility matrix covering:

- Tama versions and upstream adapters;
- MCP protocol versions;
- profile contract versions;
- supported operating systems and architectures; and
- verified client packages and versions.

Initial targets are Linux amd64 and arm64. macOS amd64/arm64 and Windows amd64
may be added when their credential storage, process behavior, and client
acceptance tests exist.

## Acceptance criteria

The first complete implementation is not done until automated tests prove:

1. MCP initialization succeeds over STDIO.
2. `tools/list` exposes exactly `submit` and `await` with the documented schemas.
3. `submit` returns promptly after one durable local acceptance.
4. Repeating the same idempotency key does not duplicate upstream work.
5. Conflicting idempotency input fails deterministically.
6. `await` returns pending state when its bounded wait expires.
7. Repeated `await` calls reach and preserve a successful terminal result.
8. Upstream failure, cancellation, and expiry produce terminal failures.
9. Client cancellation stops a wait without cancelling upstream work.
10. A process restart recovers accepted non-terminal submissions.
11. Progress cursors deduplicate ordered events.
12. Requested MCP progress notifications are rate limited and correlated.
13. Credentials and plaintext sensitive inputs do not appear in SQLite
    metadata, JSON output, logs, panic output, or test snapshots; encrypted
    state blobs fail closed when their key is unavailable.
14. Unsupported protocol, capability, profile, and Tama versions fail closed.
15. Separate App and System registrations expose isolated catalogs,
    instructions, credentials, and state while retaining the same two-tool
    contract.
16. App restart recovery reattaches to Tama's durable submission without
    duplicate graph work.
17. System read-only restart recovery safely replays unfinished local work.
18. Multiple processes sharing one profile cannot duplicate claimed work, lose
    a replacement refresh token, or corrupt credential coordination.
19. Codex, OpenCode, and at least one plain MCP inspector complete the
    `submit`/repeated-`await` workflow for both profile types.
20. Race tests, static analysis, lint, cross-builds, and protocol fixtures pass.

## Implementation phases

### Phase 0: repository foundation

- Go module, STDIO MCP server, and exact two-tool catalog;
- placeholder handlers that fail explicitly with `not_implemented`;
- unit test for the public tool surface;
- formatting, unit, race, vet, lint, and build automation; and
- documentation, Git Flow, and CI.

### Phase 1: normalized domain and state

- submission state machine and error types;
- profile loading and validation;
- atomic durable local store;
- catalog, schema, binding, and instruction projection;
- replayable and guarded local execution strategies;
- idempotency and recovery tests; and
- progress snapshot/event model.

### Phase 2: current Tama adapter

- authenticated upstream connection;
- current durable submission/result correlation;
- `tasks/get` status polling and `tasks/result` terminal capture;
- ordinary `/mcp/system` execution through the local worker;
- bounded polling and terminal failure semantics;
- result normalization and limits; and
- integration fixtures against the supported Tama release.

### Phase 3: client progress and acceptance

- MCP progress-token support;
- Codex and OpenCode package integration;
- client-specific progress presentation; and
- disconnect, restart, timeout, and live OAuth acceptance tests.

### Phase 4: newer MCP adapter

- adopt a stable SDK release supporting the newer protocol;
- add negotiated task-capability handling behind the adapter boundary;
- retain the downstream two-tool contract; and
- expand the published compatibility matrix only after live client tests.

## Remaining acceptance gates

The current adapter must still be verified against the pinned local
`memovee/tama` Compose environment: initialization and protocol negotiation,
the task lifetime and terminal-result behavior, canonical-request reattachment,
and production ingress availability. Codex, OpenCode, and plain MCP fixtures
must prove stable caller-owned `client_context.thread_id` behavior. The SQLite,
lease, GC, encryption-key, and crash-recovery suite must use separate OS
processes and cover each supported credential backend, including the explicitly
configured headless Linux case.

The newer Phase 4 adapter still requires a client-facing, request-correlated
answer path for `input_required`. The preferred direction is an optional
`input_response` on `await`, which retains exactly two downstream tools; its
schema and idempotency rules must be finalized against `tama-mcp` fixtures
before that adapter is enabled.
