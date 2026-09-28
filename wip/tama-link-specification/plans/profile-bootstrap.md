# Tama Link Interactive Profile Bootstrap Plan

Status: Implemented; live acceptance pending
Updated: 2026-09-25
Target: Phase 3
Tracking: Unassigned follow-up to [kritama/tama-link#15](https://github.com/kritama/tama-link/issues/15)
Managed profile producer: [kritama/memovee-cli#3](https://github.com/kritama/memovee-cli/issues/3)

## Purpose

Make `tama-link login` useful on a fresh machine without requiring another
tool to create a profile first.

When attached to an interactive terminal, the command asks for the Tama
instance address, selects a reviewed App or System profile template, completes
OAuth, discovers the authenticated TamaMCP catalog, and atomically publishes a
valid version 2 profile. A later `serve --profile <name>` process uses that
profile and the credentials committed by the same login.

The authoritative specification now records this self-service path alongside
managed profile creation. Memovee CLI still owns profiles it manages. Tama Link
creates only a previously absent profile and never edits an existing one.
Live App and System-read acceptance remains a separate pending gate.

## Problem

The existing-profile command remains:

```text
tama-link login --profile <name> [--config-dir <dir>] [--no-browser]
```

That path loads the named profile before it can discover OAuth metadata or
start the browser flow. On a fresh installation there is no profile to name,
so login also accepts an explicit bootstrap form:

```text
tama-link login [--address <https-origin>] [--type <app|system>] [--profile <name>] [--issuer <https-url>] [--config-dir <dir>] [--no-browser] [--yes]
```

Without that form, a user cannot use Tama Link by itself even when the Tama
instance is already running.

Prompting for only an address is not sufficient. A profile also pins:

- the exact protected-resource endpoint and expected authorization issuer;
- explicit least-privileged OAuth scopes;
- protocol compatibility bounds;
- approved operation schemas, execution strategies, task behavior, and
  declarative argument bindings;
- upstream instructions and descriptor digests; and
- isolated state and credential namespaces.

Those values are security policy. They must not be inferred by trusting every
tool or scope an arbitrary endpoint advertises.

## Ownership Boundary

### Tama Link owns self-service bootstrap

Tama Link may create a new profile only through this explicit interactive
flow. It owns:

- terminal prompts and non-interactive input validation;
- endpoint and OAuth metadata discovery;
- reviewed built-in profile templates for supported Tama endpoints;
- OAuth authorization and credential persistence;
- authenticated TamaMCP discovery used to materialize the selected template;
- crash-safe bootstrap state and atomic no-replace profile publication; and
- deterministic output, cleanup, and diagnostics.

### Memovee CLI continues to own managed installations

Memovee CLI continues to own binary pinning, topology orchestration, and
deterministic creation and reconciliation of profiles it manages. Tama Link
must not silently rewrite, reconcile, or take ownership of an existing managed
profile.

The initial self-service feature creates only a previously absent profile.
Because it does not update existing profiles, profile version 2 does not need a
new provenance field. If Tama Link later gains profile editing or
reconciliation, the profile schema must first gain an explicit management
owner and conflict policy.

### The user and authorization server retain their roles

The user enters the instance address and performs authentication and consent in
the browser. The authorization server owns credentials, consent, codes, and
tokens. Tama Link never asks for a password, copies browser cookies, or accepts
tokens from a prompt, flag, environment variable, or profile file.

## User Experience

### Fresh installation

With no profiles and an interactive terminal:

```text
$ tama-link login
Tama address: https://tama.example
Type [app]:
Profile name [tama-app]:
Authorization server: https://accounts.example
Access: App messaging (mcp.message)
Continue in your browser? [Y/n]
```

The address is the path-free HTTPS origin of the Tama instance. The type is one
of exactly two values and defaults to `app` when the user presses Enter:

| Type | Derived endpoint | Default profile name |
| --- | --- | --- |
| `app` | `<address>/mcp/app` | `tama-app` |
| `system` | `<address>/mcp/system` | `tama-system` |

The user enters only the instance address; Tama Link always derives the MCP
path from the selected type. An address containing `/mcp/app`, `/mcp/system`,
or any other non-root path is rejected with an instruction to pass the origin
and `--type` separately.

The normalized exact endpoint, selected authorization issuer, requested scope
set, profile name, and operation bundle are shown before the browser opens.
The user must confirm the summary.

### Existing profiles

When profiles already exist, `tama-link login` presents a deterministic menu:

1. log in an existing valid profile; or
2. add another Tama instance.

`tama-link login --profile <name>` preserves the current direct path when the
profile exists. If the name is absent and the command is interactive, Tama
Link asks whether to create it and then starts the address wizard. It never
turns a misspelled profile name into a new profile without confirmation.

Invalid or unreadable profile files are reported but cannot be selected. The
listing must use the same secure file and name validation as `profile.Load`;
symlinks and special files are never followed.

### Profile naming

The default is `tama-app` or `tama-system`, derived from the selected type.
If that name exists, the command asks for a different portable profile name.
It never overwrites, merges, or edits the existing file.

The new profile uses deterministic inner state references:

```json
{"database":"default","credentials":"default"}
```

The validated profile name remains the outer namespace, so two profiles using
these inner references still have separate databases and credential stores.

### Interactive and flag-driven inputs

Every value that can be entered in the wizard also has a CLI flag. Explicit
flags take precedence and suppress the matching prompt; omitted values are
prompted when input is a terminal. This supports fully interactive, partially
specified, and fully specified invocations without separate behavior paths.

```text
# Fully interactive; app is the type default.
tama-link login

# Address is known; ask for the remaining interactive choices.
tama-link login --address https://tama.example

# System profile with a derived default name.
tama-link login --address https://tama.example --type system

# Fully specified bootstrap when the issuer choice is already known.
tama-link login \
  --address https://tama.example \
  --type app \
  --profile tama-production-app \
  --issuer https://accounts.example \
  --yes
```

The proposed command surface is:

```text
tama-link login \
  [--address <https-origin>] \
  [--type <app|system>] \
  [--profile <name>] \
  [--issuer <https-url>] \
  [--config-dir <dir>] \
  [--no-browser] \
  [--yes]
```

| Wizard field | Flag | Default when omitted |
| --- | --- | --- |
| Tama address | `--address` | Prompt interactively; required for new non-interactive bootstrap |
| Profile type | `--type` | `app` |
| Profile name | `--profile` | `tama-app` or `tama-system` |
| Authorization server | `--issuer` | The sole valid advertised issuer; prompt or fail when ambiguous |
| Summary confirmation | `--yes` | Prompt on a terminal; implicit only for non-interactive invocation |

- `--address` supplies the path-free Tama origin and skips the address prompt.
- `--type` accepts only `app` or `system`; omission defaults to `app`.
- The type deterministically appends `/mcp/app` or `/mcp/system` and selects
  the corresponding reviewed internal profile template.
- `--profile` supplies the new profile name. When omitted, the default is
  `tama-app` or `tama-system` according to the selected type.
- `--issuer` selects one authorization server already advertised by protected-
  resource metadata. It never overrides or invents an unadvertised issuer.
- `--yes` skips only the final normalized-summary confirmation. It never
  weakens URL, metadata, scope, template, or credential validation.
- If a named profile already exists, the current existing-profile login path is
  used. Explicit `--address`, `--type`, `--issuer`, or `--yes` values are
  rejected on that path because the existing profile owns those values and
  does not need bootstrap confirmation.

The CLI tracks whether a flag was explicitly supplied, rather than treating a
defaulted value as user input. That distinction lets an interactive invocation
show `Type [app]` while a non-interactive invocation safely uses `app` without
waiting for input.

### Non-interactive behavior

The command must never wait for prompts when standard input is not a terminal.
`--address` is required for a new non-interactive bootstrap. `--type` defaults
to `app`, and `--profile` derives from the type when omitted. If metadata
advertises multiple valid authorization servers, non-interactive bootstrap
requires `--issuer`. Any other missing or ambiguous input is a usage error with
exit status `2`.

The implementation will support an explicit automation form:

```text
tama-link login \
  --address <https-origin> \
  [--type <app|system>] \
  [--profile <new-name>] \
  [--issuer <https-url>] \
  [--config-dir <dir>] \
  [--no-browser] \
  [--yes]
```

- `--address` is always an origin; the type selects the exact endpoint.
- `--type` selects a reviewed built-in policy, not an arbitrary catalog, and
  defaults to `app`.
- `--issuer` is required only when protected-resource metadata advertises more
  than one otherwise valid authorization server.
- The template supplies its exact required scopes. There is no flag that means
  "request every advertised scope."
- `--no-browser` retains its current meaning: print the authorization URL to
  standard output and wait for the callback. It does not enable prompting on a
  non-terminal input stream.

Interactive prompts and progress go to standard error. Standard output remains
reserved for the intentional manual authorization URL, preserving the current
output contract.

## Contract Decisions

### 1. Endpoint input is constrained

Bootstrap accepts one path-free Tama address and one of exactly two types. The
mapping is fixed:

```text
app    -> <address>/mcp/app
system -> <address>/mcp/system
```

`app` is the default type. No flag accepts an arbitrary MCP endpoint path, so
bootstrap does not become a generic MCP gateway.

Persisted origin, endpoint, and issuer URLs follow the current profile
contract:

- HTTPS is required;
- user information, query strings, and fragments are rejected;
- the address has no path other than `/`; and
- the endpoint is derived from the validated address and selected type.

This feature does not weaken the profile URL policy. Supporting plain HTTP on
an exact loopback address would require a separate authoritative contract
amendment and tests across profile, OAuth, and upstream transports.

All metadata and MCP clients reject redirects. A bearer token, authorization
code, client secret, or registration request must never follow a `Location`
header to a new destination.

### 2. OAuth bootstrap discovery is a separate stage

The current OAuth client requires an expected issuer before it can select an
authorization server. Profile bootstrap therefore needs a small discovery
component that operates before the final OAuth client exists:

1. Fetch and validate RFC 9728 protected-resource metadata for the exact
   endpoint.
2. Require the metadata resource to equal that endpoint.
3. Validate every candidate authorization-server URL before displaying it.
4. Auto-select one candidate, or require an explicit user/flag selection when
   several remain.
5. Fetch and validate the selected RFC 8414 metadata.
6. Pin the authorization-server document's exact issuer into the candidate
   profile.

The selected issuer and token endpoint remain subject to the existing exact
issuer and same-origin token-endpoint checks. Metadata response sizes and
deadlines use the existing OAuth bounds.

### 3. Reviewed templates supply policy

Add a versioned profile-template registry owned by the `adapter/tama2026`
boundary. A template is selected by endpoint kind and declares:

- the supported MCP protocol bounds;
- the expected bounded server instructions and complete approved operation
  descriptor prototypes;
- execution strategy and expected task behavior for each operation;
- exact upstream, client-visible, and output schemas plus reviewed declarative
  bindings;
- exact scopes required by each bundle; and
- whether a missing, changed, or newly advertised tool is fatal or ignored.

Initial templates are:

- `app`: the App task-backed message operation and its required caller-owned
  context binding; and
- `system-read`: the reviewed replayable System inspection operations. Unsafe
  or unreconciled System mutations remain absent. `reflection.comments.review`
  is absent from this bundle because its `mcp.reflection.review` scope is not
  requested and Tama hides the tool from the authenticated catalog.

The exact System operation and scope list must be copied from the reconciled
managed profile contract during implementation; this plan does not invent
names that are not present in current repository evidence.

Templates are code-reviewed release content, not files downloaded from the
entered address. A server cannot grant itself a strategy, binding, or scope by
advertising a tool with a familiar name. A template revision that supports a
new Tama contract ships as reviewed Tama Link code and compatibility data; the
wizard does not synthesize compatibility policy at runtime.

### 4. Scope selection follows the template

The wizard asks the user to select a reviewed access bundle, not raw OAuth
scope strings. The template maps that bundle to the least-privileged canonical
scope set.

Before browser launch, the requested set must be a subset of each authoritative
`scopes_supported` list that is present. A present empty list cannot satisfy a
non-empty template. Missing scope metadata follows the existing discovery
contract; it never causes Tama Link to request all scopes.

The confirmed canonical scope set participates in registration,
authorization, token validation, credential binding, and the final profile
digest exactly as it does for an existing version 2 profile.

### 5. Authenticated discovery materializes the profile

After OAuth succeeds, the same in-process client uses its memory-held access
token to perform bounded stateless TamaMCP discovery:

1. call `server/discover` and require MCP `2026-07-28` plus the capabilities
   required by the selected template;
2. read the complete paginated `tools/list` under the existing cumulative hard
   response ceiling;
3. intersect the live tools with the selected template;
4. verify the live instructions and every security-relevant tool field against
   the template's pinned instructions and descriptor prototypes;
5. reject missing operations or any template drift;
6. ignore additional live tools rather than enabling them;
7. place the reviewed template instructions and descriptors in the profile;
8. compute every descriptor digest; and
9. compute the complete version 2 profile digest.

This is profile construction, not the normal runtime drift check. It may copy
only fields the template explicitly defines as non-security metadata and bounds
accordingly. It must not convert an arbitrary live tool directly into an
approved descriptor or pin a server-controlled schema merely because the user
entered that server's address.

### 6. The candidate uses final namespaces from the start

The selected profile name and deterministic state references are finalized
before OAuth begins. Bootstrap opens the same profile-isolated database path
and credential namespace that the published profile will use.

Runtime construction is split so OAuth can use a validated bootstrap candidate
without pretending that an incomplete candidate is a loadable `Profile`.
`profile.Validate` continues to require a complete operation catalog.

The existing `oauth/login` and `oauth/refresh` leases and credential fences
remain authoritative. Bootstrap adds a profile-name-scoped durable claim so two
processes cannot create the same profile concurrently.

### 7. Bootstrap is resumable and never publishes a partial profile

Before opening the browser, Tama Link writes a bounded non-secret bootstrap
journal beneath a private staging directory in the configured profile root.
The journal records:

- the candidate profile name, endpoint, origin, issuer, template, and scopes;
- deterministic state references;
- a random bootstrap identifier;
- the current non-secret stage; and
- a version for forward-compatible recovery.

It contains no authorization URL, state, PKCE verifier, code, token, client
secret, or credential payload. It is not recognized by `profile.Load` or
`serve` as a profile.

On restart, `tama-link login` detects the journal and offers to resume or
discard it. Resume revalidates metadata, the template, the secure credential
binding, and every completed stage before continuing. Discard uses the
credential cleanup path under the normal leases, removes any profile-isolated
bootstrap database that has no published profile, and finally removes the
journal. Cleanup must never touch an existing profile or another namespace.

If OAuth commits but authenticated discovery or profile publication fails, the
journal remains. The command reports that bootstrap is incomplete and can be
resumed; it must not report login success or leave credentials with no durable
record describing their owner.

### 8. Profile publication is secure and no-replace

Add a focused profile writer alongside the existing secure loader. It must:

- create private configuration and profile directories without following
  links;
- hold pinned parent directory handles while creating children;
- write a unique temporary regular file owned by the current user with private
  permissions or ACLs;
- encode deterministic JSON, flush the file, and publish it atomically;
- use a platform-specific atomic no-replace primitive so an existing target
  always wins;
- flush the directory where the platform permits;
- reload and validate the published profile; and
- remove the bootstrap journal only after reload succeeds.

The writer never exposes a partial target file and never uses a replace-style
rename for the final path. A concurrent managed installer or second Tama Link
process may win the name; the loser preserves the winner and fails safely.

### 9. Success means profile plus durable credentials

Bootstrap exits `0` only when all of these are true:

- the version 2 profile was atomically published and reloads successfully;
- the profile digest and descriptor digests verify;
- the fenced registration and refresh credential bind the profile's issuer and
  scope set; and
- the journal has been removed.

The access token remains memory-only. The command does not start `serve` or
submit hidden work. A fresh `serve --profile <name>` process remains the
observable runtime acceptance step.

## Execution Flow

For a new profile, the application service performs these steps:

1. Detect terminal capability and collect or validate CLI inputs.
2. Validate the path-free address and derive one exact endpoint from the
   selected `app` or `system` type.
3. Resolve and exclusively reserve a new portable profile name.
4. Perform pre-profile OAuth metadata discovery and select the issuer.
5. Select a reviewed template/access bundle and validate its scopes against
   advertised metadata.
6. Display the normalized summary and obtain interactive confirmation.
7. Create the non-secret bootstrap journal and open the final state and
   credential namespaces.
8. Acquire the bootstrap and login leases.
9. Run the existing browser, callback, PKCE, registration, token, and fenced
   credential flow.
10. Use the memory-held access token for authenticated `server/discover` and
    complete `tools/list` discovery.
11. Materialize template-approved descriptors, instructions, bounds, scopes,
    state references, and digests into a complete profile candidate.
12. Validate the complete candidate, atomically publish it without replacement,
    and reload it from disk.
13. Verify durable credential readiness against the reloaded profile.
14. Remove the journal, release leases, and report the created profile name and
    the exact next `serve` command on standard error.

Cancellation before credential commit removes ephemeral OAuth material and may
discard an untouched journal. Cancellation or failure after a durable
credential mutation preserves the resumable journal and sanitizes the error.

## Package Boundaries

- `cmd/tama-link/login.go` owns flags, TTY selection, stable presentation, and
  exit mapping. It does not build profiles or perform discovery.
- `internal/login` continues to own the browser authorization attempt. Its
  existing-profile path remains independently testable.
- A focused `internal/bootstrap` package owns the application sequence,
  journal lifecycle, resume/discard decisions, and small interfaces for
  prompting, discovery, templates, profile publication, and runtime opening.
- `internal/oauth` owns the pre-profile metadata-discovery primitive and the
  existing authorization mechanics. It does not write profile files.
- `internal/adapter/tama2026` owns the reviewed template registry and live
  TamaMCP materialization rules.
- `internal/profile` owns candidate validation, digest construction, secure
  listing, and atomic no-replace publication.
- `internal/store` continues to own generic durable leases and credential
  fences. Bootstrap supplies names and stages; it does not add an in-memory
  process lock as the cross-process authority.
- `internal/credential` continues to own the secure backend. Bootstrap never
  receives raw secret values merely to copy them between namespaces.

Keep the prompt implementation behind a small interface so tests do not depend
on a real terminal and non-interactive callers cannot accidentally enter an
input loop.

## Failure Behavior

The command distinguishes these sanitized classes:

- no terminal and insufficient explicit inputs;
- invalid or unsupported address/resource path;
- insecure URL or redirect attempt;
- profile-name collision or concurrent bootstrap;
- invalid protected-resource or authorization-server metadata;
- ambiguous authorization-server selection;
- unsupported template, scope set, protocol, or capability;
- user cancellation before browser launch;
- existing browser/callback/OAuth failures;
- authenticated discovery or catalog materialization failure;
- secure keyring, state, journal, or profile-publication failure;
- incomplete bootstrap available for resume; and
- failed cleanup that remains durably recorded for a later retry.

Exit status remains `0` for complete success, `1` for runtime, network,
authorization, persistence, or recovery failures, and `2` for invalid usage or
input. Errors never include metadata bodies, authorization URLs, codes,
tokens, client secrets, sensitive tool arguments, or keyring payloads.

## Test Plan

### CLI and prompt behavior

- zero, one, and multiple existing profiles;
- add-new and existing-profile menu paths;
- absent `--profile` confirmation rather than typo-driven creation;
- path-free `--address` input, `--type app|system`, the `app` default, and exact
  `/mcp/app` or `/mcp/system` endpoint derivation;
- rejection of address paths, unknown types, and endpoint/type ambiguity;
- fully interactive, partially specified, and fully specified invocation;
- explicit flags suppress only their matching prompts;
- `--yes` suppresses only confirmation and does not weaken validation;
- deterministic default names and collision handling;
- confirmation, cancellation, EOF, and interrupted input;
- non-terminal input fails without prompting or hanging;
- explicit automation flags, ambiguous issuer handling, and `--no-browser`;
- stable stdout/stderr separation and exit statuses; and
- no ANSI output when not attached to a terminal.

### Discovery and templates

- exact resource binding and supported endpoint paths;
- HTTPS, user-info, query, fragment, metadata size, timeout, and redirect
  rejection;
- one and multiple authorization-server candidates;
- issuer and token-endpoint pinning;
- missing, empty, supported, and conflicting advertised scope lists;
- App and System template materialization;
- missing approved tools, duplicate live tools, schema drift, missing Tasks
  capability, and unsupported protocol;
- extra live tools ignored; and
- deterministic descriptor and profile digests across ordering differences.

### Persistence and recovery

- secure first creation of configuration, staging, profile, and state paths;
- rejection of symlinks, special files, foreign ownership, and permissive
  permissions or ACLs;
- atomic no-replace publication and concurrent creator races;
- process death before OAuth, after credential commit, during catalog
  discovery, during file flush, after publication, and before journal removal;
- resume revalidation and idempotent completion;
- discard cleanup under credential fences and leases;
- a publication loser never alters the winner's profile or credentials; and
- an existing managed profile is never edited.

### Security and runtime acceptance

- secrets absent from journals, profiles, SQLite metadata, output, errors,
  logs, panic output, and snapshots;
- authenticated discovery uses the just-committed in-memory access token;
- template scope and credential bindings remain exact;
- `go test -race ./...`, `make check`, and supported cross-builds; and
- live fresh-machine App and System-read bootstrap followed by a separate
  `serve`, real `submit`/`await`, restart, and credential refresh.

Fixture and mock success do not satisfy the live gate.

## Delivery Phases

### Phase 1: Contract alignment

- Open a dedicated tracking issue.
- Update the authoritative ownership, authentication/profile, command-surface,
  and acceptance sections.
- Reconcile `plans/login.md` so its existing-profile-only boundary is retained
  as one path rather than stated as the complete login contract.
- Clarify `kritama/memovee-cli#3`: it remains the managed profile producer but
  is no longer the only way to create a profile.

Gate: the self-service and managed ownership boundaries, command grammar,
template policy, recovery semantics, and live acceptance are approved before
implementation starts.

### Phase 2: Secure profile construction primitives

- Add secure profile listing and atomic no-replace publication.
- Add bootstrap-candidate and journal models with strict bounds and validation.
- Add the pre-profile OAuth metadata discovery component.
- Add reviewed App and System-read templates and deterministic materialization.

Gate: focused tests prove that untrusted address and live-catalog data cannot
create policy outside the selected template or overwrite an existing profile.

### Phase 3: Interactive orchestration

- Add the prompt abstraction, TTY detection, menus, confirmation, and explicit
  automation inputs.
- Compose metadata discovery, the existing login service, authenticated MCP
  discovery, candidate construction, and publication in `internal/bootstrap`.
- Add durable bootstrap ownership and journal resume/discard behavior.

Gate: deterministic integration and separate-process tests cover success,
cancellation, crash recovery, contention, and secret hygiene.

### Phase 4: Documentation and repository gates

- Update CLI help and permanent user documentation with fresh, existing,
  manual-browser, resume, and managed-installation workflows.
- Run the complete race, lint, fixture, repository, and cross-build gates.
- Keep the managed Memovee CLI workflow covered independently.

Gate: a distributable binary can start the documented bootstrap without any
development-only profile fixture or hand-written JSON.

### Phase 5: Live acceptance

- Bootstrap one clean App profile and one clean System-read profile against the
  pinned local Tama topology.
- Start fresh `serve` processes and exercise the real downstream workflows.
- Restart Link, refresh credentials, and prove durable recovery.
- Capture immutable TamaMCP, Tama, Tama Link, provider, profile-template, OS,
  architecture, and client evidence.

Gate: both resources pass the real browser, keyring, profile publication,
MCP, restart, and refresh workflow. Static, fixture, mocked, or Compose-only
evidence remains insufficient.

## Acceptance Criteria

This feature is complete when:

1. A fresh interactive installation can run `tama-link login` without a
   pre-existing profile.
2. Every wizard value has a corresponding CLI flag, so known values can be
   supplied without being prompted again.
3. `--type` accepts exactly `app` or `system`, defaults to `app`, and derives
   `/mcp/app` or `/mcp/system` from the path-free Tama address.
4. The user can enter a different supported Tama address and confirm the exact
   normalized endpoint and issuer before authorization.
5. Address and live catalog data cannot broaden the reviewed template's
   operations, strategies, bindings, scopes, or protocol bounds.
6. Existing-profile login remains behaviorally compatible.
7. Non-interactive invocation never prompts or hangs and has a complete
   explicit-input form.
8. Profile creation is private, atomic, deterministic, digest-bound, and never
   replaces an existing file.
9. Two processes cannot publish or authorize conflicting profiles under the
   same name.
10. Every durable credential belongs either to a published profile or to a
   resumable, non-secret bootstrap journal.
11. Crash recovery can resume or safely discard every recorded stage without
   exposing or overwriting secrets.
12. A successful command leaves a reloadable version 2 profile and matching
    fenced credentials, then prints the exact next `serve` command.
13. A failed command never reports a ready profile, edits a managed profile,
    enables extra live tools, or leaks secrets.
14. Automated, race, repository, cross-build, and live App/System-read gates
    pass with separately recorded evidence.

## Out of Scope

- editing or reconciling an existing profile;
- importing profiles or credentials from arbitrary files;
- accepting arbitrary MCP endpoint paths or generic provider adapters;
- discovering Tama instances through LAN scanning, DNS search, or browser
  history;
- accepting raw OAuth tokens, passwords, or client secrets from the terminal;
- automatically enabling every advertised tool or scope;
- starting `serve` or submitting an MCP operation from login;
- implementing `logout`, `doctor`, or client progress as part of bootstrap; and
- changing the downstream MCP surface beyond `submit` and `await`.

## Documentation Reconciliation Checklist

When implementation begins, update these statements together:

- `wip/tama-link-specification.md`: supporting-plan list, ownership table,
  authentication/profile model, command surface, acceptance criteria, and
  Phase 3 scope;
- `wip/tama-link-specification/plan.md`: Memovee CLI ownership, D8 profile
  production language, Phase 3 work, remaining gates, and attack order;
- `wip/tama-link-specification/plans/login.md`: purpose, non-goals, command
  surface, execution flow, tests, and acceptance criteria that currently
  require an existing profile;
- permanent CLI help and user documentation; and
- the Tama Link and Memovee CLI tracking issues so neither claims exclusive
  ownership inconsistent with the approved contract.
