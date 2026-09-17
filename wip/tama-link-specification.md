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
9. speak MCP `2026-07-28` on the upstream side while negotiating the supported
   client protocol on the downstream side;
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
  omitted, Tama Link generates one before durable acceptance, bindings, and
  the first upstream mutation; a generated key is fresh on every call, so each
  omission is a new submission. Operations whose reviewed bindings map the
  key to an upstream identity (for example the App `message` projection)
  document that a generated identifier is a fresh identity per call.
- `client_context` carries client-owned correlation values that an operation
  binding may require but ordinary MCP does not provide automatically. It is
  not passed upstream unless a reviewed profile binding maps a field.
- If the selected operation has a required binding from
  `client_context.thread_id`, omission is `invalid_request`. Tama Link must not
  substitute a profile-global or process-global conversation identity.
- Repeating the same `client_request_id` with equivalent canonical input must
  return the original submission. The request identity is the client-visible
  request — the tool name, the canonical arguments, and the client-owned
  correlation values the accepted operation can map — never live profile
  state or bound upstream output, so a retry after a profile reconciliation
  reconciles to the original submission instead of reporting a conflict. A
  context that carries no correlation value is normalized to absence, so a
  retry that omits the context or sends an empty one carries no value in
  any candidate identity. The accepted identity records how the operation
  handled the client thread ID at acceptance: its value, an explicit
  absence when the request carried no value, or no field at all when the
  operation could not map the source. A retry cannot know which shape it
  took — the binding set may have changed since — so recovery matches, for
  a retry that carries a value, the identity with that value or the
  unmappable-source identity, and, for a retry that carries no value, the
  explicitly-absent or the unmappable-source identity. Adding a thread ID
  after an accepted request omitted one from a mappable source reports a
  conflict; changing a value the accepted operation never mapped
  reconciles; and an exact retry reconciles whether the tool kept, gained,
  or lost its thread binding, or left the catalog entirely.
  The same candidate matching applies when the key is claimed concurrently
  at insert time: two processes straddling a profile reconciliation can
  both miss the preliminary lookup, and the insert loser reconciles the
  winner's row against the candidate identities instead of reporting a
  conflict for an otherwise exact request.
  Reconciliation
  runs before the catalog membership check, argument validation, and binding
  application, in addition to before the readiness and strategy checks
  below. A replay appends no
  progress events and re-offers nothing to the worker: an already advanced
  row keeps a monotonic event sequence.
- Reconciliation of an existing idempotency record precedes the catalog,
  readiness, and strategy checks: recovery of an already accepted request
  must not depend on the current catalog, credentials, or a later profile
  policy change, so a retry after reconciliation removed its tool still
  returns the original submission. The client-facing submit surface must
  not restrict the tool name to the current catalog for this: an exact
  retry whose tool the profile later removed still reaches the
  application, and the application enforces the catalog only for
  genuinely new work. The authenticate-first
  verification and the strategy gate below apply only to genuinely new
  acceptance.
- Lowering a profile limit must not make an already accepted idempotent request
  unrecoverable. Tama Link canonicalizes and reconciles an existing request
  within implementation hard ceilings before applying current profile limits;
  those current limits govern only genuinely new acceptance.
- Reusing it with different input must fail with `idempotency_conflict`.
- Success means Tama Link durably accepted responsibility for the operation in
  its local store, not that Tama accepted or completed it.
