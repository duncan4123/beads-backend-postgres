# Oracle A — wiring report and verification evidence

**Branch:** `spike/backend-seam-derisk`
**Date:** 2026-07-02
**Deliverable:** `tests/oracle-a/` — a refactor-safety differential conformance
rig for Go `bd`, reusing the bts-rs conformance harness against two Go binaries
(reference from `origin/main`, candidate from the working tree), zero tolerated
in-scope divergences.

## What was built

| file | purpose |
|---|---|
| `run-oracle-a.sh` | orchestrator: builds reference `bd` from `origin/main` in a throwaway `git worktree`, builds candidate from the working tree, builds the harness, captures goldens from the reference, scores the candidate, prints IN-SCOPE PASS/FAIL and sets exit status. |
| `harness/` | vendored verbatim copy of `/data/projects/bts-rs/crates/bts-conformance/src` (upstream commit `dffbcd9f4eb14457328dba50b6e196c39a441bcd`) with a standalone `Cargo.toml`. See `harness/PROVENANCE.md`. |
| `README.md` | prereqs, runtime, and the explicit what-green-does / does-not-prove boundary. |
| `REPORT.md` | this file. |

### Design decision: vendored harness copy, not an in-place bts-rs build

`capture_golden`/`scoreboard` hardcode the golden directory to
`CARGO_MANIFEST_DIR/testdata/golden`. The task requires re-capturing goldens
from a fresh `origin/main`-built `bd` **without overwriting bts-rs testdata or
committing inside `/data/projects/bts-rs`**. Building from a vendored copy makes
`CARGO_MANIFEST_DIR` resolve to `tests/oracle-a/harness`, so goldens land in
`tests/oracle-a/harness/testdata/golden/` (git-ignored, regenerated each run).
bts-rs is read once at vendor time and never written. The only file that differs
from upstream is `Cargo.toml` (workspace/path deps -> concrete versions; the
unused `bts-json` dep dropped), so the crate builds on its own. The Rust source
(`differential.rs`, `scenarios.rs`, `lib.rs`, `capture_golden.rs`,
`scoreboard.rs`) is byte-for-byte upstream.

The reference binary is built in a detached `git worktree add origin/main`, then
`git worktree remove`d on exit — so `origin/main` is never checked out over the
branch and the working tree is never disturbed.

## Verification evidence

> **Two corrections before reading the numbers below.**
> 1. **Reference = merge base, not `origin/main` tip.** The rig now defaults
>    `REF_REF` to `git merge-base HEAD origin/main` (env-overridable). The header
>    and README previously said "merge base" while the code used the tip; today
>    they coincide, but on a long-running effort `origin/main` advances and the tip
>    would fold upstream commits into the diff as false in-scope FAILs. The verdict
>    line prints both the resolved ref and its SHA so each green is attributable.
> 2. **The captured evidence below is stale.** It was recorded when branch HEAD
>    equalled `origin/main`, i.e. BEFORE the spike/oracle commits existed, and
>    predates the scope fix (`--label`) and the added coverage scenarios, so the
>    `39 scenarios` / `38 PASS` headline no longer matches the current rig. It also
>    does NOT exercise the spike store: `BD_SPIKE_UOWSTORE` is unset during runs, so
>    green says only that the flag-OFF wiring is inert — it says nothing about the
>    uowStore adapter. Re-run `run-oracle-a.sh` at the branch tip and paste that
>    output (with the printed REF/CAND SHAs) for a current, attributable gate record.

### (a) main-vs-main leak test (historical evidence, pre-dates the review fixes)

At the time this was captured, the branch HEAD equalled `origin/main` (`7d8063d1d`),
so a full `run-oracle-a.sh` builds the reference and candidate from the same source
as **two independently-compiled binaries** and diffs them. This is the leak test:
any divergence would be a harness/normalization bug, not a code change.

Exact command and real output:

