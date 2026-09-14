# Repository Instructions

These instructions apply to the entire Tama Link repository. Before changing
behavior, read the relevant contracts in
`wip/tama-link-specification.md` and
`wip/tama-link-specification/plan.md`, together with `CONTRIBUTING.md`.

## Keep Go Code Small and Cohesive

- Prefer small packages, files, types, and functions with one clear
  responsibility. Do not grow a feature into a single long module.
- Treat a hand-written Go file approaching 300 lines as a review signal. Split
  it when it contains more than one responsibility or would become clearer as
  separately named components. This is a design prompt, not a quota; generated
  code and large declarative fixtures are exempt.
- Treat a function approaching 40 to 50 lines, deep nesting, or multiple phases
  of work as a signal to extract meaningful operations. Do not extract trivial
  one-line helpers merely to satisfy a line count.
- Keep MCP handlers and CLI commands thin. They should validate and translate
  input, call an application service, and translate the result. Persistence,
  upstream transport, catalog discovery, OAuth, and scheduling logic belong in
  their respective packages.
- Avoid catch-all packages or files named `utils`, `helpers`, `common`, or
  `manager`. Name components after the domain responsibility they own.
- Do not duplicate protocol behavior across profiles or commands. Put shared
  invariants in one focused component and keep endpoint-specific behavior in an
  adapter.

The package boundaries in the implementation plan are intentional. Keep
responsibilities separated across packages such as `server`, `contract`,
`submission`, `catalog`, `profile`, `store`, `worker`, `credential`, `oauth`,
`upstream`, `adapter/tama014`, and `limits`. A package may be split further when
that improves cohesion, but do not collapse these concerns into a monolith.

## Follow Idiomatic Go Design

- Prefer the standard library and the smallest adequate dependency. Add a
  dependency only when its benefit outweighs its maintenance and security cost.
- Define small interfaces at the point where they are consumed. Accept
  interfaces and return concrete types unless a boundary requires otherwise.
- Make the zero value useful where practical. Otherwise, use a constructor that
  validates required dependencies and returns a fully usable value.
- Pass `context.Context` as the first parameter for blocking or externally
  visible work. Propagate cancellation and deadlines to storage and network
  calls.
- Wrap errors with useful operation context and `%w`. Convert them to the stable
  Tama Link error taxonomy only at the contract boundary.
- Avoid package-level mutable state and side effects in `init`. Inject clocks,
  ID generation, stores, and upstream clients where deterministic tests need
  control.
- Every goroutine must have an owner, a cancellation path, and a shutdown path.
  Cross-process work must use the documented SQLite leases; in-memory locks are
  not a substitute.
- Keep exported APIs minimal. Add Go documentation to exported identifiers when
  their purpose or contract is not already obvious from the name.
- Preserve deterministic output: stable JSON field names, canonicalized
  arguments, stable hashes, and explicit sorting where iteration order could be
  observable.

## Preserve Tama Link Boundaries

- The downstream MCP surface remains exactly `submit` and `await`.
- One process profile maps to one upstream endpoint and one isolated SQLite
  database, credential namespace, catalog, and instruction set.
- Profile adapters translate endpoint-specific discovery and execution into the
  shared submission model. They must not leak upstream-specific payloads into
  the downstream contract.
- SQLite is the source of truth for accepted local work. State transitions must
  be transactional, restart-safe, idempotent, and safe with multiple Tama Link
  processes using the same profile database.
- Never write MCP protocol data to logs or diagnostics to standard output. Never
  log tokens, authorization codes, credential blobs, or unredacted sensitive
  arguments and results.

## Test at Behavioral Boundaries

- Prefer focused, table-driven tests organized by behavior. Split large test
  files when distinct behaviors can be understood independently.
- Test public behavior and durable state transitions rather than private helper
  implementation details.
- Cover validation, canonicalization, idempotency, restart recovery, lease
  contention, terminal error mapping, catalog refresh, and fixture-based schema
  projection where relevant.
- A protocol or public-contract change must update both specification documents
  and its contract fixtures or tests in the same change.
- Run `make check` before handing off an implementation. If the environment
  prevents a check, run every feasible focused check and report the exact
  limitation; do not describe an unrun check as passing.

## Change Discipline

- Inspect existing contracts and ownership before implementing. Make the
  smallest coherent change that satisfies the documented behavior.
- Prefer explicit code over speculative abstractions. Introduce an abstraction
  after a real boundary or repeated behavior is visible.
- Keep refactors separate from behavioral changes when practical so each can be
  reviewed and reverted independently.
- Do not weaken profile isolation, persistence guarantees, validation, or secret
  handling to simplify an implementation.
