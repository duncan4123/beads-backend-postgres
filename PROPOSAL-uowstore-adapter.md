# Sketch: the `uowStore` adapter — one seam instead of two

**Companion to:** `PROPOSAL-pluggable-storage-backends.md` (Decision D1)
**Status:** discussion sketch, not an implementation plan
**Date:** 2026-07-02

---

## 1. The situation, in plain terms

bd today has **two parallel plumbing systems between commands and the database**, and every
command that wants to work in both worlds must be written twice.

**Path 1 — the store path** (every command, since forever):

```
bd close ─► runClose() ─► store.CloseIssue(...)          store = storage.Storage,
                          store.GetNewlyUnblockedByClose  a 62-method interface;
                                                          Dolt implements it directly
```

**Path 2 — Dustin's server path** (13 commands so far, one PR at a time since May):

```
bd close ─► if usesProxiedServer():
              runCloseProxiedServer() ─► uow := provider.NewUOW(ctx)   ← short transaction
                                         uow.IssueUseCase().CloseIssue(...)
                                         uow.Commit(ctx, "bd close")   ← one tx per command
```

Path 2 exists for a good reason: **concurrency**. Embedded Dolt allows one writer per
workspace (flock). Agent swarms run many `bd` processes at once. Path 2 talks to one shared,
long-running `dolt sql-server` through a local proxy, and each command is a short transaction
over a pooled connection — many writers, no flock.

The cost is the duplication you can see in the tree: `close.go` + `close_proxied_server.go`,
`list.go` + `list_proxied_server.go`, … 13 pairs today, ~54 more commands to go if the
conversion continues command-by-command. And the pairs are already drifting: `--offset` works
only on Path 2 (PR #4488), because the two implementations genuinely differ.

**The pluggable-backend project makes this worse if we do nothing:** every new backend
(SQLite, Postgres) would be a THIRD plumbing system, and the "written twice" problem becomes
"written N times."

## 2. The observation that fixes it

Look at what the two paths actually call:

| the store path calls      | Dustin's path calls                       |
|---------------------------|-------------------------------------------|
| `store.CloseIssue(...)`   | `uow.IssueUseCase().CloseIssue(...)`       |
| `store.SearchIssues(...)` | `uow.IssueUseCase().SearchIssues(...)`     |
| `store.AddDependency(...)`| `uow.DependencyUseCase().AddDependency(...)`|
| `store.GetConfig(...)`    | `uow.ConfigUseCase().GetConfig(...)`       |

These are **the same operations with the same names**, reached through different plumbing.
The use-case layer was largely shaped after the store's semantics (it had to be — both must
behave identically, which is what the Seam-A parity tests check).

So instead of teaching every *command* about both plumbing systems, teach the **new plumbing
to present the old interface** — one adapter type, written once:

```go
// uowStore implements storage.Storage over a UnitOfWorkProvider.
// Each call = one short transaction (exactly the proxied-server design);
// RunInTransaction = one UOW spanning the callback.
type uowStore struct {
    provider uow.UnitOfWorkProvider
}

func (s *uowStore) CloseIssue(ctx context.Context, id, reason string, actor string) error {
    u, err := s.provider.NewUOW(ctx)
    if err != nil { return err }
    defer u.Close(ctx)                       // rollback unless committed
    if err := u.IssueUseCase().CloseIssue(ctx, id, reason, actor); err != nil {
        return err
    }
    return u.Commit(ctx, "bd close "+id)     // §5: message per WriteLifecycle contract
}

func (s *uowStore) SearchIssues(ctx context.Context, q types.Query) ([]*types.Issue, error) {
    u, err := s.provider.NewUOW(ctx)
    if err != nil { return nil, err }
    defer u.Close(ctx)
    res, err := u.IssueUseCase().SearchIssues(ctx, q)
    if err != nil { return nil, err }
    u.Close(ctx)                             // read-only: rollback, never Commit("")
    return res, nil                          // (red-team fix: a committed read must be a true no-op)
}

// ... one thin method per core Storage method; see §4 for the full mapping.
```

Then `store_factory.go` gains one arm:

```go
case cfg.IsDoltProxiedServerMode():
    return storage.NewUOWStore(newProxiedServerUOWProvider(...)), nil
```

…and **`bd close` needs no `close_proxied_server.go` at all.** The command calls
`store.CloseIssue` like it always did; in proxied mode that call happens to travel through a
short transaction to the shared server. All 13 dual files collapse; the other ~54 commands
work in proxied mode **without anyone converting them** (modulo the gaps in §4 — that's the
real work).

