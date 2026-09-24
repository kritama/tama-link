# Contributing

## Toolchain

Tama Link targets Go 1.25. Use the versions pinned in `mise.toml`.

Repository-wide implementation conventions are defined in `AGENTS.md`. In
particular, keep Go packages and files small, cohesive, and aligned with the
boundaries in the implementation plan.

```sh
mise install
make check
```

The complete local validation includes formatting, unit tests, the race
detector, `go vet`, golangci-lint, and a trimmed binary build. `make check`
includes the TamaMCP fixture gate. It does not run Compose startup or live
runtime acceptance. Those commands, and the rule that they are separate
gates, are in `wip/phase-2-acceptance.md`.

## Branching

This repository uses Git Flow:

- `main` is the production branch;
- `develop` is the integration branch;
- new work uses `feature/<name>` branches from `develop`;
- releases use `release/<version>` branches; and
- urgent production fixes use `hotfix/<name>` branches.

Start feature work with:

```sh
git flow feature start <name>
```

Do not implement compatibility behavior directly on `main` or `develop`.
Protocol and public tool-contract changes require tests and an update to the
WIP specification or its eventual stable replacement.
