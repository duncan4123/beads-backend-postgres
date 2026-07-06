//go:build cgo

// DERISK SPIKE test for gastownhall/beads#4547 Route A, slice 2 (Part A):
// storage.Storage.RunInTransaction mapped onto ONE uow.UnitOfWork. It drives the
// spike store's tx view through a real managed proxy (child dolt sql-server) and
// pins the transactional contract the store-per-call path CANNOT provide:
//
//	T1 rollback atomicity  — fn error rolls BOTH mutations back; a second store
//	                         handle sees the pre-tx state; dolt_log head unchanged.
//	T2 commit-once         — a successful fn produces EXACTLY ONE dolt commit
//	                         whose message is byte-equal to the caller's commitMsg,
//	                         with both mutations visible via the second handle.
//	T3 typed-unsupported   — a Transaction stub error inside fn behaves as a domain
//	                         error: no retry, full rollback.
//	T4 read-only fn        — no version commit ("nothing to commit" -> success).
//	T5 CLI clean error     — bd dep add on the spike path exits nonzero with the
//	                         typed ErrUnsupported text and NO panic/goroutine output.
//
// NOT runtime-tested here, by design: serialization replay and
// ErrCommitIndeterminate need fault injection the proxied stack does not expose.
// Their correctness is by construction — RunInTransaction is a single delegation
// to uow.RunInTx — and is covered by internal/storage/uow/run_in_tx_test.go, which
// must still pass (go test ./internal/storage/uow -count=1).
package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/uowstore"
	"github.com/steveyegge/beads/internal/types"
)

const spikeTxActor = "spiketester"

// spikeTxStores opens two independent in-process spike store handles against the
// managed proxy the workspace already booted (a flock-elected singleton, so both
// providers attach to the same server). st1 drives the transaction under test;
// st2 is the "second store handle" that observes isolation/visibility.
func spikeTxStores(t *testing.T, proj proxiedProject) (st1, st2 storage.DoltStorage) {
	t.Helper()
	ctx := context.Background()
	// Pin root resolution to the workspace's proxy root (proxied_server.go:49).
	t.Setenv("BEADS_PROXIED_SERVER_ROOT_PATH", proj.proxyRoot)

	p1, err := newProxiedServerUOWProvider(ctx, proj.beadsDir)
	if err != nil {
		t.Fatalf("open provider 1: %v", err)
	}
	st1 = uowstore.New(p1, spikeTxActor)
	t.Cleanup(func() { _ = st1.Close() })

	p2, err := newProxiedServerUOWProvider(ctx, proj.beadsDir)
	if err != nil {
		t.Fatalf("open provider 2: %v", err)
	}
	st2 = uowstore.New(p2, spikeTxActor)
	t.Cleanup(func() { _ = st2.Close() })
	return st1, st2
}

// spikeSeedIssue creates a top-level issue through the spike store (minting the
// ID in place) and returns its minted ID.
func spikeSeedIssue(t *testing.T, st storage.DoltStorage, title string) string {
	t.Helper()
	issue := &types.Issue{Title: title, IssueType: types.IssueType("task"), Priority: 1}
	if err := st.CreateIssue(context.Background(), issue, spikeTxActor); err != nil {
		t.Fatalf("seed issue %q: %v", title, err)
	}
	if issue.ID == "" {
		t.Fatalf("seed issue %q: minted ID is empty", title)
	}
	return issue.ID
}

func spikeStoreStatus(t *testing.T, st storage.DoltStorage, id string) types.Status {
	t.Helper()
	issue, err := st.GetIssue(context.Background(), id)
	if err != nil {
		t.Fatalf("GetIssue(%s): %v", id, err)
	}
	return issue.Status
}

func spikeStoreCountDeps(t *testing.T, st storage.DoltStorage, id string) int64 {
	t.Helper()
	n, err := st.CountDependencies(context.Background(), id)
	if err != nil {
		t.Fatalf("CountDependencies(%s): %v", id, err)
	}
	return n
}