## 3. What each party gets

**The pluggable-backend project gets:** one seam. `storage.Storage` is what a backend
implements, full stop. Capability gating (`bd history`, `bd dolt push`) hangs off the store
instance as designed. SQLite/Postgres implement the same interface the CLI already consumes.

**Dustin's workstream gets:**
- The concurrency architecture ships to **every command at once** instead of one PR each.
- The `*_proxied_server.go` treadmill and the double-maintenance/parity-test burden end.
- The domain repositories become MORE valuable, not less: a future Postgres backend can
  potentially reuse them wholesale — `db.Runner` is 3 methods any `database/sql` driver
  satisfies; the repos are the SQL; only dialect diverges. His layer becomes the reference
  internal architecture for server-topology SQL backends, rather than a rival seam that has
  to win command-by-command.

**Users get:** proxied-server stops being a 13-command mode and becomes a full-surface Dolt
topology — likely lifting its init gate much sooner.

## 4. The honest part: the mapping and its gaps

Grounded in the actual interfaces (`internal/storage/uow/uow.go`,
`internal/storage/domain/{issue,dependency,label,comment,config}.go` vs
`internal/storage/storage.go`). Disposition legend:
**DIRECT** = 1:1 use-case call · **COMPOSE** = adapter combines 2+ use-case calls ·
**FALLBACK** = legal degraded implementation · **GAP** = nothing on the uow path yet.

### 4.1 Issues (core Storage → IssueUseCase) — nearly all DIRECT
`CreateIssue(s)`, `GetIssue`, `GetIssuesByIDs`, `UpdateIssue`, `ReopenIssue`, `CloseIssue`,
`DeleteIssue`, `SearchIssues(WithCounts)`, `GetReadyWork(WithCounts)`, `GetBlockedIssues`,
`GetStatistics` → same-name methods. `ClaimIssue`, `ClaimReadyIssue`, `DeleteIssues` (from the
folded BulkIssueStore) → same-name methods. The full wisp family exists in parallel
(`CreateWisp`…`ClaimReadyWisp`) covering the wisp halves of these ops.
**Events note:** the use-cases carry an `EventsSQLRepository` internally
(`domain/issue.go:268-292`) and record events inside the same transaction — the §2.3
same-tx rule of the main proposal is already honored on this path for issue mutations.
GAP-to-verify: `GetIssueByExternalRef`, `UpdateIssueType`, `GetEpicsEligibleForClosure`,
`GetNextChildID`, `PromoteFromEphemeral` (may map to `ApplyUpdate`/search filters; needs a
per-signature check).

### 4.2 Dependencies (→ DependencyUseCase) — DIRECT/COMPOSE
`AddDependency`, `RemoveDependency`, `GetDependencyTree`, `IsBlocked`, `DetectCycles`,
`GetBlockingInfo`, `GetNewlyUnblockedByClose` (lives on IssueUseCase) → DIRECT.
`GetDependencies/GetDependents(WithMetadata)` → COMPOSE over
`ListByIssueIDs`/`GetForIssueIDs`/`ListWithIssueMetadata` (direction check per signature).
`CountDependents/CountDependencies` → `CountByIssueID`/`CountsByIssueIDs`. Wisp variants exist.

### 4.3 Labels, comments, config
Labels → DIRECT (`AddLabel`, `RemoveLabel`, `GetLabels`, bulk `GetLabelsForIssues`, wisp
variants). GAP-to-verify: `GetIssuesByLabel` (no obvious use-case method; likely a
`SearchIssues` filter).
Config → DIRECT (`Get/Set/DeleteConfig`, `GetAllConfig`, plus the folded ConfigMetadataStore
methods: `GetCustomTypes/Statuses`, `GetInfraTypes`).
**Comments — asymmetric:** reads are DIRECT (`GetCommentsForIssue(s)`, counts, iterators, wisp
variants) but **comment WRITES are a GAP** — `CommentUseCase` has no `Add`; `bd comment` is
not among the 13 converted commands, consistent with this hole. Needs an
`AddComment` use-case method (small: `EventsSQLRepository`-style repo exists).

### 4.4 Events reads — GAP
Core `GetEvents`, `GetAllEventsSince`, `CountEvents`, `IterEvents`, `IterAllEventsSince`
(consumed by `bd audit` and sync flows) have no read-side use-case; the events repo exposes
`Record/DeleteAllForIDs/CountAllForIDs` only. Needs a small `EventsUseCase` (read methods over
the existing repo).

