# SPIKE REPORT — `uowStore` vertical slice (Route A of #4547)

**Branch:** `spike/backend-seam-derisk`
**Date:** 2026-07-02
**Companion designs:** `PROPOSAL-pluggable-storage-backends.md` (§4.0/§4.5/§4.7, Phase 1/4),
`PROPOSAL-uowstore-adapter.md` (§2/§4 — this spike is the embryo of that adapter)

## 1. What was built

A new package `internal/storage/uowstore` with one type:

```go
type uowStore struct {
    storage.DoltStorage         // embedded nil interface: untouched methods PANIC (spike-legal)
    provider uow.UnitOfWorkProvider
    actor    string
}
```

It satisfies `storage.DoltStorage` by **embedding the interface as a nil value** (so the
**121** methods this spike does not implement compile away and panic loudly if ever reached —
the interface is 144 methods and 23 are overridden) and
**overriding a real vertical slice**. Each override is one short unit of work:
`provider.NewUOW → use-case call → Commit(ctx, outcome-derived msg)` for writes (via
`uow.RunInTxMsg`, which already owns phase-aware retry), and `NewUOW → use-case call →
Close` (rollback) for reads. Reads **never** `Commit("")`.

Wiring (env-gated, default byte-identical): `cmd/bd/store_factory.go` +
`store_factory_nocgo.go` — the proxied-server arms return `uowstore.New(provider, actor)`
when `BD_SPIKE_UOWSTORE=1`; `usesProxiedServer()` returns false under the flag so commands
travel the ordinary store path (not the `*_proxied_server.go` duals); one guard in
`main.go`'s PreRun (`proxiedServerMode && !spikeUOWStore()`) lets the store path construct
instead of short-circuiting to the uow provider. The init gate was **not** lifted and no dual
file was modified.

Test: `cmd/bd/spike_uowstore_integration_test.go` (`TestSpikeUOWStore_RoundTrip`, gated by
`BEADS_TEST_PROXIED_SERVER=1`) round-trips **create → get → search → ready → close** through
the spike store and asserts the JSON output shape matches the embedded store path for each op.

### Verification (real output)

```
$ BEADS_TEST_PROXIED_SERVER=1 CGO_ENABLED=1 go test -tags gms_pure_go \
      -run TestSpikeUOWStore_RoundTrip ./cmd/bd/ -count=1
ok  	github.com/steveyegge/beads/cmd/bd	22.965s
```

Manual end-to-end through the spike store (prefix `spike`, proxied):
`create → spike-8y1 (open)`, `show → open`, `ready → [spike-8y1]`, `close → closed`,
`ready after close → []`. The denormalized `is_blocked`/ready recompute fires correctly on
close (ready drops to 0), which is the §2.3 semantic the proposal flags as the most
conformance-dangerous — and it works for free because the use-cases already implement it.

`go build ./...` green (cgo + nocgo); `go vet ./cmd/bd ./internal/storage/uowstore` clean.

## 2. Feasibility verdict: **GREEN — the full adapter is buildable, but the mapping is faithful only where the adapter explicitly re-threads store-side state.**

Route A is confirmed. The headline claim of `PROPOSAL-uowstore-adapter.md` (~60% of core maps
1:1 to existing use-case methods; the true remaining cost is a small set of gap units, not 54
command conversions) held up against the code. Every method needed for this five-command
slice was either a DIRECT 1:1 use-case call or a small COMPOSE. Zero methods in the slice were
true GAPs. The use-case layer's semantics (minted IDs, infra-type routing, is_blocked
recompute, same-tx events) are reused wholesale — the adapter is genuinely thin.

**But "thin" is not "free": the adapter is faithful only where it re-threads state the
embedded store carries on the `*types.Issue` and the use-case reads from a *separate* params
field.** The red team found a concrete counterexample — `CreateIssue` dropped `issue.Labels`
because the embedded store persists them off the issue struct (`issueops.PersistLabels`) while
the domain use-case reads only `params.Labels`; the adapter now copies them across (§3). This
is exactly the silent-divergence class the spike existed to surface: "zero business logic" is
true, but "zero mapping logic" is not — every place the two contracts disagree on where a
field lives is a fixup the adapter must own and the Phase-1 harness must fixture.

**The single most important spike finding: the "core five" is a lie about surface area.**
Proving create→get→search→ready→close end-to-end through the store path required **23 real
store-interface methods**, not 5–8 — because the CLI's *command handlers* AND its
`PersistentPreRunE` helpers fan out from the headline op into a surrounding read surface
before and after it (the original slice was 21, but the PreRun census missed `GetMetadata`
and `GetStatistics` — see §4). This is invisible from the interface but decisive for sizing
the real adapter. Details in §4.

## 3. Per-method friction (what actually cost something)

