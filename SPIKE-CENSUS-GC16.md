# SPIKE-CENSUS-GC16 — gc-16 command → `storage.DoltStorage` call-graph census (Slice 3)

**Branch:** `spike/backend-seam-derisk`
**Input for:** SPIKE-REPORT §7's prerequisite census (the accurate method count that the
`storage.DoltStorage` interface census under-/over-counts). Feeds the adapter-completion
work in `PROPOSAL-uowstore-adapter.md`.
**Baseline traced:** the EMBEDDED command path (default, `BD_SPIKE_UOWSTORE` unset) — i.e.
what each command's handler + PreRun/PostRun surfaces + shared helpers actually call on
`storage.DoltStorage`. Coverage is then scored against the spike adapter
(`internal/storage/uowstore/store.go` + `tx.go`) vs the generated typed-`ErrUnsupported`
shell (`unsupported_gen.go`).

## Counting rules (stated explicitly)

- **DIRECT** = the method is called by the command's own `RunE` (or a function file-local to
  that command, e.g. `cmd/bd/close.go`'s `autoCloseCompletedMolecule`).
- **TRANSITIVE** = the method is reached only through a *shared* surface that fires for the
  command but is not that command's handler:
  - **PreRun** (`cmd/bd/main.go` `PersistentPreRunE`): `validateWorkspaceIdentity → GetMetadata`
    (write commands only — `!isReadOnlyCommand`), `maybeAutoImportJSONL → GetStatistics`
    (`shouldRunAutoImportJSONL`, any store-opening command with a non-empty `issues.jsonl`),
    and the **molecules loader** (`molecules.NewLoader(store).LoadAll`, every command except
    `import`) → `GetIssue` + `CreateIssuesWithFullOptions`.
  - **Routing preflight** (`cmd/bd/routing_read.go`): `GetAllConfig` + `GetConfig` on read
    commands (`openRoutedReadStore`) and mutation resolvers (`resolveViaAutoRouting`).
  - **Resolver** (`cmd/bd/routed.go` / `dep.go`): id → `GetIssue` for every command that takes
    an id argument.
  - **PostRun** (`cmd/bd/main.go` `PersistentPostRunE`): tip metadata → `SetLocalMetadata`,
    `Commit`. **NOTE:** on the spike path PostRun's write arm is *bypassed* (it sits in the
    `else` of `if proxiedServerMode`, and the raw `proxiedServerMode` global stays true under
    the flag — SPIKE-REPORT §4). So PostRun-only methods are latent on the spike path even
    though the embedded path reaches them; flagged per row.
- **Covered** = overridden in `store.go`/`tx.go` (real UOW body). **Missing** = falls through
  to `unsupportedDoltStorage`/`unsupportedTransaction` → returns `*storage.ErrUnsupported`
  (or, for the two no-error-channel methods, a zero value). `unsupported_gen.go` stubs all
  **144** DoltStorage + **24** Transaction methods; the adapter really overrides **24**
  DoltStorage methods (+ `Close`, `RunInTransaction`) and **3** Transaction methods
  (`GetIssue`, `CloseIssue`, `AddDependency`), leaving **118** DoltStorage methods missing.
- **Corpus** = the command+flags appears in the 43-scenario oracle rig
  (`tests/oracle-a/harness/src/scenarios.rs` `all()`). "core five" methods verified working
  end-to-end by SPIKE-REPORT's `TestSpikeUOWStore_RoundTrip`.

## Coverage baseline — the 24+3 the adapter overrides

`CreateIssue`, `CloseIssue`, `GetIssue`, `GetIssuesByIDs`, `SearchIssues`,
`SearchIssuesWithCounts`, `GetReadyWork`, `GetReadyWorkWithCounts`, `GetLabels`,
`GetDependenciesWithMetadata`, `GetDependentsWithMetadata`, `CountDependencies`,
`CountDependents`, `CountIssueComments`, `IsBlocked`, `GetDependencyRecordsForIssues`,
`GetCustomStatusesDetailed`, `GetCustomTypes`, `GetInfraTypes`, `IsInfraTypeCtx`, `GetConfig`,
`GetAllConfig`, `GetMetadata`, `GetStatistics` (+ `Close`, `RunInTransaction`); Transaction
view: `GetIssue`, `CloseIssue`, `AddDependency`.

