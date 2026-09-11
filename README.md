# Tama Link

Tama Link is a small local compatibility proxy between Tama and AI coding
clients such as Codex, OpenCode, and Pi.

Tama can execute durable, asynchronous MCP work. Many coding clients either do
not implement MCP Tasks yet or implement different subsets of the protocol. A
client that only understands an ordinary blocking tool call cannot safely own
Tama's durable execution lifecycle, and making Tama imitate every client would
mix client compatibility concerns into the Tama server.

Tama Link keeps that boundary clean. It speaks the protocol expected by Tama on
the upstream side and exposes exactly two ordinary MCP tools to the client:

| Tool | Responsibility |
| --- | --- |
| `submit` | Start one allowed upstream Tama operation and return a durable submission identifier. |
| `await` | Wait for or inspect that submission and return progress or its terminal result. |

The detailed and authoritative implementation contract is in
[`wip/tama-link-specification.md`](wip/tama-link-specification.md).

## Why a separate project?

Tama Link isolates three things that change at different speeds:

- Tama owns execution, authorization, and durable task state.
- Tama Link owns protocol negotiation, task correlation, polling, and the
  two-tool compatibility contract.
- Product integrations such as `memovee-codex` and `memovee-opencode` own
  installation, client configuration, skills, and client-specific progress UI.

This lets Tama adopt newer MCP standards without requiring every client to
upgrade at the same time. It also lets client plugins improve presentation
without duplicating authentication, polling, retry, or result semantics.

```text
Codex / OpenCode / Pi
        |
        | submit / await
        v
    Tama Link
        |
        | negotiated MCP and durable task operations
        v
       Tama
```

Tama Link is intended to run on the user's machine as one Go binary. The
Memovee CLI will install and configure it, while the client-specific Memovee
packages will adapt its progress events to the UI their host supports.

## Current status

This repository is an implementation foundation. The CLI provides
`serve --profile <name>`, reserved `login` and `logout` stubs, and
`version [--json]`. `serve` starts the STDIO MCP server, which advertises
exactly `submit` and `await`; both handlers deliberately return a structured
`not_implemented` tool error until the upstream adapter, durable state,
authentication, and polling work described in the WIP are implemented. A named
profile must exist in the Tama Link configuration directory before `serve`
starts.

No client or installer should treat this foundation revision as production
ready.

## Development

The project targets Go 1.25 and pins development tools with
[`mise`](https://mise.jdx.dev/).

```sh
mise install
make check
make build
```

Run the MCP server over STDIO for a named profile:

```sh
go run ./cmd/tama-link serve --profile tama-app
```

Print build information:

```sh
go run ./cmd/tama-link version
go run ./cmd/tama-link version --json
```

A Codex-style STDIO registration invokes the same command, once per profile:

```text
tama-app    -> tama-link serve --profile tama-app
tama-system -> tama-link serve --profile tama-system
```

`login --profile <name>` and `logout --profile <name>` are reserved and return
a not-implemented error in this foundation.

## Branching

This repository uses Git Flow:

- `main` contains production releases;
- `develop` is the integration branch;
- feature branches start from `develop` as `feature/<name>`;
- release branches use `release/<version>`; and
- urgent fixes use `hotfix/<name>`.

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the validation workflow.

## License

Licensed under the Apache License, Version 2.0. See [`LICENSE`](LICENSE).
