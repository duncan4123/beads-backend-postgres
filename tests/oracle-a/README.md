# Oracle A — refactor-safety differential conformance for `bd`

Oracle A answers one question mechanically: **did this branch change any
user-visible `bd` behavior that Gas City's contract surface depends on?**

It runs the same gc-contract scenarios against two separately-built `bd`
binaries and diffs every step:

| role | built from | meaning |
|---|---|---|
| **REFERENCE** | `origin/main` (overridable via `REF_REF`) | the "before" |
| **CANDIDATE** | the current working tree (HEAD + uncommitted) | the "after" |

Each scenario is an ordered list of `bd` CLI argv steps run as real processes in
a throwaway workspace (`bd init` + steps). For every step the harness compares
**exit code**, **stderr**, and **JSON-aware stdout** (object key order ignored;
array order compared as a multiset by default, as an ordered sequence for
scenarios flagged `ordered`). Volatile values — timestamps, UUIDs, and the host
actor identity — are normalized to `<TS>`/`<UUID>`/`<ACTOR>`/`<EMAIL>` before
comparison. **Tolerated in-scope divergences: zero.**

## Run it

```sh
tests/oracle-a/run-oracle-a.sh
```

Exit status: `0` = 100% in-scope pass, `1` = at least one in-scope divergence,
`2` = setup/build error. On failure, each divergence is printed with the
reference vs candidate value for the offending step.

Overrides:

