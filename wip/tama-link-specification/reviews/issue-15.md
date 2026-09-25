# Issue #15 Re-review

Status: Open — 1 actionable finding
Updated: 2026-09-25
Tracking: [kritama/tama-link#15](https://github.com/kritama/tama-link/issues/15)
Branch: `feature/phase-3-login`
Reviewed head: `5ae14c42c3606bd36fe2649d4e8a0cc3f9828316`
Base: `c1c374a` (`develop`)

## Outcome

The six findings from the previous review are resolved. The remediation commit
introduced one new Windows browser-handoff regression, so the branch is not yet
clear of actionable review findings.

## Finding

### F1 — P1 — Windows invokes the Control Panel loader instead of the URL handler

Location: `internal/login/browser_windows.go:10`

The Windows browser command now runs:

```text
rundll32 shell32.dll,Control_RunDLL <authorization-url>
```

`Control_RunDLL` is a Control Panel loader, not the URL protocol handler. The
previous implementation used `url.dll,FileProtocolHandler`, which delegates the
authorization URL to the user's registered browser. If the current command
exits successfully without opening the URL, `handoff` treats the launch as
successful, suppresses the manual authorization URL, and waits for a callback
the user has no way to initiate through the default flow.

The existing platform-command test cannot detect this regression because
`internal/login/browser_platform_test.go` is excluded on Windows by its
`//go:build !windows` constraint. The Windows cross-build proves only that the
command compiles.

Required resolution:

1. Restore a tested native Windows URL-handler invocation, preserving the URL
   as one argument without a shell.
2. Add a Windows-specific command-construction test that verifies the
   executable, handler argument, and exact single URL argument.
3. Re-run `make check` and all supported cross-builds. Runtime validation on a
   Windows host should confirm that the default path opens the authorization
   URL and that opener failure still presents the manual handoff.

Resolution status: Open.

## Validation Evidence

The following checks passed at the reviewed head:

- `GOCACHE=/tmp/tama-link-review-gocache go test -race -count=20 ./internal/login ./internal/oauth ./internal/profile`
- `make check`
- `CGO_ENABLED=0` cross-builds for Linux amd64/arm64, macOS amd64/arm64, and
  Windows amd64
- `git diff --check`

The worktree was clean and the local branch matched
`origin/feature/phase-3-login`. These results do not exercise the Windows
browser handoff at runtime.

Live App/System login and `submit`/`await` acceptance remains a separate open
gate under issue #8 and is not established by these review checks.