- Tama Link verifies that the profile can authenticate before durable
  acceptance, without a refresh or an upstream connection. A usable
  credential is the complete pair a refresh would find: a registered client
  and a refresh credential, both bound to the active profile issuer with an
  issuer-bound token endpoint. A cached in-memory token never counts by
  itself: another process may have logged the profile out, and accepting on
  a cached token alone would only burn idempotency keys on work the worker
  cannot authenticate. A profile without that complete pair fails
  `submit` as `authentication_required`, and the idempotency key stays free,
  so the reauthorize-and-retry flow replays as a genuinely new acceptance
  instead of returning a permanently failed submission.
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
  "timeout_ms": "integer, optional",
  "input_responses": "object keyed by outstanding input-request ID, optional"
}
```

Requirements:

- `timeout_ms` is bounded by configuration and is compared in milliseconds
  before any duration conversion, so values that would overflow a 64-bit
  duration fail as `invalid_request` instead of wrapping. The initial
  recommended default is
  20 seconds and the maximum is 30 seconds.
- Returning because the wait budget elapsed is a successful pending response,
  not a tool error.
- `cursor` is opaque and allows the caller to request only progress events
  after the last observed sequence.
- A client may call `await` repeatedly until `terminal` is true.
- When the current state is `input_required`, `input_responses` may answer one
  or more outstanding request IDs. Partial response maps are allowed. An exact
  replay is idempotent; a different response for an already answered ID is
  `idempotency_conflict`, and a response for an ID that is not outstanding is
  `invalid_request`.
- One `await` call sends at most one upstream `tasks/update` before entering its
  bounded wait. The acknowledgement is eventually consistent and must not be
  interpreted as proof that the task already left `input_required`.
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

This is the rich-adapter shape. The TamaMCP task surface provides complete task
snapshots and bounded `statusMessage` values through `tasks/get` and
`notifications/tasks`, but no standard numeric task-progress channel. The
adapter emits state-transition events, may use `statusMessage` as its bounded
message, and omits unknown `current`, `total`, and `label` values. System
operations may likewise expose only queued/running state unless the upstream
tool provides reviewed structured progress.

An input-required pending response contains the validated outstanding request
map while retaining the same local submission and cursor:

```json
{
  "submission_id": "sub_opaque",
  "status": "input_required",
  "terminal": false,
  "cursor": "event_cursor",
  "input_requests": {
    "approval": {
      "mode": "elicitation",
      "schema": {}
    }
  },
  "next_poll_ms": 1000
}
```

The exact values inside each request and response remain validated protocol
JSON. Tama Link declares upstream input sub-capabilities only when this
downstream contract and the selected profile can relay them safely.

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
accepted -> queued -> running <-> input_required
                       |-> completed
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

`input_required` is non-terminal and may return to `running` after an accepted
response or move directly to any terminal state allowed by the upstream task
contract. `completed` means Tama Link captured an upstream MCP
`CallToolResult`, where `isError` is a required field: a complete result that
omits it or sets it to null is a protocol failure, never a manufactured
success. The captured result preserves `isError`, content blocks,
structured content, and safe `_meta`; a completed operation may therefore
contain `is_error: true`.
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
- upstream opaque owner-bound task identifier when the operation is task-backed;
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
an explicitly available secure credential backend. Backend availability is a
bounded startup probe: a complete set/read/remove cycle on one disposable
entry with a unique unguessable per-invocation key must finish within a short
fixed window, or Tama Link fails fast with a
clear unavailable error. The window is fixed in the binary and cannot be
extended at runtime. The probe runs through one owned worker goroutine
rather than an abandoned one per attempt: the keyring API takes no context,
so an in-flight call cannot be interrupted, but a timed-out probe abandons
only its request — the worker stays owned, serves the next probe once the
blocking call returns, and exits on shutdown. A backend that accepts a
connection but blocks on user
interaction for writes (for example a headless Secret Service) is
unavailable; an interactive unlock prompt is not a supported serve-startup
path, and Tama Link must never hang the serve process on the platform
credential store.

Writes must be atomic and safe against symlink traversal. Local state and
configuration permissions must be restrictive. Retention and garbage
collection must never remove a non-terminal submission merely because the
downstream client disconnected.

Successful `submit` means Tama Link durably accepted responsibility for
executing the operation, and that acceptance must always reach execution or a
terminal state without a process restart. The execution pool bounds live
executions and waiting goroutines alike: at most the configured maximum
executes concurrently and at most that many more wait for a slot in a
bounded parking set; a saturated pool never spawns one waiting goroutine per
offered ID, and the excess stays durable in the store until the sweep
redelivers it. The in-memory worker queue is a prompt-start aid only: when
it is saturated, a recurring durable sweep re-derives every runnable
replayable submission from the store and re-offers it, so a dropped queue
entry is rediscovered within one sweep interval. The
sweep must not re-offer work this process is already executing: in-flight
entries would otherwise crowd the prompt queue ahead of dropped IDs and
starve them. The lease remains the final single-winner guard across
processes. The sweep also runs on explicit request, so a caller that
observed queue saturation can shorten the recovery wait without changing
the durable guarantee. A worker that
has already published a terminal state releases its lease on a best-effort
basis: the release is retried against transient SQLite contention and, if it
still fails, is dropped rather than undoing the terminal transition, because
a lease that outlives a terminal submission only lingers until its TTL
expiry and can never shadow terminal work. A lease claim or transition that
times out against the store's busy timeout is a transient condition, not a
submission failure: startup recovery defers it and the recurring sweep
retries it. Refresh-lease contention is likewise transient, never a failure
of the work: an execution whose token provider could not claim the refresh
lease keeps its non-terminal state and is redelivered by the sweep, instead
of recording a terminal authentication failure for a credential that another
process is merely refreshing.

The store must tolerate multiple Tama Link processes opening the same profile.
SQLite uses WAL mode, bounded busy handling, transactional idempotency, and
lease-based worker ownership. Every multi-statement write transaction begins
with `BEGIN IMMEDIATE`: the pinned driver honors the busy timeout only when a
write lock is acquired at BEGIN time, while a write statement that upgrades a
deferred transaction after a read fails immediately with a lock error even
though the other process would release the lock well before the busy timeout.
A concurrent process's open-validation window is an ordinary write-lock holder;
overlapping writers must wait for the busy timeout and then proceed, never
corrupt or lose the submission. The database file and its `-wal` and `-shm`
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
annotations and expected task/execution strategy
declarative argument bindings
execution strategy
descriptor digest
```

