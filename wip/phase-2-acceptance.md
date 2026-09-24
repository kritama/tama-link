# Phase 2 acceptance gates

Issue #8 has four gates. They are not interchangeable. A green result in an
earlier gate is not evidence for a later one, and Phase 2 is not complete
until the live gate passes.

| Gate | Command | What a pass means |
| --- | --- | --- |
| Package fixtures | `go test ./internal/upstream/conformance` | The upstream client satisfies the pinned TamaMCP core, task, and subscription documents. |
| Mocked integration | `go test ./internal/application ./internal/worker ./internal/adapter/tama2026` | Local restart, ambiguous replay, and System recovery behavior against a scripted endpoint. |
| Compose pins | `make compose-accept` | `docker compose config` resolves `tama`, `tama-mcp`, and `provider`. Every image or build source that Compose may select pins the reported revision. Mutable tags, local contexts, and build arguments are not pins. Containers are not started. |
| Live runtime | `make live-accept` | Unimplemented scaffold. The command fails closed and does not exercise profiles, perform OAuth, or write evidence. A pass is not possible in this revision. |

`make check` runs the fixture gate and the mocked tests. It does not run the
Compose or live commands, and it must not be described as deployment or
runtime acceptance.

## Package fixture pin

Core and Tasks documents are the specification baseline
`6b5db00018d2774834db5a0f00eed5b9b55e1d2e`. Subscription fixtures were not in
that commit. They are pinned to TamaMCP `v0.2.0`
(`5c80c29e90c49438fbcc331db5c00f9f8f93ee21`). The protocol version remains
`2026-07-28`, so profile bounds are not regenerated.

Link adapts the fixtures. It does not copy their wire documents into
`submit` or `await`, and it does not replay a fixture request byte for byte.
Request IDs, client identity, and `Authorization` are Link's. Removed methods,
`Mcp-Session-Id`, and `params.task` are not emitted. Empty or duplicate
subscription task IDs in a fixture request are not copied; the client still
has to classify the fixture's authorization and subset responses.

## Compose pin command

```sh
TAMA_LINK_COMPOSE_FILE=/absolute/path/compose.yml \
TAMA_LINK_COMPOSE_TAMAMCP_SHA=<sha-or-release> \
TAMA_LINK_COMPOSE_TAMA_SHA=<sha-or-release> \
TAMA_LINK_COMPOSE_PROVIDER_SHA=<sha-or-release> \
make compose-accept
```

The command always resolves the Compose model with `docker compose config`
before inspecting it. A raw file read is not resolved configuration, whether
or not the file uses `include`. If resolution fails, the pin check fails
closed. It then requires services named `tama`, `tama-mcp`, and `provider`.
Each reported revision must be that service's image tag, its image digest, or
the git ref of a remote build source. A mutable tag such as `:latest` or
`:stable`, an untagged image, a local build context, a build argument, or a
label is not a pin. A revision that appears only in the environment, or only
inside unrelated build metadata, is not evidence.

When a service declares both `image` and `build`, both possible runtime sources
must pin the same reported revision. The only exception is
`pull_policy: build`, which proves Compose selects the build source; in that
case the remote build ref must carry the revision and the image tag is only the
build output name. This prevents Compose's default pull-first behavior from
substituting a mutable image for a pinned build or falling back from a pinned
image to an unpinned local build.

The command does not run `docker compose up`. A green pin check is not startup
evidence and is not live evidence.

## Live command

`make live-accept` is an unimplemented scaffold. It does not start Tama Link,
call either endpoint, perform OAuth, restart Link or Tama, observe
notifications or polling, or write evidence. It always fails closed. Do not
describe a pass of this command as proof that both profiles were exercised;
a pass is not possible until the migrated topology exists and the scaffold is
replaced by that runner.

```sh
make live-accept
```

The environment variables below are reserved for that future runner. Setting
them does not make the current command exercise a runtime.

```sh
TAMA_LINK_LIVE=1 \
TAMA_LINK_LIVE_APP_ENDPOINT=https://tama.app.localhost/mcp/app \
TAMA_LINK_LIVE_SYSTEM_ENDPOINT=https://tama.app.localhost/mcp/system \
TAMA_LINK_LIVE_TAMAMCP_SHA=<sha-or-release> \
TAMA_LINK_LIVE_TAMA_SHA=<sha-or-release> \
TAMA_LINK_LIVE_PROVIDER_SHA=<sha-or-release> \
make live-accept
```

The scaffold does not write `wip/phase-2-live-evidence.json` and does not
update readiness. `upmaru/tama#123` and the System/App/PubSub migration
remain unrecorded.

## Live evidence

Write `wip/phase-2-live-evidence.json` only after `make live-accept` passes
against the migrated runtime. The file is absent until then. A present file
must validate as live evidence, not as a fixture summary. Required fields:

- `gate`: `live`
- `tamamcp_sha`, `tama_sha`, `provider_sha`: exact SHAs or release identifiers
- `profiles`: includes `app` and `system`
- `checks`: every item below is `true`
- `app_restart`: same non-empty `task_id` and `task_id_after_restart`,
  `graph_executions` of 1, and `call_attempts` of at least 1
- `ambiguous_replay`: same non-empty task ID, `graph_executions` of 1, and
  `call_attempts` greater than `graph_executions`
- `system_recovery`: `replays` of at least 1 and `protected_mutations` of 0
- `forbidden_wire`: `methods` empty, `session_id` false, and `params_task`
  false

Required checks:

- `oauth`
- `owner_isolation`
- `input_update`
- `notification_delivery`
- `polling_recovery`
- `terminal_success`
- `terminal_failure`
- `link_restart`
- `tama_restart`
- `expired_credentials`
- `ambiguous_replay`
- `system_recovery`

A true check without the matching structured observation is not evidence. A
single boolean cannot stand in for the forbidden method, `Mcp-Session-Id`,
and `params.task` observations. Those observations are live evidence only
when they come from the future live runner. The existing mocked process
tests are the mocked gate.

No upstream request in the live log may be `initialize`, carry
`Mcp-Session-Id`, send `params.task`, or call `tasks/result` or `tasks/list`.

Phase 2 stays open until that evidence exists. Do not check the phase off
from fixture, mocked, or Compose pin results.