---

## Per-command tables

Legend: **C** = Covered, **M** = Missing (→ ErrUnsupported), **D** = Direct, **T** = Transitive.
"Corpus" ✓ = exercised by a scenario; ✗ = not in the rig; (flag) = only under a non-corpus flag.

### Shared surface reached by (almost) every gc-16 command

| Method | D/T | Cov | Corpus | Notes |
|---|---|---|---|---|
| `GetIssue` | T (resolver/molecules) | C | ✓ | id resolution + molecules `LoadAll` probe |
| `GetAllConfig`, `GetConfig` | T (routing_read) | C | ✓ | repo auto-routing preflight, every read + mutation-resolve |
| `GetMetadata` | T (PreRun identity) | C | ✓ | write commands only (`validateWorkspaceIdentity`) |
| `GetStatistics` | T (PreRun auto-import) | C | ✓ | any command w/ non-empty `issues.jsonl` |
| `CreateIssuesWithFullOptions` | T (molecules loader) | **M** | ✗ (latent) | fires only if a `molecules.jsonl` carries templates absent from the DB; every non-`import` command. **Highest command-fan-out missing method.** |
| `SetLocalMetadata`, `Commit` | T (PostRun tip) | **M** | ✗ | bypassed on spike path (PostRun write arm gated by `proxiedServerMode`); latent |

### close (corpus ✓)

| Method | D/T | Cov | Corpus | Notes |
|---|---|---|---|---|
| `IsBlocked` | D | C | ✓ | pre-close blocker check; only `!--force` |
| `CloseIssue` | D | C | ✓ | **error-mapping gap** — leaks `db: …Close` + wrapped `sql.ErrNoRows`, not `storage.ErrNotFound` (§6.1) |
| `GetIssue` | D | C | ✓ | resolve + re-fetch + `autoCloseCompletedMolecule` |
| `GetDependentsWithMetadata` | D | C | ✓ (cpb/ccs) | `countEpicOpenChildren`, `!--force` epic path |
| `GetDependencyRecordsForIssues`, `GetIssuesByIDs` | D | C | ✓ | unconditional parent-molecule auto-close probe (`findParentMolecules`) |
| `GetNewlyUnblockedByClose` | D | **M** | (flag `--suggest-next`) | |
| `GetReadyWork` | D | C | (flag `--claim-next`) | |
| `ClaimIssue` | D | **M** | (flag `--claim-next`) | |

**Verdict:** core close (`--force`, `-r`) is fully COVERED; the corpus close scenarios pass on
the spike path. Only non-corpus post-close flags miss. Watch the `CloseIssue` not-found bytes.

### config (corpus ✓ — load-bearing)

| Method | D/T | Cov | Corpus | Notes |
|---|---|---|---|---|
| `GetConfig` | D | C | ✓ (`config get`) | `cfs` get success/absent |
| `GetAllConfig` | D | C | ✗ | `config list` / `config show` |
| `SetConfig` | D | **M** | ✓ (`config set`) | **`cfs` config_set_get_success writes `custom.team` → ErrUnsupported on the spike path.** The `cfg` protected-keys scenario (`issue-prefix`,`dolt.debug`) is *rejected before* the store write (exit 1), so it does NOT reach `SetConfig` — only the *success* path does. This is the §Special (1) load-bearing gap: the cross-plumbing wrapper's `bd config set issue_prefix` cannot round-trip until `SetConfig` is implemented. |
| `DeleteConfig` | D | **M** | ✗ | `config unset` |

### create (corpus ✓)

| Method | D/T | Cov | Corpus | Notes |
|---|---|---|---|---|
| `CreateIssue` | D | C | ✓ | infra-type routing + label copy fixups already in adapter (§3) |
| `GetConfig` | D | C | (non-`--id`) | `issue_prefix`/`allowed_prefixes` validation; skipped w/ explicit `--id --force` |
| `GetLabels` | D | C | (flag `--parent`) | inherited-label lookup |
| `GetNextChildID` | D | **M** | (auto child id) | only when a parent is given without explicit `--id`; corpus always passes `--id` (incl. dotted `ldo-1.1`) so not hit |
| `AddDependency` | D | **M** | ✗ | inline `--depends-on/--blocks/--parent`; corpus uses separate `dep add` |
| `SetConfig` | D | **M** | ✗ | clone/dry-run prefix seeding (`openDryRunTargetStore`) |
| `Commit`, `Push` | D | **M** | ✗ | federation/explicit-commit paths |