The catalog snapshot allows Tama Link to advertise useful tools before
interactive OAuth is available. On an authenticated upstream connection, Tama
Link performs `server/discover` and reads the complete `tools/list`, then
intersects the live catalog with the profile allowlist. A live tool that is not
in the profile is never exposed automatically. A pinned operation whose
security-relevant descriptor has drifted fails closed with
`operation_contract_mismatch` until the profile is reconciled.

TamaMCP's `2026-07-28` tool listing does not expose the legacy
`execution.taskSupport` field. A profile still pins Link's expected execution
strategy. For task-backed operations, Link verifies the Tasks extension in
`server/discover`, declares it in the current request, and requires the expected
`tools/call` `resultType`. Absence of legacy task metadata must not be
interpreted as evidence that a TamaMCP tool is synchronous. A task result for a
pinned synchronous operation is a contract violation: the submission fails
terminal with `operation_contract_mismatch` and the task result is never
polled, stored, or returned.

The downstream `submit` schema does not constrain `tool` to the current
approved names: an exact retry whose tool the profile later removed must
still reach idempotency reconciliation, and the application enforces the
catalog for genuinely new work. For legacy clients, `arguments` remains an
object and the generated `submit` description includes bounded deterministic
operation signatures, which are the advertisement of the approved
operations. Tama Link performs the authoritative per-operation validation at
execution time. A later
downstream protocol adapter may use a tagged `oneOf` schema when the negotiated
client supports full JSON Schema composition; correctness must not depend on
that richer presentation.

Runtime validation is authoritative only within a reviewed assertion
vocabulary: `type`, `properties`, `required`, `additionalProperties`
(boolean or nested schema), `items`, `enum`, `const`, `minimum`, `maximum`,
`minLength`, `maxLength`, `minItems`, `maxItems`, and `pattern`, plus the
annotation keywords `title`, `description`, `examples`, `default`, `$schema`,
and `$comment`. A pinned operation schema that uses any other assertion
keyword — `oneOf`, `allOf`, `not`, `minProperties`, `uniqueItems`,
`contains`, `exclusiveMinimum`, `dependentRequired`, or another — would be
silently unenforced, so profile load fails closed and names the unsupported
keyword, recursively, instead.