| Store method | Disposition | Friction |
|---|---|---|
| `GetIssue` | DIRECT + fixups | **Three real fixups the adapter must own.** (a) The use-case's `GetIssue` probes only the `issues` table; the store contract also falls back to `wisps` — the adapter must replicate the fallback (`issueops.GetIssueInTx` does). (b) Not-found convention differs: the store contract is `storage.ErrNotFound`, the uow path surfaces a wrapped `sql.ErrNoRows` (`domain/db/issue.go:274`). The adapter must translate. Missing either silently changes `bd show` not-found behavior. (c) **Labels are NOT hydrated on the returned issue.** The store contract's `GetIssue` attaches labels in-tx (`issueops/get_issue.go` sets `issue.Labels`); the use-case's `GetIssue` selects only issue columns. `bd show` is shielded because it fetches labels via the separate `GetLabels` call, but any store-path consumer reading `GetIssue(...).Labels` (hook payloads, exporters, future commands) sees empty labels on the spike path. The full adapter must hydrate labels in `GetIssue` (extra `LabelUseCase.GetLabels` in the same UOW) and carry a labels-in-GetIssue parity fixture. |
| `CreateIssue` | DIRECT + fixups | (a) Store mutates the caller's `*types.Issue` in place with the minted ID; the use-case does too (shared pointer via `CreateIssueParams.Issue`) — so this works, but it is a **contract the adapter relies on**, not a guarantee the use-case documents. (b) **Infra-type→wisp routing lives in the store, not the use-case** (`EmbeddedDoltStore.CreateIssue` flips `Ephemeral` for infra types; `IssueUseCase.create` does not). The adapter must call `ConfigUseCase.IsInfraTypeCtx` itself before the write, in a *separate read UOW* (because `RunInTxMsg` may replay `fn`, the routing decision must be pre-computed and idempotent). (c) **Labels live in different places on the two contracts.** The embedded store persists `issue.Labels` off the `*types.Issue` (`issueops.PersistLabels`); the domain create reads `params.Labels`. The adapter must copy `issue.Labels → CreateIssueParams.Labels` or every label is silently dropped — empirically confirmed by red team (`bd create -l x` on the spike path echoed the label but `bd show` returned none). Fixed. **Comments are still a gap:** the embedded store persists `issue.Comments` (`issueops.PersistComments`) but the domain create has no comment path, so a create carrying comments (import) would drop them; the store-path `bd create` never populates Comments, so this is latent, not live — a full adapter needs a comment-write gap unit. (d) **Replay caveat:** because `RunInTxMsg` may replay `fn` after a pre-commit transient, and the use-case writes the minted ID back through the shared `issue` pointer, a replay re-enters with `issue.ID` already set from the failed attempt; the ID-minting path is idempotent for explicit IDs but the minted-ID replay semantics deserve a fixture in the real adapter. |
| `CloseIssue` | DIRECT + error-mapping fixup | The store's `CloseIssue` is the **raw** op; all the command-level validation (epic open-children, gate satisfaction, blocker refusal) lives ABOVE the store in `close.go`/`close_proxied_server.go`, so the adapter does none of it. The only adapter work is the issue-vs-wisp table probe (mirrors `proxiedResolveIssueOrWisp`). **Error-mapping caveat (same class as `GetIssue`):** on failure `CloseIssue` currently surfaces the use-case's wrapped text (`db: IssueSQLRepository.Close …`) and raw wrapped `sql.ErrNoRows`, not the store path's `issueops` text and `storage.ErrNotFound` sentinel. Shielded today because `close.go` resolves via `GetIssue` first (which the adapter *does* translate byte-compatibly), but any direct store consumer would see different bytes and different `errors.Is` behavior. The real adapter needs the shared `mapUowError` helper (§6.1) routed through EVERY method, not just `GetIssue`, with per-method not-found fixtures in the Phase-1 harness. |
| `SearchIssues(WithCounts)`, `GetReadyWork(WithCounts)` | DIRECT | Trivial: use-case returns `SearchPage{Items, HasMore}`; adapter returns `.Items`. The store interface drops `HasMore` — which is exactly the `--offset`/paging divergence the proposal flags (proxied-only). A real core-paging extension would restore it. |
| `GetLabels` | DIRECT | 1:1 `LabelUseCase.GetLabels`. |
| `GetDependenciesWithMetadata` / `GetDependentsWithMetadata` | COMPOSE | `DependencyUseCase.ListWithIssueMetadata` with `Direction: Out`/`In`. The direction mapping is the whole content of the compose — confirmed against the doc's §4.2. |
| `CountDependencies` / `CountDependents` | DIRECT-ish | `DependencyUseCase.CountByIssueID(Out/In)`. **Signature-faithful but semantically narrower:** the store counts sum `dependencies` + `wisp_dependencies` (`embeddeddolt/counts.go`); the use-case count hits one table. Correct for regular issues (the spike case); the **wisp-union is a documented gap** a full adapter must close. |
| `CountIssueComments` | DIRECT | `CommentUseCase.CountCommentsForIssue`. |
| `IsBlocked` | DIRECT | 1:1 `DependencyUseCase.IsBlocked` — signatures match exactly incl. `(bool, []string, error)`. |
| `GetDependencyRecordsForIssues` | DIRECT | 1:1 `DependencyUseCase.GetForIssueIDs` (`map[string][]*types.Dependency`). |
| `GetIssuesByIDs` | DIRECT | 1:1 `IssueUseCase.GetIssuesByIDs`. |
| `GetCustomStatusesDetailed`, `GetCustomTypes` | DIRECT | 1:1 `ConfigUseCase`. |
| `GetInfraTypes` | DIRECT + **signature snag** | Store returns `map[string]bool` (no error); use-case returns `(map, error)`. Adapter must **swallow the error** to fit the store signature. Same snag on `IsInfraTypeCtx` (store returns bare `bool`). This is a genuine seam-shape mismatch the clean-room design (§4.0) fixes by giving the seam an error channel everywhere. |
| `GetConfig`, `GetAllConfig` | DIRECT | 1:1 `ConfigUseCase`. |

**No true GAPs were hit in this slice.** Error-mapping and infra-routing were the recurring
real work; both are small and mechanical, but both are *silent-divergence* risks if skipped —
exactly the class the Phase 1 differential harness exists to catch.

## 4. Correction to the adapter doc's §4 gap list (important)

The doc's gap taxonomy (§4.6: ~60% DIRECT / ~15% COMPOSE / ~10% FALLBACK / ~15% GAP) is
**accurate for the methods it names**, but it measures the wrong denominator. It counts the
`storage.DoltStorage` interface. The real adapter cost is driven by the **transitive command
read-surface**, which the interface census does not reveal. Concretely, each headline command
pulled in this many *additional* store methods before it would run end-to-end:

- **`bd list` / `bd ready`** (before touching `SearchIssues`/`GetReadyWork`): the list-filter
  loader needs `GetCustomStatusesDetailed` + `GetCustomTypes` + `GetInfraTypes`
  (`cmd/bd/list_filter.go`), and the repo auto-routing preflight needs `GetAllConfig` +
  `GetConfig` (`cmd/bd/routing_read.go` — runs on **every read command**).