func TestSpikeUOWStore_TxRollbackAtomicity(t *testing.T) {
	requireProxiedServerEnv(t)
	bd := buildEmbeddedBD(t)
	_, _, proj := setupSpikeProxiedWorkspace(t, bd, "spiketx1")
	st1, st2 := spikeTxStores(t, proj)
	ctx := context.Background()
	db := openProxiedDB(t, proj)

	a := spikeSeedIssue(t, st1, "issue A")
	b := spikeSeedIssue(t, st1, "issue B")
	headBefore := proxiedDoltHead(t, db)

	errSentinel := errors.New("spike sentinel: roll back")
	err := st1.RunInTransaction(ctx, "bd: spike tx", func(tx storage.Transaction) error {
		if s := spikeTxStatus(t, tx, a); s != types.StatusOpen {
			t.Fatalf("in-tx A status = %q, want open", s)
		}
		if err := tx.CloseIssue(ctx, a, "done", spikeTxActor, ""); err != nil {
			return err
		}
		// Read-your-writes: the close is visible inside the tx.
		if s := spikeTxStatus(t, tx, a); s != types.StatusClosed {
			t.Fatalf("in-tx A status after close = %q, want closed", s)
		}
		// Isolation: a second store handle must NOT see the uncommitted close.
		if s := spikeStoreStatus(t, st2, a); s != types.StatusOpen {
			t.Fatalf("second handle saw uncommitted close: A status = %q, want open", s)
		}
		if err := tx.AddDependency(ctx, &types.Dependency{IssueID: b, DependsOnID: a, Type: types.DepBlocks}, spikeTxActor); err != nil {
			return err
		}
		return errSentinel
	})

	if !errors.Is(err, errSentinel) {
		t.Fatalf("RunInTransaction error = %v, want sentinel", err)
	}
	if s := spikeStoreStatus(t, st2, a); s != types.StatusOpen {
		t.Errorf("after rollback A status = %q, want open (mutation rolled back)", s)
	}
	if n := spikeStoreCountDeps(t, st2, b); n != 0 {
		t.Errorf("after rollback B dep count = %d, want 0 (mutation rolled back)", n)
	}
	if n := proxiedDoltCommitCountSince(t, db, headBefore); n != 0 {
		t.Errorf("after rollback new dolt commits = %d, want 0", n)
	}
}

func TestSpikeUOWStore_TxCommitOnce(t *testing.T) {
	requireProxiedServerEnv(t)
	bd := buildEmbeddedBD(t)
	_, _, proj := setupSpikeProxiedWorkspace(t, bd, "spiketx2")
	st1, st2 := spikeTxStores(t, proj)
	ctx := context.Background()
	db := openProxiedDB(t, proj)

	a := spikeSeedIssue(t, st1, "issue A")
	b := spikeSeedIssue(t, st1, "issue B")
	headBefore := proxiedDoltHead(t, db)

	const commitMsg = "bd: spike tx"
	err := st1.RunInTransaction(ctx, commitMsg, func(tx storage.Transaction) error {
		if err := tx.CloseIssue(ctx, a, "done", spikeTxActor, ""); err != nil {
			return err
		}
		return tx.AddDependency(ctx, &types.Dependency{IssueID: b, DependsOnID: a, Type: types.DepBlocks}, spikeTxActor)
	})
	if err != nil {
		t.Fatalf("RunInTransaction returned %v, want nil", err)
	}

	if s := spikeStoreStatus(t, st2, a); s != types.StatusClosed {
		t.Errorf("after commit A status = %q, want closed", s)
	}
	if n := spikeStoreCountDeps(t, st2, b); n != 1 {
		t.Errorf("after commit B dep count = %d, want 1", n)
	}
	assertProxiedDepExists(t, db, b, a)

	// Commit-once: exactly ONE new dolt commit between the captured head and the
	// new head, and its message is byte-equal to the caller's commitMsg. A
	// stdout assertion cannot see N-commits-instead-of-one, which is the
	// §6.3/red-team divergence class this test exists to close.
	if n := proxiedDoltCommitCountSince(t, db, headBefore); n != 1 {
		t.Fatalf("new dolt commits = %d, want exactly 1 (commit-once)", n)
	}
	if msg := readDoltLogTopMessage(t, db); msg != commitMsg {
		t.Errorf("commit message = %q, want byte-equal %q (verbatim outcome-derived msg)", msg, commitMsg)
	}
}