Profile load also validates the meta-shape of every supported keyword,
recursively: a keyword with a null or malformed value is rejected, never
interpreted as absent, so a malformed schema cannot disable an intended
restriction. Count constraints are non-negative integers within a reviewed
bound, `enum` is non-empty, `required` names are unique and, when
`properties` is present, declared, bound pairs must not be inverted, and
type names come from the closed set. The `pattern` dialect is Go's RE2
syntax, a strict subset of the ECMAScript-compatible regular expressions
JSON Schema normally assumes, and patterns must compile at profile load.
At runtime, `minLength` and `maxLength` count Unicode code points, and
numeric values and bounds accept the complete JSON number grammar,
including exponent form. Numeric work — bound comparison, integer checks,
and `const`/`enum` equality — runs on exact arbitrary-precision decimals
(`github.com/cockroachdb/apd/v3`), never through `float64`, and never
allocates in proportion to the exponent magnitude. The reviewed exponent
range is the decimal library's effective-exponent limit of ±100000:
an out-of-range exponent fails profile load for numeric bounds and count
constraints, and fails instance validation whenever a numeric assertion
(bound, `const`, `enum`, or `integer` type) must evaluate it, in both
cases as a validation error rather than a panic. `const` and `enum` use
JSON Schema instance equality recursively: numbers compare by exact
mathematical value (`1`, `1.0`, and `1e0` are equal), strings by decoded
code points, arrays positionally, and objects independently of key order.
Count constraints use the same checked decimal conversion during profile
validation and runtime schema decoding, so an exponent-form integer such as
`1e2` is enforced as 100 in both paths. Integer checks reduce trailing
coefficient zeros and inspect the resulting exponent; they never construct
`10^scale` or perform work proportional to an exponent's magnitude.

`required` follows the standard JSON Schema semantics: it applies only to
object instances (a non-object value is constrained only by an independent
type assertion), it checks map-key presence only (a present property whose
value is an explicit JSON null satisfies it, and the property's own schema
decides whether null is permitted), and no `default` annotation fills a
missing required property.

Server instructions are composed from two clearly separated sources:

1. Tama Link supplies the submit-once, await-until-terminal workflow and local
   safety constraints.
2. The trusted profile supplies a pinned bounded copy of the selected upstream
   server instructions, verified against live discovery when connected.

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
| `upstream_task` | `/mcp/app` `message` | Reauthenticate, retrieve the same owner-bound task ID through `tasks/get`, and resubscribe; replay the canonical request only after ambiguous initial acceptance with no task ID |
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

The sole upstream adapter speaks MCP `2026-07-28` as implemented by TamaMCP. It
does not send `initialize`, `notifications/initialized`, `Mcp-Session-Id`,
client-requested task augmentation, `tasks/result`, or `tasks/list`.

The Phase 2 wire baseline is the TamaMCP specification at commit
`6b5db00018d2774834db5a0f00eed5b9b55e1d2e`, including its immutable core and
Tasks conformance pins. A different TamaMCP revision is supported only after
its compatibility bounds and fixtures are reviewed and the profile contract
is regenerated.

Every request is independently authenticated and carries
`MCP-Protocol-Version`, `Mcp-Method`, conditional `Mcp-Name`, and matching
per-request `_meta` protocol version, Link client information, and declared
capabilities. Unsupported, absent, or body-mismatched standard headers fail
closed. Link declares the Tasks extension on each applicable request; it does
not infer capabilities from discovery or an earlier call.

Task creation is server-directed. A task-backed `tools/call` returns an opaque,
globally unique, owner-bound task ID. `tasks/get` returns the complete detailed
task state and includes the terminal `CallToolResult` or failure payload.
`tasks/update` carries responses while a task is `input_required`, and
`tasks/cancel` records cooperative cancellation intent. Downstream `await`
cancellation remains local and does not invoke `tasks/cancel` or cancel Tama's
durable Submission or graph execution.

`subscriptions/listen` is an authorized, task-ID-scoped SSE optimization. Link
accepts no task notification before the acknowledgement, captures complete
snapshots only when the subscription ID matches and the task ID is in the
acknowledged set, and always recovers through `tasks/get` after a disconnect,
missed notification, overflow, credential expiry, or policy invalidation.
Correctness never depends on notification delivery.