- `REF_REF=<gitref>` — compare against something other than `origin/main`
  (e.g. a release tag, or the branch's merge base).
- `KEEP_ARTIFACTS=1` — keep the scratch build dir (binaries, goldens, scoreboard
  output) for inspection instead of deleting it on exit.

## Prerequisites

- **Rust / `cargo`** — builds the vendored conformance harness (`harness/`).
- **A CGO toolchain (`gcc`/`cc`)** and **`go`** — `bd` embeds Dolt, which is cgo;
  both binaries build with `CGO_ENABLED=1 -tags gms_pure_go` (the `gms_pure_go`
  tag is mandatory per `docs/ICU-POLICY.md`).
- **`git`** with `origin/main` fetched (the script resolves `REF_REF` locally;
  run `git fetch` first if it is stale).

## Runtime

Dominated by the **cold reference build** from `origin/main` (a full `bd`
compile, ~1 min on a warm module cache, several minutes cold). The candidate
build reuses the local build cache and is fast. Harness build is ~10 s. Golden
capture + scoring run all 39 curated scenarios as real `bd` processes (~30–60 s).
End-to-end: **~2–7 minutes** depending on Go build-cache warmth. There is no
Dolt *server* in the loop — every scenario uses embedded Dolt in its own tempdir.

## What green PROVES

- For the **curated gc-contract scenarios** (the gc command surface plus their
  in-scope flags — see `harness/src/scenarios.rs`), the candidate `bd` produces
  byte-identical (post-normalization) exit codes, stderr, and JSON-structural
  stdout to the reference `bd`, step for step.
- This covers the semantics most likely to break under the pluggable-storage
  refactor: the close→ready `is_blocked` propagation, transitive/parent-child
  blocking, cycle rejection, claim lifecycle + idempotent self-reclaim, storage
  tiers (ephemeral/no-history) set algebra, **`delete`** and a **real (non-dry-run)
  `purge` with re-seed** (both must recompute surviving neighbours' `is_blocked`;
  purge has no uow dual and is where the bts-rs red team found re-seeding bugs),
  **`comment` add/list**, **`config set`/`get` success and reject paths**,
  metadata storage, **label-based query filtering** (`list --label` — now in scope),
  and the gc error contracts (`bd sql` embedded-unsupported, not-found,
  config-key rejection).

## What green does NOT prove

- **Only the in-scope surface.** The predicate is the gc-contract commands +
  flags in `harness/src/bin/scoreboard.rs` (`IN_SCOPE_CMDS` / `IN_SCOPE_FLAGS`,
  with `--json` required for gc-parsed output commands). Behavior outside that
  set — most of the ~271-command `bd` surface, human/plain output modes, and any
  flag not listed — is reported as out-of-scope and is **not** a gate. A refactor
  that changes only out-of-scope behavior passes Oracle A.
- **Ordering tiebreaks are UNGATED.** `ready`/`list` order among equal-priority
  issues is empirically **nondeterministic** here: every issue is created seconds
  apart, bd orders equal priorities by `created_at DESC` (with an `id ASC` final
  tiebreak), and `created_at` is both sub-second-precision-dependent AND normalized
  to `<TS>` — so the same 4 equal-priority creates land in a different order run to
  run (verified: three runs, three orders). Array output is therefore compared as a
  **multiset** by default; only scenarios with all-DISTINCT priorities are marked
  `ordered` and compared as a sequence. **What this leaves unpinned:** the `id ASC`
  and `created_at DESC` tiebreak chain, and the >48h hybrid-priority arm (structurally
  unreachable without an injectable clock — `PROPOSAL-pluggable-storage-backends.md`
  §4.7 item 6). A retype that breaks tiebreaks or the hybrid arm passes Oracle A.
- **Minted-ID paths are UNGATED.** Every create scenario passes an explicit `--id`.
  A `--id`-less create mints a hash-derived ID that hashes the wall-clock timestamp,
  so the reference and candidate mint *different* IDs for "the same" create and the
  ID is NOT normalized (it is not a UUID) — an `--id`-less scenario would flake. ID
  minting is a property-test concern, not a byte-diff one.
- **Anything invisible to stdout/stderr/exit diffing.** Post-commit hook firing,
  rollback-suppressed side effects, and N-Dolt-commits-instead-of-one are deliberately
  NOT asserted here. Timestamp *values* are erased by `<TS>` normalization, so the
  known local-vs-UTC defer/closed_at divergence class is also out of reach until
  clock injection lands. These are the epistemic limits in
  `PROPOSAL-pluggable-storage-backends.md` §2.4 / §7.
- **The scenario environment is scrubbed, and `BEADS_TEST_MODE=1` is ungated.**
  Scenarios run with an explicit env whitelist (`PATH`/`HOME`/`TMPDIR` +
  `BEADS_TEST_MODE=1`) rather than the inherited host env, so a green certifies the
  *same* configuration every run. But `BEADS_TEST_MODE=1` itself changes store
  construction (auto-server decisions off, port rewiring), so this rig does NOT
  cover the mode/topology construction paths (the H3/H10 surface a Phase-2a retype
  touches) — do not cite Oracle A green for those.
- **This is not a cross-backend oracle.** Oracle A is same-backend (embedded
  Dolt) before-vs-after. It is the refactor-safety net for the retype phases, not
  the Dolt-vs-SQLite/Postgres conformance run (that is Oracle B / Phase 1's
  backend-pair matrix).

## Lifespan — interim scaffolding, not a permanent harness

Oracle A is the **same-backend refactor-safety net for the retype phases**. It is
NOT the Phase-1 cross-backend conformance runner, and it is not meant to live
alongside one. When the Phase-1 Go runner (on `tests/regression` scaffolding, per
`PROPOSAL-pluggable-storage-backends.md`) lands, Oracle A's role is **folded into
or retired by it** — the repo must not carry a third parallel conformance harness
(the H1 "two seams" lesson applied to test infrastructure). Until then, re-sync the
vendored harness from upstream only as needed (see `harness/PROVENANCE.md`).

## How it stays out of bts-rs's way

The harness under `harness/` is a **vendored copy** of
`/data/projects/bts-rs/crates/bts-conformance`, byte-identical to upstream except
for the enumerated local deltas in `harness/PROVENANCE.md` (env scrub, in-scope
predicate, added coverage scenarios) plus the local `Cargo.toml`. Goldens are
re-captured from the reference build into `harness/testdata/golden/` (git-ignored)
on every run — bts-rs's own testdata is never read or written. The reference `bd`
is built in a throwaway `git worktree` that the script removes on exit.