func TestSpikeUOWStore_TxUnsupportedRollsBack(t *testing.T) {
	requireProxiedServerEnv(t)
	bd := buildEmbeddedBD(t)
	_, _, proj := setupSpikeProxiedWorkspace(t, bd, "spiketx3")
	st1, st2 := spikeTxStores(t, proj)
	ctx := context.Background()
	db := openProxiedDB(t, proj)

	c := spikeSeedIssue(t, st1, "issue C")
	headBefore := proxiedDoltHead(t, db)

	err := st1.RunInTransaction(ctx, "bd: spike tx", func(tx storage.Transaction) error {
		if err := tx.CloseIssue(ctx, c, "done", spikeTxActor, ""); err != nil {
			return err
		}
		// CreateIssues is a generated typed-unsupported Transaction stub.
		return tx.CreateIssues(ctx, nil, spikeTxActor)
	})

	var target *storage.ErrUnsupported
	if !errors.As(err, &target) {
		t.Fatalf("RunInTransaction error = %v, want *storage.ErrUnsupported", err)
	}
	if target.Op != "Transaction.CreateIssues" || target.Backend != uowstore.SpikeBackend {
		t.Errorf("ErrUnsupported = {Op:%q, Backend:%q}, want {Transaction.CreateIssues, %q}",
			target.Op, target.Backend, uowstore.SpikeBackend)
	}
	if s := spikeStoreStatus(t, st2, c); s != types.StatusOpen {
		t.Errorf("after unsupported-error rollback C status = %q, want open", s)
	}
	if n := proxiedDoltCommitCountSince(t, db, headBefore); n != 0 {
		t.Errorf("after unsupported-error rollback new dolt commits = %d, want 0", n)
	}
}