- **`bd show --json`** (default output, no `--include-*`): `GetLabels`,
  `GetDependenciesWithMetadata`, `CountDependents`, `CountDependencies`, `CountIssueComments`
  (`cmd/bd/show.go:149-158`).
- **`bd close`** (no flags): `IsBlocked` (pre-close check) plus an **unconditional**
  parent-molecule auto-close probe — `findParentMolecules → GetDependencyRecordsForIssues →
  GetIssuesByIDs` (`cmd/bd/mol_current.go:413,456`).
- **Every write command's `PersistentPreRunE`** (BEFORE any RunE): `validateWorkspaceIdentity`
  calls `GetMetadata` (`cmd/bd/main.go:1441`), and `maybeAutoImportJSONL` calls `GetStatistics`
  (`cmd/bd/auto_import_upgrade.go`) on any workspace whose `.beads/issues.jsonl` is non-empty.
  Both are now overridden (DIRECT). **This is the census's most important blind spot:** the
  original 21-method slice was derived by grepping *command handlers*, but the PreRun helpers
  run on the global store too — the spike's manual verification and integration test only
  escaped nil-panics because they used fresh temp dirs (no `issues.jsonl`) and set
  `BEADS_SKIP_IDENTITY_CHECK=1`. A real workspace panics without `GetMetadata`/`GetStatistics`.
- **Every command's molecules loader** (`cmd/bd/main.go` PreRun → `internal/molecules/molecules.go`)
  calls `CreateIssuesWithFullOptions` whenever a user/town/project `molecules.jsonl` carries
  templates not yet in the fresh spike DB. This one is **NOT overridden** in the spike (a write
  batch with `BatchCreateOptions`; latent because the test workspaces have no molecule templates)
  — it must be implemented or explicitly guarded before the spike wiring is exercised outside a
  clean temp dir.

None of the read fixups are GAPs — they are all DIRECT/COMPOSE — but they mean the adapter's *minimum
buildable slice per command* is 3–5× the headline method. **Recommendation for the real
adapter:** size it against the CLI's command→store call graph (a `grep` census of
`store.<Method>(` / `activeStore.` / `issueStore.` call sites per command **including the
`PersistentPreRunE` helpers and the molecules loader**, not just command handlers), not against
the interface. The 23 methods this spike implements already cover the read/config/dep-count
substrate that most CORE commands share, so the marginal cost of the *next* commands is lower
than the first — but the doc should add a "transitive read-surface" row to its gap scoreboard.

The doc's named GAP units (comment-writes, events-reads, local-metadata/D5, slots,
advanced/compaction queries) are **confirmed** — none were needed for this slice, and none
have an obvious use-case method, consistent with the doc. `SetLocalMetadata`/`GetLocalMetadata`
in particular are untouched here; the tip-metadata PostRun path is bypassed in spike mode
(the store self-commits per call, so `proxiedServerMode` still gates PostRun), which sidesteps
but does not solve the D5 "local-on-a-shared-server" ambiguity the doc flags.

## 5. LOC and cost

| Artifact | LOC |
|---|---|
| `internal/storage/uowstore/store.go` (23 overrides + 2 helpers) | ~450 |
| `cmd/bd/spike_uowstore_integration_test.go` | 343 |
| Factory + main.go wiring (`store_factory.go` +37, `store_factory_nocgo.go` +30, `main.go` +8) | 74 |

**Extrapolated full-adapter cost.** Averaging ~11 LOC per override (many are 6-line
NewUOW/call/Close bodies; COMPOSE and fixup methods run 15–25):

- The ~107-method flat core (§4.1) at ~11 LOC ≈ **1.2k LOC** for the mechanical DIRECT/COMPOSE
  body, PLUS the five named gap units (each a small use-case addition + repo method:
  comment-write, events-read, slots, advanced/compaction, local-metadata/D5). Call it
  **~1.5–2.5k LOC of adapter + ~1–1.5k LOC of new use-case/repo surface for the gaps.**
- This is comfortably inside the proposal's Phase 4 Route A envelope (4–6 weeks). The spike
  spent its effort on *discovery* (which methods, which fixups) not *volume*; with the
  call-graph census in hand, the remaining methods are near-mechanical.
- Route A's core claim — reuse the parity-tested dangerous semantics (same-tx events,
  is_blocked propagation, delete/purge neighbour recompute) instead of re-deriving them — is
  **validated**: the spike wrote zero SQL and zero business logic, and the is_blocked-on-close
  recompute worked on the first try.

## 6. Risks / caveats surfaced

1. **(§6.1) Error-mapping is load-bearing and silent, and applies to EVERY method, not just
   `GetIssue`.** `GetIssue`'s ErrNotFound-vs-ErrNoRows and the wisp fallback are the kind of
   thing that passes a happy-path test and breaks `bd show`'s not-found contract. The spike
   translated only `GetIssue`; `CloseIssue` and the other reads (`GetIssuesByIDs`, `GetLabels`,
   counts) still surface raw `db:`-prefixed use-case text and wrapped `sql.ErrNoRows`. The full
   adapter needs a single shared `mapUowError` helper routed through **all** methods, and the
   Phase 1 harness must carry not-found fixtures per method.
2. **Signature mismatches (`GetInfraTypes`/`IsInfraTypeCtx` drop the error).** The flat store
   interface has no error channel where the use-case does; the adapter swallows. The
   clean-room seam (§4.0) should standardize on error-returning everywhere.
3. **Per-call transaction granularity is a concurrency-CORRECTNESS risk, not just `dolt log`
   noise (red-team correction).** Every store call is its own short tx. Through the spike store
   one `bd close` becomes N *independent* transactions — resolve/GetIssue, `IsBlocked`
   (`close.go:141`), `CloseIssue` (`:152`), the molecule probe (`:169`), the re-fetch (`:172`)
   — whereas the proxied dual runs validation+close+continue+claim inside ONE UOW committed
   once (`close_proxied_server.go:103`), and embedded mode holds the workspace flock for the
   whole command. That is a **contract change**, not a cosmetic history difference: agent A
   passes the `IsBlocked` check in tx1; agent B adds a blocking dependency and commits; agent
   A's `CloseIssue` commits in tx2 → a blocked issue closes, which neither the dual nor the
   embedded path permits. This is the exact multi-writer scenario proxied mode exists for.
   **Phase 2b must pin it:** the full adapter needs the `RunInTransaction` mapping
   (`PROPOSAL-uowstore-adapter.md §4.5`) delivered for read-check-act commands like `close`
   BEFORE the proxied full surface ships, plus a two-process race fixture in the Phase-1 harness.
   The `dolt log` divergence is the visible symptom; the closeable-while-blocked race is the
   real hazard.