**Verdict:** corpus `create --id … --json` is COVERED (SPIKE round-trip). Inline-dep and
auto-child-id creates need `AddDependency`/`GetNextChildID`.

### delete (corpus ✓ — `del`)

| Method | D/T | Cov | Corpus | Notes |
|---|---|---|---|---|
| `GetDependencies` | D | **M** | ✓ | pre-delete neighbour analysis |
| `GetDependents` | D | **M** | ✓ | pre-delete neighbour analysis |
| `GetDependencyRecords` | D | **M** | ✓ | pre-delete edge enumeration |
| `RunInTransaction` | D | C | ✓ | delete runs its mutations in one tx… |
| `Transaction.UpdateIssue` | D(tx) | **M** | ✓ | …but the tx view stubs it (text-ref rewrite) |
| `Transaction.RemoveDependency` | D(tx) | **M** | ✓ | tx view stub (edge teardown) |
| `Transaction.DeleteIssue` | D(tx) | **M** | ✓ | tx view stub (the delete itself) |
| `DeleteIssue` | D | **M** | ✓ | single-delete non-tx path |
| `DeleteIssues` | D | **M** | ✗ | batch/cascade path |
| `UpdateIssue` | D | **M** | ✗ | `updateTextReferencesInIssues` |

**Verdict:** delete is fully MISSING for corpus — both the non-tx `DeleteIssue` and the
`RunInTransaction` body (all three tx methods are stubs). §2.3 names delete the top-danger
op (surviving-neighbour `is_blocked` recompute); the `del` scenario asserts exactly that.

### dep (corpus ✓ — many)

| Method | D/T | Cov | Corpus | Notes |
|---|---|---|---|---|
| `AddDependency` | D | **M** | ✓ | **`dep add` — ~15 scenarios (graph_close_ready, cycle_reject, dep_retype, transitive_block, parent_child_block, …).** Direct `fromStore.AddDependency`, NOT via `RunInTransaction`, so the tx-view override does not help. Highest-frequency corpus gap. |
| `RemoveDependency` | D | **M** | ✓ (`drm`) | `dep remove` |
| `DetectCycles` | D | **M** | ✗ | `dep cycles`; the in-corpus cycle rejection is enforced inside `AddDependency`'s use-case, not this method |
| `GetDependencyTree` | D | **M** | ✗ | `dep tree` |
| `GetDependenciesWithMetadata`, `GetDependentsWithMetadata`, `GetDependencyRecordsForIssues` | D | C | ✗ | `dep list/show` read side |

### init (corpus ✗ — bootstrap, special)

| Method | D/T | Cov | Corpus | Notes |
|---|---|---|---|---|
| `SetConfig`, `SetMetadata`, `SetLocalMetadata`, `AddRemote`, `HasRemote`, `Commit` | D | **M** | ✗ | workspace bootstrap |
| `GetMetadata`, `GetConfig`, `GetStatistics` | D | C | ✗ | identity/pre-checks |

**Verdict:** `init` constructs the store/workspace out-of-band and is not a candidate to run
*through* `uowStore` (the provider must already exist). Treated as out-of-scope for the
adapter; listed for completeness. Its writes are the densest missing cluster but latent.

### list (corpus ✓ — list/ordering/labels/metadata/parent)

| Method | D/T | Cov | Corpus | Notes |
|---|---|---|---|---|
| `SearchIssuesWithCounts` | D | C | ✓ | default `list --json` path |
| `GetReadyWorkWithCounts` | D | C | (flag `--ready`) | |
| `SearchIssues`, `GetReadyWork`, `GetIssue` | D | C | (non-json/pretty) | |
| `GetAllDependencyRecords` | D | **M** | ✗ | pretty/tree + `--format` only |
| `GetBlockingInfoForIssues` | D | **M** | ✗ | agent/long text only |

**Verdict:** every corpus `list … --json` (incl. `--all`, `--label`, `--metadata-field`,
`--parent`) resolves through `SearchIssuesWithCounts` → COVERED. Missing methods are
text-render-only, never reached with `--json`.

### query (corpus ✓)