The stream contract fails closed: the acknowledgement must be the first event
and may authorize only a subset of the requested task IDs; every later task
snapshot must carry an acknowledged task ID; the final JSON-RPC response may
only follow the acknowledgement and marks graceful closure; a final JSON-RPC
error is a protocol failure, not a clean close. A successful stop signal — the
matching finite response or the graceful final response — ends the scan
immediately; the client never waits for the peer to close a held-open body.
SSE events are bounded as a complete encoded event, including multi-line data
fields, and an event whose delimiter never arrives at end of stream is not
dispatched. A stream's lifetime is bounded by the caller context, credential
expiry, and the stream-lifetime owner, never by a finite-request client
timeout. Finite requests additionally receive a per-request deadline (default
60 seconds); a supplied HTTP client's overall timeout is cleared when the
transport clones it, so no client-level timeout can terminate a stream.

Detailed task states and initial `tools/call` task results share one common
envelope: `createdAt`, `lastUpdatedAt`, and `ttlMs` are required, correctly
typed, non-negative, and bounded by the Tasks maximum safe integer (2^53-1);
`pollIntervalMs` carries the same bounds when present; explicit nulls fail
like missing values. The pinned TamaMCP profile creates tasks in the `working`
state, so an initial task result in any other state is a protocol failure,
not a shortcut to a captured result.

At discovery time Tama Link records and validates:

- negotiated protocol version;
- server identity and declared capabilities;
- Tasks and task-notification capabilities;
- protected-resource and authorization-server metadata; and
- profile compatibility bounds.

Unsupported combinations fail closed with `protocol_mismatch`. Tama Link must
not guess task support from a product name or user agent.

The authorization-server metadata is validated before any token request:
the token endpoint must be a secure absolute URL on the same origin as the
validated issuer, because the authorization code and any client secret are
sent there. The same origin policy re-validates the stored token endpoint
before every refresh. A refresh holds its cross-process lease for the whole
critical section: the lease is renewed on a third of its TTL while the token
exchange runs and the replacement credential is written, and a lost lease
aborts before any replacement token is persisted: ownership is re-verified
by an atomic commit gate immediately before the write, so a lease lost to a
foreign claim blocks the stale write. The renewal spans the whole critical
section — the token exchange and the fenced persistence — so a slow
secret-store write cannot outlive the lease TTL and reject the replacement
after the endpoint already rotated the grant. The credential persistence
itself is fenced: every writer commits its replacement at its own unique
secure-backend slot and then atomically advances a durable fence pointer to
that slot's generation, and the commit is a single atomic operation that
requires both an unadvanced fence generation and the writer's own live,
unexpired lease ownership epoch — the epoch gate covers the plain insert
path too, so a fence row that is absent because logout cleared it or no
authorization ever committed cannot be created by a stale writer either —
with a provider that permits overlapping
rotations, a writer that lost the lease while its secret-store write was
blocked can never make its value live, not even before the winner commits
its own generation. A writer whose commit fails or is rejected rolls its
own uncommitted slot back, and if that rollback deletion also fails the
slot is durably enqueued in the retirement backlog so a later refresh or
logout retries it — a rejected writer never orphans a refresh token in
the backend. The commit is the last fallible step for the slot
swap: the previous live slot stays referenced until the commit is durable,
so a rejected commit can never leave the fence pointing at a deleted
credential, and the commit atomically enqueues the previous slot in the
retirement backlog — one transaction with the advance — so a committed
fence always carries a durable retirement record for the slot it replaced.
A successful commit leaves the new slot as the only live credential; the
replaced credentials — the legacy label and the previous live slot — are
retired after the commit, the backlog record is cleared on success, and a
failed retirement keeps its durable record so a later refresh or logout
retries the deletion instead of silently stranding a still-valid grant. The
authorization-code exchange is a credential rotation too: it holds the
local refresh lock across the exchange and persistence, so an in-process
refresh or logout waits for the login instead of racing it through the
shared lease owner, and it claims the same refresh lease before redeeming
the single-use code and renews the lease across both the exchange and the
fenced persistence, so a lease contention can never burn the code; it
commits its credential through the same fence under its own epoch. Logout holds the local refresh lock
and claims the cross-process refresh lease before touching credentials, so
no concurrent writer can reinstall a fence and slot behind the logout: the
claim advances the epoch and every in-flight writer's commit fails and
rolls back its own slot. A contended claim fails logout with a retryable
error. Ownership is renewed through the whole cleanup, outliving the
caller's cancellation because the fixed-label deletions take no context
and cannot be aborted: a cancel mid-delete must not hand the epoch to
another process whose new registration the stale deletion would then
remove. The fence clear
is bound to the claimed epoch, so a logout that loses its lease
mid-cleanup fails retryably and can never wipe a newer fence installed by
the process that took over. The committed slot is deleted while the fence
still references it — a failed deletion keeps the slot discoverable, so a
retried logout finishes the cleanup — and the fence is cleared once its
slot is gone; a fence that is already absent clears successfully, so a
repeated logout or a legacy-only profile can complete its cleanup; a later
login starts from a clean fence. The
fence subsumes post-write ownership checks, which
cannot repair an unfenced external write. Refresh transactions are also
serialized inside one process, and a burst of concurrent token requests
reuses one rotation: queued callers recheck the cached token under the
single-flight lock, while an explicit forced refresh never coalesces. The
refresh lead time is capped to a quarter of the issued token lifetime so a
short token keeps a positive validity window instead of refreshing on every
request.