```
$ tests/oracle-a/run-oracle-a.sh
[oracle-a] reference ref : origin/main (7d8063d1d42cddaff179be0efe69f64dae081651)
[oracle-a] candidate     : working tree (HEAD 7d8063d1d42cddaff179be0efe69f64dae081651)
[oracle-a] reference bd : .../bd-reference (bd version 1.1.0-rc.2 (dev))
[oracle-a] candidate bd : .../bd-candidate (bd version 1.1.0-rc.2 (dev: spike/backend-seam-derisk@f41ece47eab3))
capturing 39 scenarios...
   ... 39 scenarios captured, exits as expected (e.g. cycle_reject=[0,0,0,1,1]) ...

=== bts-rs scoreboard ===
total scenarios: 39  (no-golden: 0)

IN-SCOPE (gc contract) — the pass target:
  PASS: 38   FAIL: 0   (100%)

OUT-OF-SCOPE (bd-only features bts-rs intentionally omits):
  pass: 1   fail: 0  (informational; not a target)

[oracle-a] RESULT: IN-SCOPE PASS (38 scenarios, 0 divergences) — refactor is behavior-preserving on the gc-contract surface.
$ echo $?
0
```

Wall time end-to-end: **6 min 30 s** (cold reference build dominated).
**No harness or normalization gap surfaced** — the host git identity is
`CI Bot`/`ci@beads.test`, exactly the tokens `normalize()` collapses, so identity
and timestamps normalize cleanly and the two separately-built binaries agree
byte-for-byte on every in-scope step.

> **Correction (adversarial review).** The original report attributed the single
> out-of-scope scenario to `config_set_protected_keys` "invoked without `--json`".
> That was wrong twice: those steps DO pass `--json`, and `config` is not in
> `JSON_OUTPUT_CMDS` anyway, so `config_set_protected_keys` is IN scope. The
> genuinely out-of-scope scenario used to be `labels`, whose `list --all --label …`
> step used `--label`, a flag absent from `IN_SCOPE_FLAGS` — so a regression in
> label filtering silently did not gate. That is now fixed: `--label` was added to
> `IN_SCOPE_FLAGS`, so `labels` is in scope, and label filtering is gated. Re-run
> the rig for the current pass/fail headline; do not trust the stale `38/0` numbers
> below, which pre-date the scope fix and the added coverage scenarios.

An even tighter sanity was run during bring-up: scoring the reference binary
against *its own* goldens (same binary both sides) also produced 38/0/100% — 0
divergences — confirming the volatile normalizer leaves no residue.

### (b) injected-diff proof — the rig FLAGS a real behavior change

A user-visible error string was changed in a **scratch** candidate built from
`git archive HEAD` (the real working tree was never touched):

`cmd/bd/sql.go:45`
`'bd sql' is not yet supported in embedded mode`
-> `'bd sql' is INJECTED-DIFF unsupported here`

Scoring that injected candidate against the unchanged reference goldens:

```
=== bts-rs scoreboard ===
total scenarios: 39  (no-golden: 0)

IN-SCOPE (gc contract) — the pass target:
  PASS: 37   FAIL: 1   (97%)

in-scope failures by command:
  sql          1

--- divergence detail (/tmp/bts-failures.txt) ---
sql_unsupported_embedded — step 0 `sql SELECT 1 --json` stderr mismatch:
   reference: Error: 'bd sql' is not yet supported in embedded mode
   candidate: Error: 'bd sql' is INJECTED-DIFF unsupported here
```

In-scope pass dropped 38 -> 37, the `sql` command was flagged, and the divergence
detail names the exact step and shows reference-vs-candidate stderr. The patch
was then discarded (scratch dir removed); `grep -c INJECTED-DIFF cmd/bd/sql.go`
in the working tree returns `0`. **The rig detects real user-visible changes.**

### (c) re-runnable from clean

`run-oracle-a.sh` is idempotent: it uses a unique per-run scratch dir
(`oracle-a-<timestamp>-<pid>`), a unique binary path per binary (never `cp`s over
an exec-mapped file), removes the reference `git worktree`, and deletes
`harness/testdata/golden` before re-capturing so goldens are always fresh from the
current reference.

**go.mod/go.sum safety (adversarial-review fix).** The rig no longer runs
`git checkout -- go.mod go.sum`, which restored to HEAD and so would have DESTROYED
a pre-existing uncommitted edit (the working tree may legitimately carry one — e.g.
a dep bump that is itself the candidate under test; the exact Phase-4 use case adds
a Postgres driver dependency). It now snapshots the two files' bytes BEFORE any
build and restores the pre-run snapshot only when the build actually changed them
(content compare) — restore-to-pre-run, never restore-to-HEAD. `INT`/`TERM` are
also trapped so a Ctrl-C during the multi-minute reference build still runs
cleanup (no leaked `/tmp` worktree registration).