func TestSpikeUOWStore_TxReadOnlyFnNoCommit(t *testing.T) {
	requireProxiedServerEnv(t)
	bd := buildEmbeddedBD(t)
	_, _, proj := setupSpikeProxiedWorkspace(t, bd, "spiketx4")
	st1, _ := spikeTxStores(t, proj)
	ctx := context.Background()
	db := openProxiedDB(t, proj)

	a := spikeSeedIssue(t, st1, "issue A")
	headBefore := proxiedDoltHead(t, db)

	err := st1.RunInTransaction(ctx, "bd: spike read-only tx", func(tx storage.Transaction) error {
		if _, err := tx.GetIssue(ctx, a); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read-only RunInTransaction returned %v, want nil", err)
	}
	if n := proxiedDoltCommitCountSince(t, db, headBefore); n != 0 {
		t.Errorf("read-only fn created %d dolt commits, want 0 (nothing-to-commit -> success)", n)
	}
}

func TestSpikeUOWStore_CLIUnsupportedIsCleanError(t *testing.T) {
	requireProxiedServerEnv(t)
	bd := buildEmbeddedBD(t)
	spikeDir, spikeEnv, _ := setupSpikeProxiedWorkspace(t, bd, "spiketx5")

	// Two real issues via the CLI (store path: CreateIssue is implemented).
	xOut, se, err := spikeRun(t, bd, spikeDir, spikeEnv, "create", "X issue", "--json", "-t", "task")
	if err != nil {
		t.Fatalf("bd create X failed: %v\n%s", err, se)
	}
	yOut, se, err := spikeRun(t, bd, spikeDir, spikeEnv, "create", "Y issue", "--json", "-t", "task")
	if err != nil {
		t.Fatalf("bd create Y failed: %v\n%s", err, se)
	}
	x := spikeFirstJSONID(t, xOut)
	y := spikeFirstJSONID(t, yOut)

	// Slice 3 implemented store-level AddDependency (dep.go:378 path), so bd dep
	// add now SUCCEEDS through the spike store instead of returning the typed
	// unsupported error it did in Slice 2.
	if stdout, stderr, addErr := spikeRun(t, bd, spikeDir, spikeEnv, "dep", "add", x, y); addErr != nil {
		t.Fatalf("bd dep add exited nonzero, want success (AddDependency implemented in Slice 3): %v\n%s%s", addErr, stdout, stderr)
	}

	// The clean-error contract now rides a method the census leaves genuinely
	// unsupported: count --by-status -> store.CountIssuesByGroup (no domain
	// cardinality-by-group seam). It must surface the typed ErrUnsupported text,
	// exit nonzero, and never panic or dump a goroutine trace.
	stdout, stderr, err := spikeRun(t, bd, spikeDir, spikeEnv, "count", "--by-status")
	combined := stdout + stderr
	if exitCode(err) == 0 {
		t.Fatalf("bd count --group-by exited 0, want nonzero (unsupported)\noutput:\n%s", combined)
	}
	if !strings.Contains(combined, `operation "CountIssuesByGroup" not supported by the uowstore spike backend`) {
		t.Errorf("output missing typed ErrUnsupported text:\n%s", combined)
	}
	for _, bad := range []string{"panic:", "goroutine ", "runtime error"} {
		if strings.Contains(combined, bad) {
			t.Errorf("output contains %q (want clean error, no crash):\n%s", bad, combined)
		}
	}
}

// TestSpikeUOWStore_IsInfraTypeCtxRoutes is the tripwire for the IsInfraTypeCtx
// override (#4547 slice-2 finding 2). IsInfraTypeCtx has no error channel, so the
// generated shell can only return the zero value (false) — it CANNOT return a
// typed ErrUnsupported. Left to the shell, an infra type would silently answer
// false, the exact silent-divergence class the spike exists to surface, and it is
// reachable under BD_SPIKE_UOWSTORE=1 from wisp.go's infra-type gating.
//
// The override must therefore route to the use-case. This test proves it: seed
// types.infra directly in the config table, then a real override returns true for
// a configured infra type while the shell stub would still return false. The
// negative case (a non-configured type) guards against the override answering
// true unconditionally.
func TestSpikeUOWStore_IsInfraTypeCtxRoutes(t *testing.T) {
	requireProxiedServerEnv(t)
	bd := buildEmbeddedBD(t)
	_, _, proj := setupSpikeProxiedWorkspace(t, bd, "spikeinfra")
	st1, _ := spikeTxStores(t, proj)
	ctx := context.Background()

	// Seed types.infra in the config table via the proxied DB. GetInfraTypes
	// returns an empty map when the key is unset (domain/db/config.go:287), so
	// without this seed the override and the stub both return false and would be
	// indistinguishable — the seed is what makes this a real tripwire.
	db := openProxiedDB(t, proj)
	if _, err := db.ExecContext(ctx,
		"REPLACE INTO config (`key`, value) VALUES (?, ?)", "types.infra", "agent,role,message"); err != nil {
		t.Fatalf("seed types.infra: %v", err)
	}

	if !st1.IsInfraTypeCtx(ctx, types.IssueType("agent")) {
		t.Error("IsInfraTypeCtx(agent) = false; the generated stub answered instead of the use-case override")
	}
	if st1.IsInfraTypeCtx(ctx, types.IssueType("task")) {
		t.Error("IsInfraTypeCtx(task) = true; want false for a non-infra type")
	}
}

// mustTxStatus reads an issue's status through the transaction view.
func spikeTxStatus(t *testing.T, tx storage.Transaction, id string) types.Status {
	t.Helper()
	issue, err := tx.GetIssue(context.Background(), id)
	if err != nil {
		t.Fatalf("tx.GetIssue(%s): %v", id, err)
	}
	return issue.Status
}

// firstJSONID pulls the "id" field from the first JSON object in bd output.
func spikeFirstJSONID(t *testing.T, out string) string {
	t.Helper()
	obj := jsonObject(t, out)
	id, _ := obj["id"].(string)
	if id == "" {
		t.Fatalf("no id in bd output:\n%s", out)
	}
	return id
}
