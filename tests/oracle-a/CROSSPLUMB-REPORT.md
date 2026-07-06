# Oracle X — cross-plumbing differential report

**flag-off embedded ↔ BD_SPIKE_UOWSTORE spike-proxied**, one binary, two
plumbings, over the gc-contract corpus. This is the Slice-3 headline: it proves
where the spike's proxied-uowstore path IS and IS NOT behaviorally equivalent to
the ordinary embedded path, with **every divergence attributed**.

Run: `tests/oracle-a/run-oracle-x.sh` · 2026-07-03 · binary built from the
`spike/backend-seam-derisk@426dd0626` working tree · 45 curated scenarios.

> **Binary-provenance caveat.** The candidate is built from the *working tree*
> at `426dd0626`, which carries an intentional uncommitted `go.mod` edit
> (`charmbracelet/x/ansi`). The exact binary is therefore **not** reproducible
> from a clean checkout of that commit; the counts below describe this
> working-tree build.

## What this oracle is (vs Oracle A)

Oracle A builds **two binaries** (before vs after) and runs them on the **same**
plumbing (embedded) — the retype refactor-safety net. Oracle X is the orthogonal
axis: **one** working-tree binary, run through **two** plumbings.

| role | plumbing | how |
|---|---|---|
| **REFERENCE** (goldens) | embedded, `BD_SPIKE_UOWSTORE` unset | the working-tree bd run natively — exactly what the harness does for real bd |
| **CANDIDATE** | proxied-server + uowstore adapter (`BD_SPIKE_UOWSTORE=1`) | a generated wrapper (`BTS_CANDIDATE`) that bootstraps a proxied workspace and execs the *same* bd with the proxied env |

The harness `run_scenario` `env_clear()`s to `{PATH,HOME,TMPDIR,BEADS_TEST_MODE=1}`,
so the wrapper is the only place the proxied env is injected. It intercepts the
harness `init -p <prefix> --quiet` bootstrap (writes `.beads/metadata.json` in
proxied-server mode, boots the managed proxy + child dolt sql-server via a first
read, per `cmd/bd/spike_uowstore_integration_test.go` `setupSpikeProxiedWorkspace`)
and passes every other argv through to `bd` untouched with
`BD_SPIKE_UOWSTORE=1` + the proxied env.

## Results

```
total scenarios: 45   (in-scope: 44, out-of-scope: 1)
IN-SCOPE   PASS: 41   FAIL: 3   (93%)
OUT-OF-SCOPE pass: 1  fail: 0    (note_append_two, informational)
```

**This is the Slice-4 equivalence-clean run.** Every class-(a) finding the
recorded Slice-3 run surfaced (F-1..F-5) is now fixed at source (see §Findings
and the remediation addendum), so **zero class-(a) divergences remain**. The
only 3 in-scope divergences left are the 3 allowlisted mode/unsupported/artifact
differences (`CROSSPLUMB-ALLOWLIST.md`, AX-1..AX-3):

- **41 in-scope scenarios are byte-identical** (post-normalization) across the
  two plumbings — the close→ready `is_blocked` loop, transitive/parent-child
  blocking, cycle *detection and message*, self-dependency guard, dep-retype
  refusal, claim lifecycle, tiers set algebra **including no_history**, metadata
  filtering, labels round-trip (incl. close-output hydration), infra-type
  auto-ephemeral for `message`, `started_at`/`closed_at` on status transitions,
  delete-unblocks-neighbour, real purge + reseed count (incl. `.events`), config
  custom-key set/get, and the omitempty output boundaries.
- **3 in-scope divergences**, every one **allowlisted** (class b/c/d —
  `CROSSPLUMB-ALLOWLIST.md`): AX-1 `sql_unsupported_embedded`,
  AX-2 `config_set_protected_keys`, AX-3 `comment_add_list`. **No class-(a)
  finding diverged this run.**