The local worker executes at most a bounded number of submissions
concurrently; queued work beyond the bound waits for a free slot. The bound
protects the store writer and the upstream connection pool when a sweep or
burst discovers a large backlog. Before a replayable submission executes,
the worker rechecks the submission's accepted descriptor digest against the
connection's effective descriptor: a submission accepted under one contract
is not executed under a different descriptor that reuses the tool name, and
the mismatch fails with `operation_contract_mismatch` before any upstream
call. The verified upstream connection is resolved once and reused for the
process lifetime: the connection's token provider tracks refreshes, so a
reused connection always authenticates with the current credential, while
re-resolving per replayable execution would repeat the authenticated
`server/discover` and the complete paginated `tools/list` for every
operation and let a transient discovery outage fail already queued work.

Both the downstream STDIO server and the upstream core transport use a reviewed
stable release of the official MCP Go SDK. If the selected stable release does
not expose the separately versioned Tasks methods or task-ID subscription
shape, Tama Link may add one minimal reviewed extension layer for those exact
wire contracts. It must reuse SDK core transport conventions and pass the
TamaMCP package fixtures; it must not grow into a second general MCP client or
reintroduce legacy session behavior.

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

The authorization-code exchange binds one exact loopback redirect URI. The
ephemeral listener port is selected before the authorization URL is built, and
that exact URI appears in the authorization request, is required on the
observed callback, and is resent verbatim in the token request, as the
authorization-code grant requires. A callback observed on any other URI is
rejected before any token request is sent.

Tama Link coordinates refresh through a profile-scoped cross-process lease,
re-reads the credential after acquiring it, and safely stores a replacement
refresh token when the provider returns one. The reported `expires_in` is
bounded before its duration conversion, so a pathological value can never
wrap negative and commit a credential that is already expired.
`invalid_grant` maps to
`authentication_required` without an automatic retry loop, and the durable
refresh credential is invalidated under the held, renewed lease: the fence
pointer and the fenced slot's retirement record are committed in one
transaction — honoring a lost epoch and treating an absent fence as already
cleared — before the slot is deleted, so a crash or a failed deletion can
never leave the slot with no durable reference; a failed deletion keeps its
record for the next refresh or logout, and the record is removed only after
the deletion succeeds. The durable invalidation marker is established
before the fence is cleared — clearing makes a surviving legacy label
eligible for the fence-less fallback — so a crash or a failed fenced-slot
deletion can no more bring the rejected grant back to life than a failed
legacy deletion; the marker is cleared once the label is gone or a new
credential commits. With
the credential invalidated, readiness rejects new
work as `authentication_required` instead of accepting submissions that
can only fail on the same known-invalid grant. An upstream
subscription closes no later than credential expiry; after successful refresh,
Link reconciles through `tasks/get` before opening a replacement stream.
A successful exchange requires the response to declare the Bearer token
type, case-insensitive: the upstream transport always sends the access
token as a Bearer credential, so an omitted or different type is a failed
exchange, not a credential.