| Method | D/T | Cov | Corpus | Notes |
|---|---|---|---|---|
| `SearchIssues`, `SearchIssuesWithCounts` | D | C | ✓ | all four query scenarios (`ephemeral=`, `label=`, `parent=`, `--limit 0`) COVERED |

**Verdict:** fully COVERED. No gap.

### ready (corpus ✓)

| Method | D/T | Cov | Corpus | Notes |
|---|---|---|---|---|
| `GetReadyWorkWithCounts` | D | C | ✓ | default `ready --json` (incl. `--include-ephemeral`) |
| `GetReadyWork`, `GetStatistics` | D | C | (non-json) | |
| `GetIssue` | D | C | (non-json parent map) | |
| `ClaimReadyIssue` | D | **M** | ✓ (`rcp`) | **`ready --claim` — ready_claim_skips_preassigned.** |
| `GetDependencyCounts` | D | **M** | ✓ (`rcp`) | `buildReadyIssueOutput` after a `--claim` |
| `GetCommentCounts` | D | **M** | ✓ (`rcp`) | `buildReadyIssueOutput` after a `--claim` |
| `GetDependencyRecordsForIssues` | D | C | (`rcp`) | same enrichment |
| `GetBlockedIssues`, `DetectCycles` | D | **M** | ✗ | verbose/consistency paths |

**Verdict:** plain/ephemeral `ready --json` COVERED; `ready --claim` is a corpus gap
(`ClaimReadyIssue` + the two enrichment counts).

### reopen (corpus ✓ — `upo`)

| Method | D/T | Cov | Corpus | Notes |
|---|---|---|---|---|
| `GetIssue` | D | C | ✓ | resolve + re-fetch |
| `ReopenIssue` | D | **M** | ✓ | `reopen upo-2 --json` → ErrUnsupported |

### show (corpus ✓ — many)

| Method | D/T | Cov | Corpus | Notes |
|---|---|---|---|---|
| `GetIssue` | D | C | ✓ | resolve |
| `GetLabels` | D | C | ✓ | default `--json` fan-out |
| `GetDependenciesWithMetadata`, `GetDependentsWithMetadata` | D | C | ✓ | |
| `CountDependencies`, `CountDependents`, `CountIssueComments` | D | C | ✓ | |
| `GetIssueComments`, `IterIssueComments` | D | **M** | ✗ | comment *bodies*; **best-effort — error is discarded (`comments, _ :=`)**, so show does not hard-fail, it silently omits comments. Latent divergence for an issue that has comments (corpus reads comments via `comments`, not `show`). |
| `SearchIssues` | D | C | ✗ | related/children sub-render |

**Verdict:** every corpus `show … --json` is COVERED. Note the silent comment-omission (§6.1
class): the missing `GetIssueComments` is swallowed, not surfaced.

### update (corpus ✓ — many, load-bearing)

| Method | D/T | Cov | Corpus | Notes |
|---|---|---|---|---|
| `GetIssue` | D | C | ✓ | resolve + re-fetch |
| `UpdateIssue` | D | **M** | ✓ | **status / set-metadata / assignee / defer — claim_lifecycle, error_notfound, df, umk, umc, cpb, upo.** |
| `ClaimIssue` | D | **M** | ✓ | `update --claim` (claim_lifecycle `k`, `cp`) |
| `GetCustomStatuses` | D | **M** | (`--status` custom) | note: this is the **non-`Detailed`** variant; `GetCustomStatusesDetailed` is Covered but update calls `GetCustomStatuses` → still Missing |
| `GetDependencyRecords`, `RemoveDependency`, `AddDependency` | D | **M** | ✗ | `--parent` reparent |

**Verdict:** update is a major corpus gap — `UpdateIssue` and `ClaimIssue` gate 8 scenarios.

### purge (corpus ✓ — `pdr` dry-run, `prg` real; §Special 2)

| Method | D/T | Cov | Corpus | Notes |
|---|---|---|---|---|
| `SearchIssues` | D | C | ✓ | closed-ephemeral candidate scan |
| `DeleteIssues` | D | **M** | ✓ | **BOTH paths hit it:** `purge --dry-run` calls `DeleteIssues(…, dryRun=true)` (purge.go:176) and `purge --force` calls `DeleteIssues(…, force=true)` (purge.go:225). So even the dry-run scenario `pdr` reaches the missing method; `prg` (real purge + reseed) needs the mutating path AND the §2.3 neighbour re-seed the use-case would own. No uow dual exists for purge — this is a true gap unit. |