The attribution table below is retained as the **historical Slice-3 record** of
all 11 divergences the first recorded run found and how each was dispositioned;
rows 1–3, 5–8, 11 (the class-(a) findings + withdrawn AX-4) no longer diverge in
the Slice-4 run above.

### Attribution table (historical — Slice-3 recorded run, all 11)

| # | scenario | step | field | class | attribution |
|---|---|---|---|---|---|
| 1 | cycle_reject | 3 `dep add` (reverse) | stdout `.error` | **(a)** | adapter error-**wrapping**: "add dep: adding c-b → c-a would create a cycle" vs embedded "adding dependency would create a cycle". Cycle *is* rejected on both (exit 1); only the user-facing string differs. → F-1 |
| 2 | dep_retype | 3 `dep add` (retype) | stdout `.error` | **(a)** | adapter surfaces the raw repo error "add dep: insert: db: DependencySQLRepository.Insert: …" vs embedded's user-facing "…remove it first with 'bd dep remove'…". Retype *is* rejected on both. → F-1 |
| 3 | tiers_ephemeral | 5 `list --all` | stdout (set) | **(a)** | a `--no-history` issue (`t-h`) is **visible in `list --all` on proxied but hidden on embedded**. Tier-filtering divergence. → F-2 |
| 4 | sql_unsupported_embedded | 0 `sql` | stdout | (b) | raw-DB access unsupported on both; embedded → stderr text, proxied → typed-unsupported JSON on stdout. **AX-1** |
| 5 | ready_excludes_infra_and_coordination_types | 3 `create -t message` | stdout `.ephemeral` | **(a)** | `-t message` is auto-marked `ephemeral` on embedded but **not** on proxied — `IsInfraTypeCtx("message")` disagrees. → F-3 |
| 6 | list_excludes_gate_and_infra_types | 3 `create -t message` | stdout `.ephemeral` | **(a)** | same root cause as #5 (infra-type classification of `message`). → F-3 |
| 7 | output_parent_omitempty_boundary | 3 `update --status in_progress` | stdout `.started_at` | **(a)** | embedded sets `started_at` on the in_progress transition; proxied leaves it `null` — the adapter's `UpdateIssue` path skips `ManageStartedAt`. → F-4 |
| 8 | purge_dry_run_zero_metrics | 3 `close --force` | stdout `.labels` | **(a)** | embedded close output hydrates `labels:["red"]`; proxied returns `null` — the adapter's close return-object omits label hydration. → F-5 |
| 9 | config_set_protected_keys | 1 `config set dolt.debug` | stderr | (b)+(d) | `dolt.debug` is sql-server-only: embedded rejects at the gate, proxied passes the gate then fails on the spike workspace's missing `config.yaml`. Both exit 1. **AX-2** |
| 10 | comment_add_list | 1 `comment` | exit + stdout | (c) | `AddIssueComment` typed-unsupported; `comment` is outside the gc-16 set (not a census escape). **AX-3** |
| 11 | purge_real_then_reseed | 4 `purge --force` | stdout `.events` | (b) | audit-event materialization count (4 vs 0); `purged_count` identical (2). **AX-4** |

> The scoreboard records the **first** divergence per scenario. For #5/#6 the
> shared `IsInfraTypeCtx` root cause also perturbs the later `ready`/`list`/`count`
> steps of those scenarios; they are one finding (F-3), not three. **First-only
> also masked a second divergence inside `cycle_reject` (row 1): step 4's
> self-dependency case emits a semantically distinct error on the two plumbings —
> now folded into F-1.**
>
> **Slice-3 remediation:** rows **7** (F-4 `started_at`) and **11** (AX-4 purge
> `.events`) are FIXED — see the remediation addendum below — and the two delete
> data-loss blockers that the corpus *masked* (batch `delete --force` cascade,
> lost `!force` refusal) are fixed at the same seam. Rows 1–3, 5, 6, 8 (F-1, F-2,
> F-3, F-5) remain open. The headline PASS/FAIL counts above predate these fixes.