4. **Env-gated wiring only.** Default behavior is byte-identical (flag off → the proxied arms
   return the original error, `usesProxiedServer()` unchanged). Verified: existing proxied
   integration tests fail identically with/without the spike changes (they fail on the
   pre-existing init gate, `--proxied-server is not yet implemented`, which was NOT lifted).
5. **The shape-equality integration test is weaker than the oracle's byte diff — do not read
   it as conformance evidence.** `normalizeIssue` drops empty/null values before comparing key
   sets, so a field that is present-but-empty on one backend and absent on the other compares
   equal — masking exactly the representational-divergence class (`metadata {}` vs absent) the
   regression normalizer documents as real. It is a shape check, not a conformance check; the
   real gate for the adapter is the Phase-1/Oracle-B differential harness run against it with
   empty-vs-absent fixtures. (The test now also pins the label-persistence and not-found paths,
   which the pure key-set check was structurally blind to.)
6. **(§6.4) The flag's blast radius is wider than a single arm, and the wiring pattern must NOT
   graduate.** With `BD_SPIKE_UOWSTORE=1`: (a) `usesProxiedServer()` is forced `false` while the
   workspace IS proxied — a topology predicate that lies. This disables the 13 working proxied
   dual commands (they route into the nil-panic minefield) and, worse, its OTHER consumers
   silently change behavior: `doctor.go:207`'s proxied guard is bypassed and `init.go:1188`
   would persist the wrong `DoltMode`. It is safe in the spike (env-gated, default-off) but is
   the exact split-brain the clean-room design forbids — `main.go:1223`'s PostRun keys off the
   raw `proxiedServerMode` global (stays true) while the function says embedded. (b)
   `newReadOnlyStoreFromConfig` returns the writable spike store, ignoring `doltCfg.ReadOnly`,
   so read-only-command protection is bypassed. (c) `flushBatchCommitOnShutdown` calls
   `store.Commit` — unimplemented — and would panic if a user with dolt autocommit=batch signals
   the process. **Must-not-survive at promotion:** dual dispatch dies by DELETING the
   `*_proxied_server.go` duals, not by falsifying the mode predicate; the factory arm keys on
   the locator (`cfg.IsDoltProxiedServerMode`) with NO env consulted at open; the nil embed is
   replaced by a generated typed-`ErrUnsupported{Op, Backend}` shell that names
   `BD_SPIKE_UOWSTORE`/#4547 (loud typed errors, not raw nil-pointer panics).

## 7. Recommendation

Ship Route A. The adapter is real, thin, and reuses the exact semantics the May-2026 store
attempt lacked. Before committing to the full build, run the **command→store call-graph
census** described in §4 to get an accurate method count — the interface undercounts the work
by ignoring the transitive read-surface, and overcounts it by including capability methods a
core adapter never needs. This spike's 23 methods are a reusable substrate for that census.

## 8. Slice 2 — `RunInTransaction` + typed-unsupported shell (follow-up)

Slice 2 closes the two §6 hazards this spike's own recommendation flagged as
must-not-survive: the **nil-embed panic minefield** (§6.4/§6.6) and the missing
**`RunInTransaction` read-check-act atomicity** (§6.3). It touches only additive
files — `internal/storage/errors_unsupported.go`, `internal/storage/uowstore/**`,
and new `cmd/bd/spike_uowstore_tx_integration_test.go` — so the default
(BD_SPIKE_UOWSTORE-unset) path stays byte-identical (Oracle A in-scope 100% pass).

### 8.1 Part B — the nil embed is gone; gaps are now loud AND typed

The `storage.DoltStorage` nil-interface embed is replaced by a **generated
concrete shell** (`unsupported_gen.go`, from a stdlib-only `go/ast` generator in
`gen/main.go`). Every unimplemented method with an error channel returns a typed
`*storage.ErrUnsupported{Op, Backend}` wrapped with BD_SPIKE_UOWSTORE / #4547
context — never a nil-pointer panic. The §6.4 crash class is retired: `store.Commit`
(from `flushBatchCommitOnShutdown`), `CreateIssuesWithFullOptions` (molecules
loader), and every dual command that falls through under the flag (e.g. `bd dep
add → AddDependency`) now exit nonzero with a clean message, verified by the CLI
fixture (`bd dep add` output contains the typed text and no `panic:`/`goroutine`).