### count (corpus ✓ — `t`, `lx`)

| Method | D/T | Cov | Corpus | Notes |
|---|---|---|---|---|
| `CountIssues` | D | **M** | ✓ | `count --json` (tiers_ephemeral, list_excludes_gate_and_infra_types) |
| `CountIssuesByGroup` | D | **M** | ✗ | `--group-by` |

### version (corpus ✗)

No `storage.DoltStorage` calls; prints build info. Trivially "covered" (nothing to implement).

### stats (corpus ✗)

| Method | D/T | Cov | Corpus | Notes |
|---|---|---|---|---|
| `SearchIssues` | D | C | ✗ | aggregates over a full scan |
| `GetLabelsForIssues` | D | **M** | ✗ | label breakdown render |

---

## MISSING-methods rollup — ordered by how many gc-16 commands need them

Rank by command fan-out (a command counts if the method is reachable on any realistic path;
corpus-blocking marked). Transaction-view methods tagged `(tx)`.

| Rank | Method | # gc cmds | Commands | Corpus-blocking |
|---|---|---|---|---|
| 1 | `CreateIssuesWithFullOptions` | ~14 | all except version/init (molecules loader) | ✗ latent (only if `molecules.jsonl` has new templates) |
| 2 | `AddDependency` | 3 | dep (add), create (inline), update (reparent) | ✓ **dep add — ~15 scenarios** |
| 3 | `RemoveDependency` | 3 | dep (remove), delete, update | ✓ dep remove (`drm`), delete |
| 4 | `SetConfig` | 3 | config (set), create (clone-seed), init | ✓ **config set (`cfs`)** |
| 5 | `UpdateIssue` | 2 (+tx) | update, delete (text-ref + tx) | ✓ **update — 8 scenarios** |
| 6 | `DeleteIssues` | 2 | purge, delete (batch) | ✓ **purge (`pdr`,`prg`)** |
| 7 | `GetDependencyRecords` | 2 | delete, update | ✓ delete pre-analysis |
| 8 | `ClaimIssue` | 2 | update (--claim), close (--claim-next) | ✓ **update --claim (`k`,`cp`)** |
| 9 | `DetectCycles` | 2 | dep, ready | ✗ |
| 10 | `DeleteIssue` | 1 | delete | ✓ delete (`del`) |
| 11 | `Transaction.DeleteIssue` (tx) | 1 | delete | ✓ delete tx body |
| 12 | `Transaction.UpdateIssue` (tx) | 1 | delete | ✓ delete tx body |
| 13 | `Transaction.RemoveDependency` (tx) | 1 | delete | ✓ delete tx body |
| 14 | `ReopenIssue` | 1 | reopen | ✓ reopen (`upo`) |
| 15 | `CountIssues` | 1 | count | ✓ count (`t`,`lx`) |
| 16 | `ClaimReadyIssue` | 1 | ready | ✓ ready --claim (`rcp`) |
| 17 | `GetDependencyCounts` | 1 | ready | ✓ ready --claim enrich (`rcp`) |
| 18 | `GetCommentCounts` | 1 | ready | ✓ ready --claim enrich (`rcp`) |
| 19 | `GetDependencies` | 1 | delete | ✓ delete pre-analysis |
| 20 | `GetDependents` | 1 | delete | ✓ delete pre-analysis |
| 21 | `GetCustomStatuses` | 1 | update | ✗ (only `--status` custom validation) |
| 22 | `GetNextChildID` | 1 | create | ✗ (auto child id) |
| 23 | `CountIssuesByGroup` | 1 | count | ✗ (`--group-by`) |
| 24 | `GetNewlyUnblockedByClose` | 1 | close | ✗ (`--suggest-next`) |
| 25 | `GetBlockedIssues` | 1 | ready | ✗ |
| 26 | `GetDependencyTree` | 1 | dep | ✗ (`dep tree`) |
| 27 | `GetAllDependencyRecords` | 1 | list | ✗ (pretty/format only) |
| 28 | `GetBlockingInfoForIssues` | 1 | list | ✗ (agent/long text only) |
| 29 | `GetIssueComments`, `IterIssueComments` | 1 | show | ✗ (best-effort, error swallowed) |
| 30 | `DeleteConfig` | 1 | config | ✗ (`config unset`) |
| 31 | `GetLabelsForIssues` | 1 | stats | ✗ |
| — | init writes (`SetMetadata`,`SetLocalMetadata`,`AddRemote`,`HasRemote`,`Commit`) | 1 | init (bootstrap) | ✗ out-of-scope |

