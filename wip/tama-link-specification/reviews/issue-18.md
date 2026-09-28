# Linux Default Keyring Review

Status: R1–R3 resolved; retained pending live acceptance
Updated: 2026-09-28
Target: Phase 3 keyring recovery and live acceptance
Tracking: [kritama/tama-link#18](https://github.com/kritama/tama-link/issues/18)
Related acceptance: [#15](https://github.com/kritama/tama-link/issues/15), [#8](https://github.com/kritama/tama-link/issues/8)
Reviewed branch: `feature/linux-default-keyring`
Reviewed base: `31a4f097bf4e037ace2e71c9507403f9df317843`

## Scope and Authority

This document records the resolution of the Linux default-alias Secret Service
and legacy repair review. The [specification](../../tama-link-specification.md)
and [implementation plan](../plan.md) remain authoritative. The review is
retained pending the live-acceptance gates below; passing fixtures does not
establish those gates.

## Findings and Resolution

| Finding | Resolution | Regression coverage |
| --- | --- | --- |
| R1 — P2: Concurrent repair can create duplicate destination items | Repair holds the same durable profile credential lease as OAuth writers, rechecks the destination before copying, and fails closed on duplicates and conflicting values. Removal deletes every matching item. | `TestConcurrentRepairPublishesOneItem`, `TestRemoveDeletesDuplicateCredentialItems` |
| R2 — P2: Cancellation does not cover all D-Bus operations | Property reads use `CallWithContext` and discard results after cancellation. The two-minute repair budget starts before dialing. Cancelled setup closes the raw socket before joining setup and closing godbus, releasing blocked `Hello` and `AddMatchSignal` writes. | `TestPropertyReadHonorsCancellation`, `TestAuthenticateReleasesBlockedSetup`, `TestAuthenticateCancelsBlockedSetupWrite`, `TestCancelledLabelReadDoesNotWrite`, `TestRepairBoundsDialToInteractionBudget` |
| R3 — P2: An invalid state key or malformed required record can contaminate the destination | Candidate state-key bytes must decrypt existing ciphertext, and required credential records must validate before any destination write. Source items remain available for a corrected retry. | `TestWrongStateKeyDoesNotContaminateDestination`, `TestMalformedRequiredRecordDoesNotContaminateDestination` |

The final R2 follow-up exposed a lock-order problem in the pinned godbus
implementation: message writes hold the output-handler read lock, while
`Conn.Close` takes its write lock before closing the transport. Closing godbus
first therefore cannot interrupt a blocked request write. The fix closes the
underlying socket first and waits for the owned setup goroutine to finish.

The repository regression uses an in-memory D-Bus peer that completes
authentication and then stops reading either the `Hello` or `AddMatchSignal`
request. Both cases failed before the fix and pass after it. This covers the
production godbus write path, rather than substituting a cancellable service
fixture for the blocked transport.

## Validation Evidence

### CodeRabbit PR #20 follow-up

Both actionable review findings are resolved:

- The cancelled-dial test pins a test-owned unavailable Unix socket address and
  runs without `t.Parallel`, so it works with an absent or incompatible desktop
  session-bus environment.
- Migration selects only the state key and requested required/optional keys.
  Filtering happens before secret reads and conflict checks. Regression tests
  preserve requested optional/retired items while excluding malformed,
  parseable, and conflicting unrelated items in the same namespace.

The partial-publication concern is captured by an explicit recovery contract:
an interruption may leave exact destination copies, retains sources, and an
explicit retry revalidates and completes only missing items. The
`TestInterruptedRepairResumesExactCopies` regression checks this behavior,
including one destination match per key and no source deletion. Live provider
acceptance remains a separate gate.

The following checks passed on 2026-09-28:

| Check | Result | Boundary |
| --- | --- | --- |
| `make check` | Passed | Formatting, unit tests, race detector, vet, lint, and trimmed Linux build; loopback fixtures ran outside the sandbox. |
| CodeRabbit selection and recovery regressions with `-race -count=20` | Passed | Requested keys and optional items, unrelated malformed/parseable/conflicting values, and exact-copy recovery after an interrupted repair. |
| Cancelled-dial test with absent and incompatible host addresses, `-race -count=20` | Passed | Host-independent controlled Unix address. |
| Setup cancellation regressions with `-race -count=100` | Passed | Blocked authentication plus blocked `Hello` and `AddMatchSignal` writes return after cancellation. |
| Six remediation regressions with `-race -count=20` | Passed | Concurrent repair, duplicate removal, cancellation without writes, stalled unlock, wrong state key, and malformed required record. |
| Property-read and repair-budget regressions with `-race -count=20` | Passed | Production property helper cancellation, cancelled label reads, socket cancellation, and budget-before-dial behavior. |
| Windows amd64 and macOS arm64 cross-builds | Passed | Compile compatibility; does not establish platform runtime behavior. |
| `go mod tidy -diff` and `git diff --check` | Passed | Module consistency and patch whitespace. |

## Remaining Live Acceptance

- Verify default-alias reuse and existing-keyring unlock behavior against real
  GNOME Secret Service and KWallet installations without creating collections.
- Migrate a profile affected by the old implementation, preserving its encrypted
  SQLite state and exact OAuth credentials while retaining source items.
- Start a fresh process after login or repair and confirm state-key and OAuth
  reuse through `serve` and the real `submit`/`await` workflow.
- Confirm unattended startup fails promptly when the default collection is
  locked, without prompting or creating a replacement collection.

These remain separate gates under issues #18 and #8. The automated checks above
do not establish live migration, process-restart, or runtime acceptance.