### 4.5 The mechanical rest — FALLBACK or small
- The 9 core `Iter*` methods: **FALLBACK** to `NewSliceIter` over the slice method — exactly
  what the embedded Dolt backend ships today (`embeddeddolt/iter_stubs.go`), so this is
  precedented, not a cheat. (`IterWithIssueMetadata`/`IterCommentsForIssue` even exist
  natively.)
- `Count*` → mostly DIRECT/COMPOSE per above.
- `Close()` → `UnitOfWorkProvider.Close`.
- `RunInTransaction(ctx, msg, fn)` → **the shape actually fits**: open ONE UOW, wrap it in a
  `storage.Transaction`-shaped view (the 24 tx methods delegate to the same use-cases bound to
  that UOW's runner), run `fn`, `Commit(ctx, msg)`. One UOW = one transaction is literally the
  uow design.
- `SetLocalMetadata/GetLocalMetadata` → **GAP with a design smell**: "clone-local,
  dolt-ignored" state. On a SHARED server, "local" is ambiguous anyway — this needs the D5
  (local tables) decision, not just an adapter method.
- `MergeSlot*`/`Slot*` (7) → GAP; bead-backed by design ("expressible in terms of interface
  methods" per `merge_slot.go`), so implementable ABOVE the seam or as one small use-case.
- Folded AdvancedQueryStore (repo mtime, molecule progress, stale) and CompactionStore →
  GAP; small read-mostly repos, same pattern as events.

### 4.6 Gap scoreboard
Of the ~86 core methods (62 + folded neutrals actually consumed): roughly **~60% DIRECT,
~15% COMPOSE, ~10% legal FALLBACK, ~15% GAP** — and the gaps cluster in five small,
well-shaped units: comment-write, events-read, local-metadata (needs D5), slots,
advanced/compaction queries. **The gap list IS the true remaining cost of full proxied
coverage** — currently hidden as "54 unconverted commands," it becomes a finite checklist on
one type. That reframing is the main value of drafting this sketch.

## 5. Two things the adapter does NOT solve by itself

1. **The commit-message/WriteLifecycle protocol.** `uow.Commit(ctx, message)` is
   `CALL DOLT_COMMIT('-Am', ?)` (`uow/doltserver_tx.go:28`) — the same H2 protocol from the
   main proposal, one layer down. The adapter must route messages/suppression through the §4.2
   WriteLifecycle contract, not invent a third convention. (Per-command auto-commit semantics
   on the shared server ARE different from embedded — that behavioral contract needs pinning
   in the Phase 1 harness either way.)
2. **Per-call transaction granularity.** The store path can batch several store calls inside
   one command without a transaction; the adapter turns each call into its own short tx.
   For most commands that is fine (it IS Dustin's design); commands needing multi-call
   atomicity must use `RunInTransaction` — which the harness will surface as divergences if
   any command relies on accidental single-tx behavior today.

## 6. Questions for the uow workstream owner

1. Does the endgame you have in mind keep use-cases as the **command-facing API** (commands
   call use-cases directly, store withers), or are you open to use-cases as the **server
   backend's internal architecture** behind `storage.Storage` (this sketch)? The
   `store_factory.go:51/96/169` TODOs read like the former — is that a settled position or a
   local convenience? (§7 below reconstructs the May 2026 decision those TODOs record, and
   why this sketch is not a re-run of the attempt that was abandoned then.)
2. If the adapter lands, the 13 `*_proxied_server.go` files and their integration tests fold
   back — is there proxied-only behavior in them (e.g. `--offset` paging, `HasMore`) that you
   want PROMOTED to the core interface rather than lost? (`--offset` argues for a core paging
   extension: the spike added `exists_many`/`close_many` for exactly this kind of round-trip
   collapse.)
3. Are the five gap units (§4.6) reasonable use-case additions, or are any of them things you
   deliberately excluded from the domain layer?
4. Would you take Postgres repos as a dialect variation of the existing `domain/db` repos, or
   would you rather new backends implement `storage.Storage` directly and keep `domain/db`
   Dolt-focused?

## 7. Design history: a store-over-the-proxy WAS tried — and why that failure doesn't apply here

Reconstructed from primary sources (git history, PR bodies, the dogfood tracker); no prose
design doc for the pivot exists — the decision is recorded only as the two-line TODO in
`store_factory.go`. Provenance for every claim below is in the commit/PR/issue cited.

### 7.1 What happened (May 2026)

1. **The proxy came first, for lifecycle + connection cost.** PR #3728 (May 5): a
   singleton-per-workspace TCP daemon, flock-elected, pidfile-discovered, idle-timeout
   reaping of the dolt child. It answers two documented pains: nobody owned "who stops the
   server" (owned servers deliberately never stop — `dolt/store.go:82-84`; a recorded
   incident of 45 zombie sql-servers; journal corruption from racing kills, GH#2430), and
   fork-per-command connection cost measured from Gas City at **71 new connections/sec with
   the sql-server at 85–170% CPU doing pure connection setup** (issue #4303; the proxy took
   it from 7.02 → 1.02 dolt connections per invocation).
2. **A `storage.DoltStorage` implementation over that proxy was built FIRST.** PR #3792
   (May 6–7): `internal/storage/doltserver.DoltServerStore` — "constructor + schema
   bootstrap are wired; CRUD methods are stubbed." It reached 805 lines with **128 of 132
   methods as `panic("unimplemented")`**.
3. **Pivoted in place four days later.** Commit cb04e03fd (May 9) replaced the factory call
   sites with `// TODO: this should not be a store // it should be a uow provider`; commit
   3d7173c3c (same day) created `internal/storage/uow/` + `internal/storage/domain/db/` and
   git-renamed the store's test file into `uow/`; commit 18a93ec56 (May 11) deleted the
   store carcass. That TODO is the entire written rationale.

### 7.2 Why the store route died (reconstructed, each point evidenced)

- **The god interface cannot be implemented incrementally.** All ~168 methods must exist
  before ONE command is safe, because the global `store` can call any of them — a partial
  implementation is a panic minefield, which is literally what the deleted store was. The
  UOW shape ships a working vertical slice per command (the observed `db/*` PR cadence).
- **`DoltStore`'s semantics are method-scoped, which is data-loss under concurrency.** N
  store calls = N `DOLT_COMMIT`s and non-atomic read-modify-write; safe under embedded's
  exclusive flock, unsafe on a shared server (issue #3822, filed the pivot week: stale
  snapshot import "losing any writes… embedded mode happens to avoid this because it gets an
  exclusive write lock"). Plus session-pinned state (GH#2455: commit must run on the same
  Dolt session; pooled connections have independent working sets) and a dual-transaction
  wisp dance opening a second `*sql.DB` per write.
- **Command-scoped short transactions are the right concurrency unit.** One UOW per command
  = one pinned conn, one `DOLT_COMMIT('-Am', msg)`, minimal lock-hold for other agents,
  phase-aware retry (pre-commit replays safely; failure AT commit → `ErrCommitIndeterminate`
  rather than double-apply, `uow/run_in_tx.go:15-35`). Validation: the legacy `DoltStore`
  **ported this design back** in June (#4462).
- **The dual command files are a deliberate dark-launch isolation contract** (TLS/auth
  "decision-pending", Steve Yegge 1425e7338; every PR body repeats "the non-proxied path is
  untouched"), not an architectural end-state anyone wrote down. The strangler-fig reading
  (`proxied_server.go:212`: "the global uow provider used by all commands") is the only
  hint at the intended end-state, and it is a TODO, not a decision.

### 7.3 Why the adapter is not a re-run of the May failure

The May attempt and this sketch differ in the one dimension that killed the former:

| | `DoltServerStore` (May, died) | `uowStore` (this sketch) |
|---|---|---|
| Implements the interface **from** | scratch — raw SQL, nothing existed | six weeks of shipped, parity-tested use-cases |
| Incremental? | no — 168-method panic wall before one command works | yes — ~60% DIRECT today, gaps are 5 small named units (§4.6) |
| Transaction semantics | inherited DoltStore's per-method commits | inherits the UOW's per-call short tx — the semantics the pivot chose |
| Relationship to the uow stack | competed with it | is completed BY it |

In other words: **the reason the store route failed in May (no incrementally-buildable
substrate) was removed by the uow workstream itself.** The adapter is the store route
finished via Dustin's work, not instead of it — and the main proposal's Phase 2 (shrink the
god interface, capability-gate the 5 Dolt-shaped sub-interfaces) removes the very
structural cause of the May failure for every future backend.

One governance note: the design conversation for the pivot appears to have happened
off-repo (empty commit bodies and PR templates on the foundational PRs), and the project
has since adopted maintainer guidelines citing a 440-PR audit about merges that hid their
contents. The D1 conversation should be synchronous and recorded — this document is an
input to it, not a substitute for it.