**Corpus-blocking missing set (the adapter-completion must-do list to make the 43-scenario rig
green through the spike store):** `AddDependency`, `RemoveDependency`, `SetConfig`,
`UpdateIssue`, `DeleteIssues`, `DeleteIssue`, `ReopenIssue`, `ClaimIssue`, `ClaimReadyIssue`,
`CountIssues`, `GetDependencyCounts`, `GetCommentCounts`, `GetDependencies`, `GetDependents`,
`GetDependencyRecords`, and the three Transaction-view stubs (`DeleteIssue`, `UpdateIssue`,
`RemoveDependency`) used by delete's `RunInTransaction`.

**Fully-covered corpus commands today:** `query`, `show`, `list`, `create`, `close`, `ready`
(all `--json`, minus `ready --claim`). These are the SPIKE round-trip substrate.

---

## Error-mapping gap list (SPIKE-REPORT §6.1 — covered methods that leak raw text)

There is **no `mapUowError` helper anywhere in the package** (verified: grep returns 0). Only
`GetIssue`/`Transaction.GetIssue` translate — via the shared `getIssueInUOW` +
`isNotFound` helpers, which map the uow path's wrapped `sql.ErrNoRows` to
`storage.ErrNotFound` and add the wisp fallback. Every other covered method returns the
use-case's raw error verbatim, so `errors.Is(err, storage.ErrNotFound)` is FALSE and the bytes
carry the `db: …Repository.…` prefix the store contract never emits.

| Covered method | Leak | Reached by | Impact |
|---|---|---|---|
| `CloseIssue` | `db: IssueSQLRepository.Close …` + wrapped `sql.ErrNoRows`, not `storage.ErrNotFound` | close, delete's tx | Shielded on the happy path because close resolves via the translated `GetIssue` first; a direct `close <missing>` (or any consumer doing `errors.Is(…, ErrNotFound)`) diverges. `error_notfound` (`e`) hits it via `update` today, not close. |
| `GetIssuesByIDs` | raw db-prefixed on error | close molecule probe, ready | list-shaped; not-found rare but text differs |
| `GetLabels` | raw | show | best-effort caller, low blast |
| `GetDependenciesWithMetadata` / `GetDependentsWithMetadata` | raw | show, close, dep | text-only divergence |
| `CountDependencies` / `CountDependents` / `CountIssueComments` | raw | show | text-only; also semantically narrower (no wisp-union — SPIKE §3) |
| `IsBlocked` | raw; also the ` (<type>)` blocker suffix (`ccs`) depends on the use-case string shape | close | conformance-sensitive for the `ccs`/close-guard message |
| `GetDependencyRecordsForIssues` | raw | close, ready | text-only |
| `GetConfig`/`GetAllConfig`/`GetMetadata`/`GetStatistics` | raw on infra error (config-miss returns `""`, so lower risk) | routing, PreRun | mostly benign |

**Recommendation (unchanged from §6.1):** introduce one shared `mapUowError` and route EVERY
override through it (not just `GetIssue`), with a per-method not-found fixture in the Phase-1
harness. The `CloseIssue` not-found contract is the load-bearing one; the counts/labels/deps
leaks are text-only but are exactly the silent-divergence class the differential harness exists
to catch.

## Silent-answer gaps (no error channel — cannot be ErrUnsupported)

`GetInfraTypes` (→ `map[string]bool`) and `IsInfraTypeCtx` (→ `bool`) have no error channel;
their shell stubs return a benign zero value (`nil`/`false`), i.e. a *silent wrong answer*.
Both are therefore real overrides in `store.go` (not shell), pinned by
`TestSpikeUOWStore_IsInfraTypeCtxRoutes`. Any future missing no-error-channel method (e.g. a
capability predicate) needs a value-equality tripwire, not an ErrUnsupported assertion.