## Findings / friction discovered (feeds the design)

1. **The gc-contract actor/time normalization is sufficient for Oracle A on
   this host, by luck of identity.** bd derives its actor from
   `--actor > BEADS_ACTOR > BD_ACTOR > git config user.name > $USER`
   (`cmd/bd/main.go:464`) and owner from `git config user.email`. This host's git
   identity is exactly `CI Bot` / `ci@beads.test` — the literal tokens
   `differential.rs::normalize()` collapses. So same-host reference-vs-candidate
   traces normalize to identical bytes **without any actor/time env injection**.
   This is a same-host convenience, not a general guarantee: on a host with a
   different git identity, the harness would still be internally consistent
   (both binaries derive the same actor, and stderr/stdout still match), because
   normalization keys on whatever bd emits *for both sides equally* — the
   `ACTOR_NAME`/`ACTOR_EMAIL` constants only matter for cross-host golden
   portability, which Oracle A does not rely on (goldens are captured fresh each
   run). **No bd change is needed for Oracle A.** (This is distinct from the
   Phase-1 cross-backend need for injectable actor/clock for byte-stable traces,
   which remains a known work item — see below.)

2. **The scoreboard never signals failure via exit code** — it always exits 0
   and prints a scoreboard, dumping in-scope failures to `/tmp/bts-failures.txt`.
   That is fine for the bts-rs backlog workflow but useless as a CI gate. The
   wrapper (`run-oracle-a.sh`) parses the IN-SCOPE `PASS:/FAIL:` line and sets a
   real exit code (0/1/2). Any future promotion of this rig into CI should keep
   the gate in the wrapper, not expect it from the harness bin.

3. **`catalog()` is inert in the vendored copy, intentionally.** The catalog
   loader reads `../../docs/scenarios/enumerated.json` (500+ enumerated scenarios)
   relative to `CARGO_MANIFEST_DIR`; that path does not exist here, so it returns
   empty and Oracle A runs only the curated `all()` set (39 scenarios) — the
   maintained gc-contract surface. If broader coverage is wanted later, vendor
   the enumerated JSON alongside and set `BTS_CATALOG=1` in capture; the harness
   already supports it. Left out deliberately to keep the rig's scope crisp and
   its runtime bounded.

4. **`purge`, `delete`, `comment`, and `config` success paths were coverage gaps —
   now closed.** The original rig exercised `purge` only as `--dry-run` (which never
   mutates), had ZERO `delete`/`comment` steps, and pinned `config set` only on its
   reject path. Since `delete`/`purge` are §2.3's top-danger ops (must recompute
   surviving neighbours' `is_blocked`), that was a real hole. Added scenarios:
   `delete_unblocks_neighbour` (delete a blocker, assert the neighbour becomes
   ready), `comment_add_list` (add two comments, list them), `config_set_get_success`
   (custom-key round trip + missing-key read), and `purge_real_then_reseed` (a real
   `purge --force` with `purged_count` and a post-purge create+list re-seed
   assertion). And to correct the earlier note: `config set` IS in scope (`config`
   is not a JSON-output command, so `--json` is not required for its scope), so the
   reject-path scenario always gated — the earlier "out-of-scope" claim was wrong.

## Known gaps documented, not patched (per task instruction)

- **Byte-stable-trace actor/clock injection into bd** is *not* required by
  Oracle A (finding #1) and was not added. The general Phase-1 need — an
  injectable `Clock`/`Identity` so cross-backend traces are byte-stable
  independent of host git config and wall-clock — remains a known Phase-1 work
  item (proposal §4.7 item 6: "clock ownership moves client-side"). Oracle A
  sidesteps it by capturing goldens fresh from the reference on the same host
  each run, so no persisted, host-portable golden depends on a fixed identity.
- **Coverage is the curated 39, not the CORE-67 or the full enumerated catalog.**
  Widening is a config change (finding #3), not a rig change; the proposal's
  Phase-1 gate calls for a checked-in coverage artifact before a backend is
  announced — that artifact is out of scope for Oracle A (which is the
  same-backend refactor net, not the cross-backend conformance run).