The stored client registration and refresh credential must both bind the
active profile issuer, and the stored token endpoint is re-validated against
the issuer-bound metadata policy before use. A profile that changes issuer
while retaining its credential namespace fails closed rather than replaying a
foreign token to a stored endpoint. A registration response must supply the
client secret the selected auth method requires: a client-secret
registration without a secret fails the registration instead of being
persisted as a permanently unusable record that readiness keeps accepting.
The stored registration is revalidated against the same requirement on
load, so a legacy record persisted without the secret is treated as absent:
readiness fails as not-ready and the next login's registration replaces the
unusable record instead of the profile looping on it. The RFC 7591
`client_secret_expires_at` is persisted with the registration, and an
expired secret makes the record absent for readiness and refresh, so the
next login re-registers before an exchange can fail on an `invalid_client`
authentication. Because a replacement registration issues a new client ID,
any refresh credential still stored from the previous client is retired and
the new record committed under one refresh-lease epoch: a login that
started from the previous record revalidates the stored registration before
consuming the single-use authorization code and again before committing its
grant, so it either commits before the replacement — its grant is then
retired as orphaned — or aborts, and the new client is never paired with a
grant issued under the old one. The registration mutation also holds the
local lock that orders completions, refreshes, and logouts (a same-owner
claim never advances the epoch, so the lease alone cannot order in-process
mutations) and renews the epoch across the whole mutation. The client
record is fenced exactly like the refresh credential: the record is
written to a unique secure-backend slot and made live only by an atomic
fence advance that requires the writer's own live, unexpired lease
ownership epoch. The slot write itself takes no context and cannot be
aborted, but a writer that loses the lease while the write is in flight —
or whose caller cancels before the commit is observed — has its fence
advance rejected and removes its own uncommitted slot, so it can never
install a stale registration, and a winner whose registration commits in
between keeps its own fenced record untouched. If that rollback deletion
fails, the uncommitted slot is durably added to the client retirement backlog
on a cancellation-independent context so a later refresh or logout retries it
instead of orphaning a client secret. The fence pointer and its
clear are therefore the only authority for which registration is live:
reads take the fenced slot when a client fence has been committed and
fall back to the legacy single-label record only when no fence exists, a
registration replaced by a newer commit is retired through the same
retirement backlog as refresh credentials, and logout clears the client
fence and removes its slot under the same epoch-bound protocol as the
refresh fence, so a logout that loses its lease mid-cleanup fails
retryably and can never wipe a registration installed by the process
that took over. The normal
first-login path, which stores no credential yet, is unaffected.
The client auth method is selected from the set the authorization server
advertises — the field is a set of supported methods, not a single choice:
the first method this client supports, in the server's advertised order,
with the RFC 8414 default when the field is omitted, and the same
selection shared between discovery and registration. A server that
advertises no supported method fails discovery.

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
a reconciled profile digest. Deleting a lapsed tombstone also deletes the
input responses retained for that submission: retained response payloads must
not outlive the tombstone that bounds them. The serve process applies these
deadlines on a periodic retention sweep for its lifetime — with an owned,
cancellable loop that shuts down before the store closes — so a
continuously running or restarted process expires payloads, tombstones,
and their idempotency rows instead of accumulating them; the sweep is one
idempotent transaction, so overlapping sweeps across processes are safe.

All network destinations come from a validated profile. Discovered metadata,
JWKS locations, and authorization endpoints require the same SSRF and origin
review expected of an OAuth/MCP client. HTTP redirects are never followed on
either side: every client, default or supplied, rejects 3xx responses instead
of changing destination, so a bearer token or form credential can never be
replayed to a destination introduced by a `Location` header.

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
7. Repeated `await` calls reach and preserve a successful terminal result
   returned in a detailed `tasks/get` state or task notification.
8. `input_required` requests can be answered idempotently through `await`, and
   an eventually consistent `tasks/update` acknowledgement is reconciled.
