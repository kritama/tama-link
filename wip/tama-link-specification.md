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
compatibility boundary between a client-facing synchronous tool surface and
Tama's durable upstream execution. Client integrations stay deliberately thin.

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
12. support deterministic diagnosis and compatibility testing.

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
| Tama | Authorization enforcement, graph execution, durable upstream task state, progress source, terminal result |
| Tama Link | Protocol adaptation, OAuth client behavior, local correlation, idempotency, polling, progress normalization, client-facing tool contract |
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

The initial implementation must not open a TCP listener. A future loopback
HTTP, Unix-domain socket, or named-pipe control surface requires a separate
threat model and authentication design.

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
  "client_request_id": "string, optional"
}
```

Requirements:

- `tool` identifies an operation allowed by the selected profile. It must not
  supply an endpoint, origin, executable, or arbitrary transport destination.
- `arguments` is passed only after validation against the selected upstream
  operation contract.
- `client_request_id` is an opaque idempotency key scoped to the profile. When
  omitted, Tama Link generates one before the first upstream mutation.
- Repeating the same `client_request_id` with equivalent canonical input must
  return the original submission.
- Reusing it with different input must fail with `idempotency_conflict`.
- Success means the operation was durably accepted, not that it completed.
- The response must not block until graph completion.

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

Successful terminal output:

```json
{
  "submission_id": "sub_opaque",
  "status": "succeeded",
  "terminal": true,
  "cursor": "event_cursor",
  "result": {},
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
accepted -> queued -> running -> succeeded
                            |-> failed
                            |-> cancelled
                            |-> expired
```

`succeeded`, `failed`, `cancelled`, and `expired` are terminal. State must never
move from a terminal value back to a pending value. Repeated terminal reads
must return the same normalized result unless retention has expired, after
which they return the terminal `submission_expired` error.

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
- upstream opaque correlation identifier;
- normalized state and timestamps;
- progress cursor and bounded recent events;
- terminal result or structured failure within retention limits; and
- compatible protocol and adapter versions.

The state store must not contain OAuth authorization codes, access or refresh
tokens, private JWK members, setup credentials, client assertions, database
passwords, or unredacted request headers. Secret material belongs in the
platform credential store or another explicitly selected secure backend.

Writes must be atomic and safe against symlink traversal. Local state and
configuration permissions must be restrictive. Retention and garbage
collection must never remove a non-terminal submission merely because the
downstream client disconnected.

## Upstream adapters and protocol evolution

The core state machine must not depend directly on one MCP revision. An
upstream adapter translates between the normalized Tama Link operations and
the protocol spoken by Tama.

Initial adapter work must cover the protocol and durable-result contract in the
currently supported Tama release. Later adapters may cover standard MCP Tasks
and the newer MCP protocol without changing the downstream `submit`/`await`
tool contract.

At connection time Tama Link records and validates:

- negotiated protocol version;
- server identity and declared capabilities;
- selected task/result adapter;
- protected-resource and authorization-server metadata; and
- profile compatibility bounds.

Unsupported combinations fail closed with `protocol_mismatch`. Tama Link must
not guess task support from a product name or user agent.

The initial Go dependency must use a reviewed stable release of the official
MCP Go SDK. Pre-release support for a newer protocol must not enter the default
build until the application and client compatibility matrix is proven.

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

## Authentication and profiles

A profile selects trusted upstream configuration. Tool arguments must never
select or override the upstream origin.

The planned non-secret profile contains:

```text
profile version
Tama protected-resource origin
MCP endpoint
expected authorization-server issuer
allowed upstream operations
compatibility bounds
state and credential-store references
timeouts, size limits, and retention policy
```

The Memovee CLI owns creation and reconciliation of its profile. Tama Link
validates profiles but does not start containers or create root users.

OAuth behavior must follow protected-resource metadata and authorization-server
discovery. Browser authorization and consent remain user-visible. Tokens must
be stored only through the configured secure credential backend, redacted from
errors, and excluded from logs and local submission state.

Authentication failure must not trigger an unbounded retry loop. An `await`
operation may refresh authorization when standards and policy permit, but must
return an actionable terminal or retryable error when user interaction is
required.

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
idempotency_conflict
submission_not_found
submission_expired
authentication_required
authorization_failed
protocol_mismatch
upstream_unavailable
upstream_execution_failed
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
tama-link doctor --profile <name> [--json]
tama-link version [--json]
```

Only `serve` is needed for MCP clients. `doctor` is read-only. Mutating profile
or credential commands require a later explicit design; Memovee-owned setup is
normally performed by the Memovee CLI.

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
3. `submit` returns promptly after one durable upstream acceptance.
4. Repeating the same idempotency key does not duplicate upstream work.
5. Conflicting idempotency input fails deterministically.
6. `await` returns pending state when its bounded wait expires.
7. Repeated `await` calls reach and preserve a successful terminal result.
8. Upstream failure, cancellation, and expiry produce terminal failures.
9. Client cancellation stops a wait without cancelling upstream work.
10. A process restart recovers accepted non-terminal submissions.
11. Progress cursors deduplicate ordered events.
12. Requested MCP progress notifications are rate limited and correlated.
13. Credentials and sensitive inputs do not appear in state, JSON output, logs,
    panic output, or test snapshots.
14. Unsupported protocol, capability, profile, and Tama versions fail closed.
15. Codex, OpenCode, and at least one plain MCP inspector complete the
    `submit`/repeated-`await` workflow.
16. Race tests, static analysis, lint, cross-builds, and protocol fixtures pass.

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
- idempotency and recovery tests; and
- progress snapshot/event model.

### Phase 2: current Tama adapter

- authenticated upstream connection;
- current durable submission/result correlation;
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

## Open implementation questions

The implementation agent must resolve these with repository-backed tests before
production activation:

- the exact current Tama durable submission and terminal-result API;
- secure credential backends for each supported platform;
- the local state engine and migration strategy;
- terminal result retention and size defaults;
- how allowed upstream operation schemas are projected into the `submit`
  description and product skills; and
- which richer local event interface, if any, OpenCode and later plugins need
  beyond MCP progress notifications and `await` snapshots.

These choices may refine internals but must not weaken the two-tool contract,
ownership boundaries, secret custody, or durable terminal semantics above.