## Findings (class-(a), NOT allowlistable)

Real, uowstore-caused behavior differences. These are the deliverable of the
oracle — the cross-plumbing equivalence gaps in the spike adapter.

- **F-1 — error-message wording (cycle_reject, dep_retype).** *Severity: low
  (cosmetic).* The rejection semantics and exit codes match; the uow/dolt_sql
  provider surfaces a differently-worded (more internal) error string than the
  embedded store's user-facing message. gc code that string-matches these errors
  would break. **Three distinct wording divergences live in this family — the
  scoreboard records only the FIRST per scenario, so two were initially masked:**
  1. `cycle_reject` step 3 `dep add` (reverse edge): proxied "add dep: adding
     c-b → c-a would create a cycle" vs embedded "adding dependency would create
     a cycle".
  2. `cycle_reject` step 4 `dep add c-a c-a` (**SELF-dependency**): embedded
     emits its dedicated "cannot add self-dependency: c-a cannot depend on
     itself"; the adapter mis-routes it through the generic cycle check and emits
     "add dep: adding c-a -> c-a would create a cycle". Both exit 1. This step is
     a *semantically distinct* divergence (self-dep vs cycle), not just a
     re-wording — mapping only the two cycle strings leaves it failing.
  3. `dep_retype` step 3 `dep add` (retype): proxied "add dep: insert: db:
     DependencySQLRepository.Insert: …" vs embedded "…remove it first with 'bd
     dep remove'…".
  Fix: have the adapter reproduce embedded's self-dependency guard AND map the
  cycle/retype strings to the embedded user-facing messages.
- **F-2 — `no_history` tier visibility (tiers_ephemeral).** *Severity: medium.*
  `list --all` includes a `--no-history` issue on proxied but excludes it on
  embedded. A tier/data-visibility divergence — the adapter's list path does not
  reproduce embedded's no-history filtering.
- **F-3 — infra-type auto-ephemeral for `message`
  (ready_excludes_…, list_excludes_…).** *Severity: medium.* Embedded marks
  `-t message` creates `ephemeral`; the adapter's `applyInfraTypeRouting` →
  `IsInfraTypeCtx("message")` (`internal/storage/uowstore/store.go:495`) returns
  a different classification, so proxied leaves `message` non-ephemeral. This
  changes downstream ready/list/count filtering for the coordination types.
- **F-4 — `started_at` on `update --status in_progress`
  (output_parent_omitempty_boundary).** *Severity: medium.* **FIXED (Slice-3).**
  Embedded auto-sets `started_at` (GH#2796, `issueops/update.go:81`
  `ManageStartedAt`); the adapter's non-claim `UpdateIssue` path did not. The
  domain repo `Update` (`internal/storage/domain/db/issue.go`) now applies
  `issueops.ManageStartedAt` **and** `ManageClosedAt` on a status change, so
  `started_at`/`closed_at` track the transition on both plumbings. The same fix
  closes the broader (corpus-masked) sibling: `update --status closed` left
  `closed_at` NULL and reopening left stale `closed_at`/`close_reason`.
- **F-5 — label hydration in `close` output (purge_dry_run_zero_metrics).**
  *Severity: low/medium.* Labels *persist* (the round-trip test proves `bd show`
  returns them on both), but the adapter's `close` **return object** omits the
  `labels` array that embedded includes. An output-hydration gap on the close
  path, not a persistence bug.

## Commit-count observer (create → update → close, both plumbings)

Counts dolt commits per command. **Embedded:** `dolt log` against
`.beads/embeddeddolt/<db>` between commands (no server holds the lock).
**Proxied:** the child dolt sql-server stays up, so query it live —
`SELECT COUNT(*) FROM dolt_log` over the port in `.beads/proxieddb/proxy.pid`.