9. Upstream failure, cancellation, and expiry produce terminal failures.
10. Client cancellation stops a wait without cancelling upstream work.
11. A process restart recovers accepted non-terminal submissions through the
    same owner-bound task ID without a protocol session. Startup recovery
    lists the durable backlog and offers it to the bounded worker pool, then
    returns immediately: a large backlog executes in the background and must
    not delay the downstream MCP server accepting clients.
12. Progress cursors deduplicate ordered events. A cursor beyond the
    submission's current event sequence is an `invalid_request`, never
    echoed, so a copied cursor cannot create a persistent event gap.
13. Recording an input response detects a concurrent winner in the same
    operation: when another process answered the same outstanding request
    with a different value, the loser gets `idempotency_conflict` and
    reconciles against the durable winner instead of silently assuming its
    value was stored. An exact replay of the recorded value is a no-op.
13. Requested MCP progress notifications are rate limited and correlated.
14. Credentials and plaintext sensitive inputs do not appear in SQLite
    metadata, JSON output, logs, panic output, or test snapshots; encrypted
    state blobs fail closed when their key is unavailable.
15. Unsupported protocol, capability, profile, and Tama versions fail closed;
    legacy upstream initialization, session IDs, `tasks/result`, and
    `tasks/list` are rejected rather than used as fallbacks.
16. Separate App and System registrations expose isolated catalogs,
    instructions, credentials, and state while retaining the same two-tool
    contract.
17. App restart recovery retrieves the same owner-bound durable task; an
    ambiguous initial call replay does not duplicate graph work.
18. System read-only restart recovery safely replays unfinished local work.
19. Subscription acknowledgement, authorized task snapshots, stream loss, and
    credential-expiry recovery preserve correctness through `tasks/get`.
20. Multiple processes sharing one profile cannot duplicate claimed work, lose
    a replacement refresh token, or corrupt credential coordination.
21. Codex, OpenCode, and at least one plain MCP inspector complete the
    `submit`/repeated-`await` workflow for both profile types.
22. Race tests, static analysis, lint, cross-builds, TamaMCP conformance
    fixtures, and live migrated-Tama acceptance pass.

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

### Phase 2: TamaMCP 2026 upstream adapter

- official-SDK MCP `2026-07-28` core transport plus the smallest required
  Tasks/subscription extension layer;
- authenticated stateless `server/discover`, standard headers, and per-request
  metadata/capability negotiation;
- server-directed App task creation, owner-bound `tasks/get`, idempotent
  `tasks/update`, and cooperative `tasks/cancel` support;
- task-ID `subscriptions/listen` and `notifications/tasks`, with polling as the
  recovery source of truth;
- ordinary synchronous `/mcp/system` execution through the local worker;
- bounded polling, terminal failure semantics, result normalization, and
  limits; and
- package conformance fixtures plus live integration against the migrated Tama
  server.

### Phase 3: client progress and acceptance

- MCP progress-token support;
- Codex and OpenCode package integration;
- client-specific progress presentation; and
- disconnect, restart, timeout, and live OAuth acceptance tests.

### Phase 4: production release and migration closure

- certify every supported credential backend and crash-recovery path;
- publish the exact TamaMCP, Tama, Tama Link, protocol, profile, OS, and client
  compatibility matrix;
- coordinate acceptance evidence before Tama removes its Anubis runtime; and
- publish the independent Tama Link binary and checksums through Git Flow.

## Remaining acceptance gates

TamaMCP Phase 2 is complete, while task subscriptions remain tracked by
`kritama/tama-mcp#9`. Live Link acceptance waits for the Tama-owned persistence,
runner, PubSub, System, App, OAuth-composition, and endpoint migration beginning
with `upmaru/tama#123`. The migrated endpoint must be verified for stateless
discovery, standard headers, owner-bound task lookup, input responses,
subscription recovery, terminal capture through `tasks/get`, and production
ingress availability.

Codex, OpenCode, and plain MCP fixtures must prove stable caller-owned
`client_context.thread_id` behavior. The SQLite, lease, GC, encryption-key, and
crash-recovery suite must use separate OS processes and cover each supported
credential backend, including the explicitly configured headless Linux case.
