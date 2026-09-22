---
name: worktree-cleanup
description: >-
  Inventory, classify, and conservatively clean Git worktrees and local branches
  across all repositories on a machine. Reports managed, provider-native, legacy,
  dirty, locked, active-bead, contained, patch-equivalent, and conflict-free merge
  candidates in human or JSON form. Use when worktrees have piled up, worktree
  placement needs auditing, or the user asks which branches can be merged or
  removed. Triggers on worktree cleanup, worktrees aufräumen, prune worktrees,
  stale worktrees, remove merged branches, and /worktree-cleanup.
triggers:
  - worktree cleanup
  - worktrees aufräumen
  - worktree aufräumen
  - prune worktrees
  - clean up worktrees
  - stale worktrees
  - remove merged branches
  - branches aufräumen
  - /worktree-cleanup
requires_standards: [worktree-subagent-discipline]
---

# worktree-cleanup

Use the installed `ccore` doctor to discover every registered worktree under
configured machine roots, deduplicate repositories by Git common directory,
classify their worktrees and local branches, and optionally remove only
proven-retired state.
Report mode is always the default. Merge candidates are never merged by this
skill; an owning delivery flow must integrate approved candidates explicitly.

## Ownership and location policy

Ownership decides who may retire a worktree; location is only a convention that
makes self-managed worktrees easy to find. Where the two disagree, the recorded
owner wins — and never a Bead-shaped branch name.

Detection and decision are different moments. At **record** time, Session Close
may use provider signals to work out the owner: `ccore` marks a worktree `t3code`
when its path sits under a T3Code session directory or the harness says so. At
**decision** time, cleanup reads only the recorded field and re-derives nothing
from the path.

Two spellings name the same two facts, and both are valid input:

| Recorded owner | Written by | Means |
|---|---|---|
| `self` | this Library's own callers | self-managed |
| `session-close` | `ccore` Session Close | self-managed |
| `provider:<id>` | this Library's own callers | provider-owned |
| `t3code` | `ccore` Session Close | provider-owned |

`ccore` treats any recorded owner other than `session-close` as provider-owned.
An owner this Library does not recognize is refused rather than guessed, so a
typo cannot quietly buy the provider exemption.

| Owner | Default location | Lifecycle |
|---|---|---|
| `cdx -b`, direct Git, or other self-managed task worktrees | `~/code/.worktrees/<repo>/<bead-or-purpose>` | calling-session close for owned work; this doctor for fleet cleanup |
| Cognovis release pipeline | `~/code/.worktrees/releases/<release-id>/<repo>` | release-specific retirement only |
| Claude-native `cld -b` | Claude's native path | Claude owns creation and primary cleanup |
| Provider-owned, such as a T3Code session worktree | whatever path that provider chose | the provider owns creation and retirement; report only |

A provider-owned worktree is valid at any path and stays provider-owned through
terminal cleanup. Treat it like `dirty` or `locked` state: report it, never remove
it. A worktree whose owner is not recorded is unknown, not self-managed, so it is
preserved.

Treat `~/.codex/worktrees`, `~/code/releases`, and direct sibling paths such as
`~/code/<repo>-worktree-*` as legacy locations. Report them; do not relocate an
active worktree in place. Release worktrees remain review-only even when their
Git commits are contained because Git evidence alone cannot prove a release is
retired. Git worktrees under `.git/beads-worktrees` are Beads internals and are
always protected.

Codex itself does not supply this local placement policy. The canonical `cdx`
wrapper owns the default and accepts `CDX_WORKTREE_ROOT` for a one-run override
or `CDX_WORKTREE_HOME` for a different shared parent.

## Commands

Requires `ccore` 2026.8.1 or newer. Run from any canonical checkout:

- Machine report: `ccore cleanup worktrees`
- Structured report: add `--json`.
- Custom discovery: add repeated `--root ~/code --root /another/root` options.
- One repository only: add `--current-repo`.
- Confirmed cleanup: add `--apply`, and add `--yes` only when confirmation was
  already obtained.

Session Close never calls the machine-wide `--apply` mode. Its exact,
identity-bound worktree scope is handled internally by `ccore session-close`
after containment, Bead finalization, and memory persistence. Do not reproduce
that private capability with Fleet Cleanup flags or pass unrelated worktrees to
Session Close.

The default discovery roots are `~/code` and `~/.codex/worktrees`. Repositories
without a secondary worktree are omitted unless `--include-singletons` is set.
Override roots with repeated `--root` arguments or the colon-separated
`WORKTREE_CLEANUP_ROOTS` environment variable. `--no-bead` disables `bd show`
lookups, but bead-shaped refs then have unknown state and are preserved.

## Classification contract

The human and JSON outputs describe the same records. Important classes are:

| Class | Meaning | Apply behavior |
|---|---|---|
| `safe_remove` | clean, unlocked, contained or patch-equivalent, and no active or unknown bead | revalidate, remove without `--force`, then revalidate and delete the branch |
| `safe_delete` | unattached local branch with equivalent evidence | revalidate and delete |
| `contained_active` | code is contained but its bead is active or unknown | keep |
| `merge_candidate` | unique patches with a conflict-free merge simulation | report for an explicit owning delivery flow; never auto-merge |
| `unmerged` | unique patches not proven conflict-free | review |
| `dirty`, `locked`, `unknown` | safety evidence is incomplete | keep |
| `release_review` | release worktree needs release-specific retirement evidence | keep |
| `prunable` | stale Git metadata | prune metadata only in apply mode |

Containment first uses ancestry, then `git cherry` patch equivalence to recognize
squash- or rebase-landed work. Mergeability uses `git merge-tree --write-tree`,
which does not change the checkout.

## Agent flow

1. Run the report without `--apply`.
2. Summarize safe removals, legacy locations, dirty/locked/active preservation,
   and merge candidates.
3. Obtain explicit user approval before a real-machine apply unless the user
   already explicitly authorized cleanup in the current request.
4. Run `--apply`; use `--yes` only after that approval.
5. Route selected `merge_candidate` records into an explicit new task or their
   owning delivery flow. They are unrelated state and must not be passed to
   Session Close. Do not merge them inside this skill.

## Safety invariants

- Protect every repository's primary checkout and default-branch worktree.
- Never force-remove a worktree.
- Preserve all dirty, locked, active-bead, unknown, release, provider-owned, and
  unmerged state.
- Decide ownership from the recorded owner, not from the path at decision time
  and not from a Bead-shaped branch name. Provider signals belong to the moment
  the owner is recorded. Delivery identity lives in the Session Close journal —
  session ID, Bead IDs, repository, branches, candidate SHA, harness, and worktree
  owner — and is never re-derived from where a worktree sits.
- Re-read worktree registration, cleanliness, lock, containment, and bead state
  immediately before each removal.
- In apply mode, historical teardown records may still be consumed for migration;
  new `ccore` Session Close runs do not publish fleet-cleanup records.
- Deduplicate repositories by absolute Git common directory so nested discovery
  roots cannot apply twice.
- Read bead state only through `bd show --json`.
- Keep Session Close's internal exact-target cleanup separate from discovery and
  broad `--apply`; neither path may inspect or remove unrelated worktrees as a
  side effect.