```
command   embedded(Δ)      spike-proxied(Δ)
create    9 (+1)           8 (+1)
update    10 (+1)          9 (+1)
close     11 (+1)          10 (+1)
base(init+schema): embedded=8  spike-proxied=7
```

**1 dolt commit per mutating command on BOTH plumbings** — no
"N-commits-instead-of-one" divergence (the exact class Oracle A's README lists as
out of its reach). The base differs by one init/bootstrap commit, but the
per-command deltas are identical 1:1:1.

## The wrapper (env parity notes)

The CANDIDATE wrapper mirrors `bdProxiedEnv()` + the spike flag, with three
**stated deviations** from `spike_uowstore_integration_test.go`, each required
for a clean cross-plumbing diff (not for the store):

1. **`BEADS_ACTOR="CI Bot"` + `GIT_AUTHOR_EMAIL="ci@beads.test"`** — the spike
   test forces `actor=spiketester` for a symmetric *two-spike* comparison. Here
   the REFERENCE is captured by the plain harness under the host git identity
   (`CI Bot`/`ci@beads.test`, which the harness normalizes to `<ACTOR>`/`<EMAIL>`),
   so the CANDIDATE must mint the **same** identity, not `spiketester`. Without
   this, `created_by`/`owner` diverge on every scenario.
2. **`git config --global beads.role maintainer`** (under `HOME=$WS`) — suppresses
   the GH#2950 role warning the reference never emits (it resolves the host
   global `beads.role`; `HOME=$WS` hides it). A wrapper artifact, killed at
   source so it can't pollute the stderr diff.
3. **`chmod 700 .beads`** — matches `bd init`'s perms (a plain `mkdir` warns at 0775).

The proxied side intentionally **does not** set `BEADS_TEST_MODE` (the reference
*does*, via the harness): the spike boot must construct a real managed server,
and `BEADS_TEST_MODE` flips store/topology construction off. This asymmetry is a
documented mode difference affecting store construction only, not field values.

Seeding note: the brief's `config set issue_prefix <prefix>` is **CLI-rejected**
as a protected key (`cmd/bd/config.go` `rejectProtectedConfigKey`) in **both**
plumbings, so it is a no-op. Seeding is unnecessary: every corpus create passes
an explicit `--id`, and `ValidateIDPrefixAllowed` short-circuits on an empty
`dbPrefix` — so the candidate mints the same ids as the reference regardless.

## Per-scenario child lifecycle

Each scenario boots a managed proxy + child dolt sql-server on its first command.
They self-terminate on a **30s idle timeout** (`defaultProxyIdleTimeout`,
`internal/storage/uow/dolt_sql_provider.go`), so concurrency stays bounded during
the run. The harness deletes each scenario tempdir on scenario end, but the
processes linger up to the idle window; `run-oracle-x.sh` therefore forces all
scenario workspaces under `$SCRATCH/scenwork` (via `TMPDIR`) and **reaps** any
proxy/child whose cmdline references that exact path after scoring and after the
observer. The path match is exact — this host runs many unrelated dolt servers,
so the reaper never `pkill`s a bare `dolt sql-server`.

## How to rerun

```sh
# builds one working-tree bd, generates the spike-proxied wrapper, captures
# embedded goldens, scores the proxied candidate, runs the commit observer.
tests/oracle-a/run-oracle-x.sh

# reuse a prebuilt bd (skip the ~1min build) and keep the scratch dir:
BD_BIN=/abs/path/to/bd KEEP_ARTIFACTS=1 tests/oracle-a/run-oracle-x.sh
```

Requirements: cargo, a CGO toolchain, go, `dolt` in PATH, `jq`, `mysql` (observer
only). 43+ scenarios each boot a proxy + child server; the full capture+score can
exceed 30 minutes. Exit 0 = completed; attribution (this report + the allowlist)
is the gate, not a bare FAIL count.

## Verdict