**Two signatures have no error channel and are the sole exception to the
"typed-error everywhere" rule: `GetInfraTypes` (returns `map[string]bool`) and
`IsInfraTypeCtx` (returns bare `bool`).** The generator emits them as zero-value
stubs (it comments the tradeoff inline: *"no error channel on this signature;
returns the zero value"*), so a stub in their place returns `nil`/`false` — a
*silent wrong answer*, not a loud typed one. Both are therefore **real overrides
in `store.go`, never left to the shell**: each opens a UOW, calls the use-case,
and swallows the use-case's error to fit the store signature. `IsInfraTypeCtx` in
particular is reachable under the flag from `wisp.go:637/:682/:759`, where a silent
`false` would misroute infra-type wisp handling; its override is pinned by
`TestSpikeUOWStore_IsInfraTypeCtxRoutes` (seed `types.infra`, assert `agent`→true —
verified RED when the override is removed, so the shell stub cannot masquerade for
it). This is the concrete evidence the §4.0 clean-room seam cites for its
error-channel-everywhere rule: the two methods that lack one are exactly the two
the shell cannot make loud.

Two design choices matter for the real adapter:

- **Codegen, not a handwritten stub.** SPIKE-REPORT §6.6 mandated a *generated*
  shell, and Phase 4's compat wedge regenerates the same thing against the
  ~107-method bridge core, so the generator is the reusable artifact. The drift
  guards are layered, and each covers a distinct class:
  - **Interface growth / a hand-deleted stub** — two compile-time completeness
    assertions (`var _ storage.DoltStorage = …{}`, `var _ storage.Transaction =
    …{}`) live INSIDE the generated file. The shell type alone must satisfy the
    full interface, so if either interface grows a method, or a stub is deleted by
    hand, the build breaks and forces `go generate`. Interface drift can never
    silently grow or drop a stub.
  - **A hand-edited stub *body*** (a typo'd `Op` string, or a stub changed to
    return `nil` instead of the typed error) — the compile assertions cannot see
    this; it type-checks. `gen/gen_test.go::TestGeneratedShellIsUpToDate` closes
    it: an ordinary (ungated) unit test that regenerates to a temp path and
    byte-compares against the committed `unsupported_gen.go`, the same drift-check
    idiom `scripts/check-cli-docs-drift.sh` uses for CLI docs. It is verified RED
    against a corrupted committed file. To make this testable the generator's
    `run()` was parameterized on `(srcDir, outFile)`; `main()` still writes the
    canonical file. (A CI *workflow* gate is promotion-time work — no
    `.github/workflows` step invokes the generator today — but the drift class the
    §6.6 codegen mandate cares about is now pinned by the checked-in unit test, not
    just asserted.)

  The DO-NOT-EDIT header signals intent; the unit test enforces it (the exact way
  the molecules-loader gap would otherwise reappear as a quiet PreRun failure).
- **The silent-unsupported tradeoff is real and must be tripwired.** Converting a
  panic to a quiet typed error also means a typo'd override name compiles and
  silently routes to the stub. The mitigation is one integration assertion per real
  override (RoundTrip + T1–T5 collectively exercise all three tx overrides and the
  store slice); a stub answering in an *error-returning* method's place returns
  ErrUnsupported, which those tests catch. **The two no-error-channel methods
  (`GetInfraTypes`/`IsInfraTypeCtx`) cannot rely on that** — their stub returns a
  benign zero value, not a catchable error — so they need a *value-equality*
  tripwire instead: `TestSpikeUOWStore_IsInfraTypeCtxRoutes` seeds `types.infra`
  and asserts `agent`→true, which the shell stub (always `false`) fails. This is
  the general lesson for the real seam: an error-channel-everywhere signature is
  what lets a single ErrUnsupported assertion cover every override; where the
  signature refuses one, the tripwire must assert the *answer*, not the error.

### 8.2 Part A — `RunInTransaction` is ONE delegation to `uow.RunInTx`

`(*uowStore).RunInTransaction` opens ONE `uow.UnitOfWork` and runs `fn` against a
`uowTransaction` view **constructed inside the retry closure**; the adapter adds
zero retry/backoff/phase logic (grep gates: no `backoff` import anywhere in the
package, no `NewUOW(` in `tx.go`). All of that is inherited from `uow.RunInTx` and
remains owned/tested by `internal/storage/uow/run_in_tx_test.go`. The view is the
minimal read-check-act set — `GetIssue` (read-your-writes + `storage.ErrNotFound`
translation, via a helper now SHARED with the store method), `CloseIssue` (in-tx
`is_blocked` recompute), `AddDependency` (wisp routing + in-tx cycle check) — which
is enough to prove multi-statement atomicity across two tables (issues +
dependencies). The other 21 Transaction methods stay generated-unsupported; because
a stub error propagates out of `fn`, calling one also exercises the rollback path.

Semantics inherited for free and pinned by the integration fixtures (a managed
proxy + two independent in-process store handles):

| Fixture | Contract proven |
|---|---|
| T1 rollback atomicity | `fn` error → BOTH mutations roll back; the second handle sees the issue still open and dep count 0; dolt_log head UNCHANGED. Read-your-writes + cross-handle isolation asserted mid-`fn`. |
| T2 commit-once | success → EXACTLY ONE new dolt commit (DOLT_LOG walk between head hashes), message byte-equal to `commitMsg`. This is the assertion a stdout check structurally cannot make, and it is precisely the §6.3 "N commits instead of one" divergence class. |
| T3 typed-unsupported in `fn` | a `Transaction` stub's `*storage.ErrUnsupported` is treated as a domain error — no retry, full rollback. |
| T4 read-only `fn` | zero mutations → "nothing to commit" → success with NO new version commit. |
| T5 CLI clean error | `bd dep add` on the spike path exits nonzero with the typed text and no crash output. |

Three commit-semantics edges embedded mode handles differently were handled
explicitly, NOT papered over:

- **Blank commit message** — embedded means "SQL-commit, defer the version
  commit", but the uow Tx only has `DOLT_COMMIT`, which rejects empty messages.
  Passing it through yields a confusing dolt error; substituting a default silently
  forks version history. `RunInTransaction` refuses a blank message with an explicit
  guard (unit-tested: guard fires before any UOW opens, `fn` not called). This is
  unreachable from the CLI on the spike path because `transactHonoringAutoCommit`
  only blanks the message in embedded mode (`dolt_autocommit.go:31`).
- **"Nothing to commit"** maps to success — owned by `RunInTxMsg`, not intercepted
  in the adapter (T4).
- **`ErrCommitIndeterminate`** (connection loss at/after COMMIT) is terminal and
  surfaced via `errors.Is`, NEVER retried — a hoisted view or an adapter-level retry
  around `uow.RunInTx` would risk a double-apply (second close event, duplicate
  dependency). Not runtime-tested (needs fault injection the proxied stack does not
  expose); correct by construction (single delegation) and covered by
  `run_in_tx_test.go`.

**Open edge — a NON-transient commit-phase failure and the pooled session
(corrected).** An earlier draft of this section overstated commit-phase safety as
"correct by construction ... covered by `run_in_tx_test.go`". That is true for the
*retry policy* (`RunInTxMsg` classifies a `Permanent` DOLT_COMMIT failure and does
not replay), but it says nothing about the *pinned session's state* after the
failure. On BASE, `doltServerTx.Commit` marked the tx `done` and released the conn
unconditionally: when DOLT_COMMIT fails with a non-transient error (a commit-time
constraint-verification or working-set error — NOT serialization/conn-loss), the
session returned to the pool **still holding the open transaction with `fn`'s
writes pending**. go-sql-driver v1.9.3's `ResetSession` only does a liveness check
(no `COM_RESET_CONNECTION`), so the next borrower's `START TRANSACTION` implicitly
commits those orphaned writes — a late/double-apply, the exact atomicity-violation
class T1 is meant to preclude but never exercises (T1's rollback is the *fn-error*
path, which does run `ROLLBACK` and is sound). This hardening once existed
(commit `794ff0790`) and was reverted to BASE in `a59e75325`'s serverv2 triage,
two weeks before this slice; slice 2 makes commit-phase failure a first-class
adapter path via `RunInTransaction`, so the gap became load-bearing here.

  **Fixed (re-landed).** `doltServerTx.Commit` now runs `ROLLBACK` on the same
  session when DOLT_COMMIT fails, and poisons the conn (`driver.ErrBadConn`, so the
  pool discards the session) if even the rollback fails; `Rollback` poisons on its
  own failure; and `BeginTx` closes the pinned conn when `START TRANSACTION` fails
  instead of leaking it. Pinned by `internal/storage/uow/doltserver_tx_test.go`
  (sqlmock, asserting the ROLLBACK-then-release sequence and `db.Stats()` pool
  discard) — verified RED against BASE. This is the same repair the reverted
  `794ff0790` shipped; a live fixture that forces a real `dolt_constraint_violations`
  commit failure and asserts the writes never appear on a second handle remains
  feasible promotion-time work, but the unit-level pool-discard contract is now
  enforced, not merely asserted.

### 8.3 What Slice 2 does NOT do

It does not lift the init gate, delete any `*_proxied_server.go` dual, or change
the flag-gated factory wiring — those are the promotion-time must-not-survive
changes §6.6 describes, not spike work. The Transaction view is still 3-of-24
methods; the rest are typed-unsupported. And the shell's typed errors, while loud,
are still a spike contract (`Backend == "uowstore spike"`, #4547 in the message) —
the real seam's `ErrUnsupported{Op, Backend}` will name the actual backend.

## 9. Slice 3 — gc-16 census, adapter completion, and the first cross-plumbing run

Slice 3 turns §7's *"do the census first"* prerequisite into an artifact, completes
the gc-16 adapter surface against that census, and stands up the first
**cross-plumbing** differential (one binary, embedded vs spike-proxied) so the
"faithful only where re-threaded" claim of §2 is measured rather than asserted. It
touches additive spike files plus two domain-layer parity fixes; the default
(`BD_SPIKE_UOWSTORE`-unset) embedded path is exercised as the *reference* of the new
run, so its behavior is pinned by construction rather than merely left alone.

### 9.1 The census artifact and its headline numbers

`SPIKE-CENSUS-GC16.md` traces the EMBEDDED command path for each of the 16 gc
commands (the 12 dual commands + `purge`, `count`, `version`, `stats`) down to the
`storage.DoltStorage` calls it actually makes — direct (own `RunE`), transitive
(PreRun molecules loader / routing preflight / id resolver / PostRun tip metadata),
with explicit counting rules stated up front. The counts it establishes, and which
the rest of the slice is scored against:

- The generated shell (`unsupported_gen.go`) stubs **144** `DoltStorage` + **24**
  `Transaction` methods. This corrects the looser interface-census figure §8/§6.6
  worked from (~107); the census is the accurate denominator §7 asked for.
- The adapter overrides **24** `DoltStorage` methods (+ `Close`, `RunInTransaction`)
  and **3** `Transaction` methods (`GetIssue`, `CloseIssue`, `AddDependency`),
  leaving **118** `DoltStorage` methods still typed-unsupported.
- The 24 overridden are exactly the read/mutation set the gc-16 handlers + shared
  surfaces reach on the embedded baseline (listed in the census "Coverage baseline"
  block), so "complete for gc-16" means *complete for what those commands call*, not
  complete for the interface.

Honest limit: the baseline traced is the embedded path, and PostRun's write arm is
*bypassed* on the spike path (it sits in the `else` of `proxiedServerMode`, which
stays true under the flag — §4). PostRun-only methods are therefore latent on the
spike path even though the embedded baseline reaches them; the census flags this
per-row rather than hiding it.

### 9.2 Adapter surface now covered (24 of the gc-16 call-set)

Adapter completion landed in `fbb2bc332` (uowstore) with its coverage +
error-mapping contract pinned by `53c72a602`. Every gc-16 command's embedded
call-set is now overridden with a real UOW body sharing one error-mapping helper
(`storage.ErrNotFound` translation etc.), so the surface a gc-16 command exercises
no longer falls through to the shell. The 118 untouched methods remain
typed-unsupported — this is *gc-16 completeness*, not interface completeness, and the
distinction is the census's whole point.

### 9.3 The cross-plumbing run — result, attribution, and what it does NOT prove

`tests/oracle-a/run-oracle-x.sh` runs ONE working-tree binary through two plumbings:
embedded (`BD_SPIKE_UOWSTORE` unset) as REFERENCE goldens, and proxied-server +
uowstore adapter (`BD_SPIKE_UOWSTORE=1`) as CANDIDATE, over the gc-contract corpus.
This is orthogonal to Oracle A (two binaries, one embedded plumbing — the
refactor-safety net); Oracle X is one binary, two plumbings.

Result (binary `spike/backend-seam-derisk@f41ece47`, 45 curated scenarios): **33
in-scope PASS, 11 FAIL (75%)**, plus 1 out-of-scope informational pass. Every one of
the 11 divergences is attributed by class in `tests/oracle-a/CROSSPLUMB-REPORT.md`:

- **class (a)** — real uowstore-caused behavior difference; a finding, never
  allowlisted. Seven of the 11: `cycle_reject` and `dep_retype` (F-1, error
  wording), `tiers_ephemeral` (F-2, `--no-history` list visibility),
  `ready_excludes_infra_and_coordination_types` + `list_excludes_gate_and_infra_types`
  (both F-3, `-t message` infra-type not auto-marked ephemeral by the adapter —
  `uowstore/store.go:495` `IsInfraTypeCtx('message')`), `output_parent_omitempty_boundary`
  (F-4, `started_at` on the in_progress transition), `purge_dry_run_zero_metrics`
  (F-5, close-output label hydration).
- **class (b) mode difference** and **(c) unimplemented-out-of-scope** — allowlistable.
  Four allowlisted at run time, now **3** after AX-4's withdrawal (see §9.5):
  `sql_unsupported_embedded` (AX-1, raw-DB rejected on both, different surface),
  `config_set_protected_keys` (AX-2, sql-server-only key + missing spike `config.yaml`),
  `comment_add_list` (AX-3, `AddIssueComment` typed-unsupported — `comment` is outside
  the gc-16 completion set, so NOT a census escape; the harness `IN_SCOPE_CMDS`
  predates and is broader than gc-16).

**What the run does NOT prove.** It is a differential over a *curated corpus*, not a
proof of equivalence: (1) it only covers the ~45 scenarios in the rig, so unexercised
code paths are silent; (2) the reference is the embedded path, so any behavior wrong
in *both* plumbings passes undetected; (3) golden comparison is post-normalization,
so anything the normalizer folds (timestamps, hashes) is invisible; (4) an
allowlisted class-(b)/(c) divergence is an *accepted* difference, not an identical
one — the allowlist records four such surfaces where the two plumbings deliberately
differ. The run establishes *where* the proxied-uowstore path is and is not
byte-equivalent to embedded over the corpus, with every gap attributed — nothing
stronger.

### 9.4 Two new corpus scenarios

`4de3065a3` pins two PR #4560 review gaps in `tests/oracle-a` (Oracle A corpus):
**force-close-of-pinned** and the **bd-note separator**. These are corpus additions
(refactor-safety scenarios), distinct from the Oracle X cross-plumbing run above.

### 9.5 Review findings and dispositions

An adversarial review of the slice raised blocker/major findings; all
blocker/major items were fixed (none refuted). The fixes:

- **`fb385cd92` (uowstore delete cascade/force + update lifecycle parity)** closes
  three blockers and two majors in the domain layer:
  - *DeleteIssues cascade/force absorbed (data-loss blocker).* Added
    `DeleteIssuesParams.EnforceCascadePolicy/Cascade/Force` +
    `DeleteIssuesResult.OrphanedIssues`; new `deleteManyWithPolicy` mirrors
    `issueops.DeleteIssuesInTx` (cascade-expand / refuse-if-external / orphan-under-force).
    The legacy always-cascade path is preserved for `delete_proxied_server.go`
    (untouched, per hard rule). Verified live: `delete a b --force` with external
    `ext` → both plumbings `deleted_count=2`, `orphaned_issues=[ext]`, `ext` survives.
  - *Batch-delete preview lost its refusal / "Would delete: 0" (blocker).* Same path:
    refusal returns before the dry-run branch with the identical string, and dry-run
    `DeletedCount` is now computed.
  - *UpdateIssue skipped `ManageClosedAt` lifecycle (blocker — `closed_at` NULL / stale
    on reopen).* `domain/db/issue.go` `Update` now reads the prior row on a status
    change and applies `issueops.ManageClosedAt` + `ManageStartedAt` (this also fixes
    the reported **F-4** `started_at`).
  - *Domain repo always recorded `EventUpdated` (major).* `Update` now uses
    `issueops.DetermineEventType` (`EventClosed`/`EventReopened`/`EventStatusChanged`).
- **`c541dda57` (oracle-x attribution gate + ancestor-`.beads` block)** fixes two
  majors in the harness itself: the `run-oracle-x.sh` exit contract is now enforced
  (verdict parses allowlisted AX-N headers + attributed report-table names and exits
  1 on any failing scenario in neither; scoreboard exit captured via `PIPESTATUS`),
  and an `assert_no_ancestor_beads` preflight now walks `$SCENWORK`→`/` and dies if any
  ancestor `.beads` exists (confirmed to trip on this host's real stale `/tmp/.beads`).
- **`4d353d544` (docs)** withdraws AX-4 and records the self-dep divergence: AX-4's
  `purge_real_then_reseed` `events:4 vs 0` was a mis-classification — a genuine
  class-(a) counting bug smuggled in as class-(b). Fixed at source in `fb385cd92`
  (`deleteManyWithPolicy` counts wisp aux rows for `cascadeWispIDs` only, not
  directly-purged wisps), so the all-ephemeral purge reports `events:0` on both
  plumbings; AX-4 is withdrawn from `CROSSPLUMB-ALLOWLIST.md` (total 4→3). The doc
  fix also expands **F-1** to enumerate all three wording divergences including the
  step-4 self-dependency (embedded "cannot add self-dependency" vs proxied "would
  create a cycle"), left as an open error-wording finding rather than code-fixed this
  slice.

Remaining open class-(a) findings after this slice, all outside the blocker/major
scope of the review: **F-1** (error wording), **F-2** (`--no-history` list
visibility), **F-3** (`-t message` infra-type auto-ephemeral —
`uowstore/store.go:495`), **F-5** (close-output label hydration). They are recorded,
not resolved.

Pre-existing unrelated failure left untouched per hard rule:
`internal/storage/issueops/config_yaml_fallback_test.go` (a deliberate untracked
file) fails on custom-types/status-fallback — `issueops` is a lower layer these edits
do not touch, so it is orthogonal to Slice 3.

### 9.6 What Slice 3 does NOT do

It does not lift the init gate, touch any `*_proxied_server.go` dual, or change the
flag-gated factory wiring. The adapter is complete for the gc-16 call-set only (24 of
144 `DoltStorage` methods); the other 118 remain typed-unsupported. The
cross-plumbing run is a corpus differential, not an equivalence proof (§9.3), and 3
of its divergences remain accepted-by-allowlist while 4 class-(a) findings (F-1, F-2,
F-3, F-5) remain open.

### 9.7 Slice 4 — equivalence-clean

(Numbered 9.7, not 9.5: 9.5/9.6 above are already taken by the Slice-3 record.)

Slice 4 closes the four class-(a) findings §9.5 left open (F-1, F-2, F-3, F-5;
F-4 was already fixed in the Slice-3 lifecycle work) so the flag-off embedded ↔
`BD_SPIKE_UOWSTORE` spike-proxied cross-plumbing differential is
equivalence-clean: every remaining in-scope divergence is one of the 3
allowlisted mode/unsupported/artifact differences (AX-1..AX-3), and no class-(a)
finding diverges.

The four fixes (embedded is the reference on every one):

- **F-1 — dep-add error wording (`f4c20cfa8`).** `AddDependency` routes through
  the domain use-case, which surfaced the three `bd dep add` rejections
  differently from embedded. The use-case `add()` now checks self-dependency
  before the cycle probe, emits the bare `"adding dependency would create a
  cycle"` message, and passes the retype conflict through as a typed
  `domain.DependencyTypeConflictError` whose `Error()` is byte-identical to the
  embedded `issueops` string.
- **F-2 — `--no-history` list visibility (`f9cebb9dd`).** `CreateIssue` only
  routed `Ephemeral` beads to the wisps table, so a `--no-history` bead landed
  in `issues` and showed up in `list --all` / `count`. Embedded's `useWispsTable`
  is `Ephemeral || NoHistory || infra`; routing `issue.NoHistory` to `CreateWisp`
  (ephemeral=0) hides it from `list --all` while `ready` still surfaces it.
- **F-3 — `-t message` infra-type auto-ephemeral (`a0c5879f1`).** The domain
  `ConfigUseCase.GetInfraTypes` returned only the (empty-by-default) `types.infra`
  DB key, so `IsInfraTypeCtx("message")` answered false. Reproduced embedded's
  config.yaml → hardcoded-defaults (`agent`/`role`/`message`) fallback in the
  use-case layer so an unset `types.infra` classifies those types as infra on
  both plumbings.
- **F-5 — close-output label hydration (`f9cebb9dd`).** `close --json` re-fetches
  via `GetIssue`, but the adapter returned the row without labels, so the close
  object carried `labels=null` though labels persisted. `getIssueInUOW` now
  hydrates labels (issues→labels, wisp→wisp_labels), mirroring
  `issueops.GetIssueInTx`.

Differential guards (`426dd0626`) — `TestSpikeUOWStore_CrossPlumbFindings` runs
each remediated command on both plumbings with prefix-normalized IDs and asserts
byte-identical observable output (gated behind `BEADS_TEST_PROXIED_SERVER=1`).
The recorded run itself is `37f621752`.

**Both oracle verdicts (verbatim):**

- Oracle X: `[oracle-x] IN-SCOPE PASS=41 FAIL=3 / [oracle-x] attribution gate OK
  — every divergence is attributed. (exit 0; the 3 divergences are exactly
  allowlist AX-1 sql_unsupported_embedded, AX-2 config_set_protected_keys, AX-3
  comment_add_list — all class b/c/d; ZERO class-(a) divergences)`
- Oracle A: `[oracle-a] RESULT: IN-SCOPE PASS (44 scenarios, 0 divergences) —
  refactor is behavior-preserving on the gc-contract surface. (exit 0)`

**Review findings and dispositions.** An adversarial review of the slice raised
one class-(a) finding; it was fixed (not refuted):

- **fix2 (`ade2eba3b87e99cf404135e67d7594df950fca0d`) — spike create path skipped
  validation.** The spike `CreateIssue` never ran `issue` validation, so it
  diverged from `EmbeddedDoltStore` on two `create` inputs embedded rejects with
  exit 1: custom-only types (`-t agent`/`-t role`/`-t totallybogus`) succeeded
  and — after the F-3 classification fix — silently became ephemeral wisps; and
  `-t message --no-history` persisted a bead with both `ephemeral=true` and
  `no_history=true`, the mutually-exclusive state embedded's validator forbids
  (GH#2619). `prepareForCreate` (renamed from `applyInfraTypeRouting`) now runs
  `ValidateWithCustom` against the same custom status/type sets embedded reads,
  wrapping the error byte-identically; the infra-type ephemeral flip stays
  unconditional to match `EmbeddedDoltStore.CreateIssue`, so the
  `message`+`--no-history` combo reaches the validator instead of slipping
  through. `TestSpikeUOWStore_CrossPlumbFindings` gained a
  `create_validation_parity` subtest pinning all four rejections to embedded's
  message and exit code; full oracle-a stays 44/44 in-scope (the default embedded
  path is untouched).

Pre-existing unrelated failure left untouched per the same hard rule as §9.5:
`internal/storage/issueops/config_yaml_fallback_test.go` (a deliberate untracked
file) fails on custom-types/status-fallback in a lower layer these edits do not
touch.

**What equivalence-clean does and does NOT prove.** It proves that on the gc-16
contract surface the spike plumbing (`BD_SPIKE_UOWSTORE`, the proxied UOW-store
adapter) produces byte-identical observable behavior to the flag-off embedded
store — for those two Dolt-backed plumbings, and only for the corpus/oracle
scenarios exercised. It does **not** prove full equivalence beyond the exercised
surface (24 of 144 methods; the other 118 stay typed-unsupported), it does **not**
lift the init gate or touch any `*_proxied_server.go` dual, and — most
importantly — it says **nothing about Postgres**: both sides of this differential
run over Dolt, so PG is not involved and no PG-backend equivalence is implied.
