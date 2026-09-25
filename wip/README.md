# Tama Link WIP Index

This directory contains unfinished design, delivery, and acceptance work. It is
not an archive. Completed or superseded plans and resolved review documents are
removed after their durable decisions have been incorporated into the
authoritative specification or permanent documentation.

## Authority

`tama-link-specification.md` is the authoritative product and protocol
contract. Supporting documents refine execution or record acceptance evidence;
they do not override the specification.

When documents disagree, the specification governs and the supporting document
must be reconciled before implementation or acceptance continues.

## Active documents

| Document | Role | Status | Tracking |
| --- | --- | --- | --- |
| [`tama-link-specification.md`](tama-link-specification.md) | Authoritative product and protocol contract | Implementation handoff | Repository scope |
| [`tama-link-specification/plan.md`](tama-link-specification/plan.md) | Phase roadmap and dependency ledger | Active | [#9](https://github.com/kritama/tama-link/issues/9) |
| [`tama-link-specification/plans/login.md`](tama-link-specification/plans/login.md) | Interactive OAuth login implementation plan | Implemented; live acceptance pending | [#15](https://github.com/kritama/tama-link/issues/15) |
| [`tama-link-specification/acceptance/phase-2.md`](tama-link-specification/acceptance/phase-2.md) | Phase 2 gate definitions and required live evidence | Active | [#8](https://github.com/kritama/tama-link/issues/8) |
| [`tama-link-specification/reviews/issue-15.md`](tama-link-specification/reviews/issue-15.md) | Issue #15 implementation re-review | 1 open finding | [#15](https://github.com/kritama/tama-link/issues/15) |

## Layout convention

```text
wip/
├── README.md
├── tama-link-specification.md
└── tama-link-specification/
    ├── plan.md
    ├── plans/
    │   └── <feature>.md
    ├── acceptance/
    │   └── <phase-or-release>.md
    └── reviews/
        └── <issue-or-pr>.md
```

- Keep only the index and authoritative specification at the WIP root.
- Use `plan.md` for phase ordering, dependencies, and current delivery status.
- Use `plans/` for active feature plans. Name files by feature because the
  directory already supplies the document type.
- Use `acceptance/` for executable gates and evidence requirements, not design
  decisions.
- Use `reviews/` only for unresolved actionable findings. Delete a review when
  all findings are resolved; do not retain it as history.
- Use lowercase hyphenated names. Prefer stable issue or feature names over
  dates or `current-work` suffixes.

## Lifecycle

1. Add `Status`, `Updated`, and tracking metadata to every active plan.
2. Link the plan from this index and from the relevant roadmap phase.
3. Keep contract decisions synchronized with `tama-link-specification.md`.
4. When implementation lands, merge lasting decisions into the specification
   and delete the completed feature plan.
5. When an acceptance gate passes, move reusable operational material to
   permanent `docs/` documentation and remove its WIP artifact.