The cross-plumbing differential **completed clean** in the Slice-4 run. The
spike-proxied uowstore path is now byte-equivalent to embedded on **41/44
in-scope scenarios** and on the 1:1:1 commit shape, with the run exiting 0 under
its machine-checked attribution gate. **The only 3 remaining divergences are the
3 allowlisted** mode/unsupported/artifact differences (`CROSSPLUMB-ALLOWLIST.md`,
AX-1..AX-3); **zero class-(a) findings diverge** — every Slice-3 adapter gap
(infra-type classification, `started_at`/`closed_at` management, no-history list
filtering, close-time label hydration, cycle/self-dep/retype error wording) is
fixed at source. The Slice-3 findings and the attribution table are retained
below as the historical record of what the oracle surfaced and how it was closed.

## Slice-3 remediation addendum (post-run fixes)

A Slice-3 red-team pass surfaced findings the recorded run's counts did not
reflect (some corpus-masked). The following are now fixed at source; the numbers
above are **not** re-derived here — re-run `run-oracle-x.sh` for fresh counts.

**Data-loss blockers (corpus-masked — no failing scenario caught them):**

- **B-DEL-1 — batch `delete --force` cascaded durable dependents.** The store's
  `DeleteIssues` absorbed `cascade`/`force` into an unconditional cascade
  (`FindAllDependents`), so `bd delete a b --force` / `bd purge --force` silently
  deleted durable issues that merely *depended on* the deleted set. Fixed:
  `writes.go` now threads `cascade`/`force` into
  `deleteManyWithPolicy`, which refuses (force=false) or orphans (force=true)
  external dependents, mirroring `issueops.DeleteIssuesInTx`. Verified
  embedded↔spike: `delete a b --force` with external `ext` → both report
  `deleted_count=2, orphaned_issues=[ext]` and `ext` survives.
- **B-DEL-2 — `!force` batch preview lost the refusal.** `bd delete a b` (no
  `--force`) with an external dependent reported "Would delete: 0 issues" instead
  of embedded's "…has dependents not in deletion set…". Fixed by the same path
  (refusal returned pre-dry-run, and the dry-run `DeletedCount` is now computed).
  Verified: both plumbings emit the identical refusal string.
- **B-UPD-3 — `update --status` skipped the close/reopen lifecycle.** The domain
  repo `Update` never ran `ManageClosedAt`, so `update --status closed` left
  `closed_at` NULL and reopening left stale `closed_at`/`close_reason`. Fixed (see
  F-4) and verified embedded↔spike.

**Attribution corrections:**

- **AX-4 withdrawn / fixed.** The purge `.events` `4 vs 0` was a class-(a)
  counting bug (adapter counted `wisp_events` for directly-purged wisps; embedded
  counts them only for cascade-discovered wisps), not the "(b) audit-row
  materialization" story AX-4 claimed. `deleteManyWithPolicy` now matches
  embedded, so `purge_real_then_reseed` reports `events:0` on both plumbings.
  AX-4 removed from the allowlist.
- **F-1 completed.** The `cycle_reject` self-dependency step (step 4) is a
  distinct divergence the first-divergence-only scoreboard masked; it is now
  documented under F-1. Still open (error-wording family).
- **Event-type parity (new).** The domain repo `Update` recorded `EventUpdated`
  for every status change where embedded records
  `EventClosed`/`EventReopened`/`EventStatusChanged`; now uses
  `issueops.DetermineEventType`. Corpus-masked (events surface only as counts).

**Slice-4 status: all closed.** The Slice-4 run (§Results) shows F-1 (wording +
self-dep), F-2 (`no_history` list visibility), F-3 (infra-type auto-ephemeral for
`message`), and F-5 (close-output label hydration) **no longer diverge** — they
join F-4 as fixed at source. The cross-plumbing differential is now
equivalence-clean: only the 3 allowlisted (AX-1..AX-3) mode/unsupported/artifact
differences remain.
