package uowstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/internal/types"
)

// uowTransaction is a storage.Transaction view bound to ONE open
// uow.UnitOfWork. It is only valid inside the RunInTransaction callback; the
// underlying pinned connection is released when the UOW commits or rolls back,
// so callers must not retain it (same hazard as the embedded *sql.Tx view).
//
// The embedded unsupportedTransaction shell returns typed *storage.ErrUnsupported
// for the Transaction methods this spike does not implement; the ones overridden
// below (GetIssue, CloseIssue, AddDependency plus the delete-command trio
// UpdateIssue, RemoveDependency, DeleteIssue) are the read-check-act set that
// proves multi-statement atomicity across the issue + dependency tables. Because
// a stubbed method's error propagates out of fn, calling one also exercises the
// rollback-on-domain-error path.
type uowTransaction struct {
	unsupportedTransaction // generated: the non-overridden methods return typed ErrUnsupported

	u uow.UnitOfWork
	// storeActor is the enclosing store's construction-time audit identity,
	// threaded here so the actor-less tx methods (DeleteIssue) have a §4.0
	// identity on the seam. Set in RunInTransaction — never leave it empty.
	storeActor string
}

var _ storage.Transaction = (*uowTransaction)(nil)

// GetIssue reads through the same shared helper as (*uowStore).GetIssue, so
// read-your-writes inside the transaction and the storage.ErrNotFound
// translation behave identically to the non-transactional path.
func (t *uowTransaction) GetIssue(ctx context.Context, id string) (*types.Issue, error) {
	issue, err := getIssueInUOW(ctx, t.u, id)
	return issue, mapUowError(err)
}

// CloseIssue mutates through the shared close helper (issue-vs-wisp probe →
// CloseIssue/CloseWisp). The is_blocked recompute fires inside this same tx.
func (t *uowTransaction) CloseIssue(ctx context.Context, id, reason, actor, session string) error {
	return mapUowError(closeIssueInUOW(ctx, t.u, id, reason, actor, session))
}

// AddDependency routes wisp-vs-issue via the shared addDependencyInUOW helper
// (same body as the store-level AddDependency); the use-case runs its
// blocking-cycle check in the same tx.
func (t *uowTransaction) AddDependency(ctx context.Context, dep *types.Dependency, actor string) error {
	return mapUowError(addDependencyInUOW(ctx, t.u, dep, actor))
}

// UpdateIssue rewrites text references on a surviving neighbor inside the
// delete tx (cmd/bd/delete.go). Same whitelisted-column semantics as the
// store-level UpdateIssue, routed issue-vs-wisp.
func (t *uowTransaction) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	isWisp, err := isWispInUOW(ctx, t.u, id)
	if err != nil {
		return mapUowError(err)
	}
	if isWisp {
		err = t.u.IssueUseCase().UpdateWisp(ctx, id, updates, actor)
	} else {
		err = t.u.IssueUseCase().UpdateIssue(ctx, id, updates, actor)
	}
	return mapUowError(err)
}

// RemoveDependency tears down an edge inside the delete tx; the domain Delete
// recomputes is_blocked for the surviving neighbors in the same tx (the `del`
// scenario's blocked -> ready assertion rides this recompute).
func (t *uowTransaction) RemoveDependency(ctx context.Context, issueID, dependsOnID, actor string) error {
	return mapUowError(removeDependencyInUOW(ctx, t.u, issueID, dependsOnID, actor))
}

// DeleteIssue removes the issue row inside the delete tx. delete.go tears down
// every edge touching id via RemoveDependency BEFORE this runs, so the cascade
// delete's FindAllDependents resolves to {id} alone — no unintended cascade.
func (t *uowTransaction) DeleteIssue(ctx context.Context, id string) error {
	return mapUowError(deleteIssueInUOW(ctx, t.u, id, t.actor()))
}

// actor resolves the audit identity for the tx-view's actor-less store methods
// (Transaction.DeleteIssue). It reads the enclosing store's construction-time
// actor, threaded onto the view at construction (RunInTransaction below).
func (t *uowTransaction) actor() string {
	return t.storeActor
}

// RunInTransaction implements storage.Storage.RunInTransaction over ONE
// uow.UnitOfWork: fn's mutations share one pinned connection/transaction and
// become exactly ONE DOLT_COMMIT carrying commitMsg (outcome-derived by the
// caller, per §4.0: the description travels on Commit).
//
// Retry semantics are inherited from uow.RunInTx and are NOT reimplemented here
// (phase-aware):
//
//   - Pre-commit transients (NewUOW, a connection pin, or a serialization
//     failure raised by fn before COMMIT) replay the whole sequence with a
//     FRESH UnitOfWork and a FRESH Transaction view; the server already rolled
//     the prior attempt back.
//   - Domain errors returned by fn (validation, not-found, and a tx stub's
//     *storage.ErrUnsupported) are permanent — no retry — and the deferred
//     Close rolls back.
//   - A connection loss AT or AFTER commit surfaces uow.ErrCommitIndeterminate
//     and is NEVER retried (double-apply risk); the caller must reconcile by
//     re-reading. "nothing to commit" (a read-only fn) maps to success with no
//     new version commit.
//
// fn must therefore be replay-safe: no external side effects, and no state
// captured from a previous attempt's view (the view is constructed INSIDE the
// retry closure below — never hoist it).
func (s *uowStore) RunInTransaction(ctx context.Context, commitMsg string, fn func(tx storage.Transaction) error) error {
	if strings.TrimSpace(commitMsg) == "" {
		// Embedded semantics for "" are "SQL-commit, defer the version commit"
		// (embeddeddolt RunInTransaction), but the uow Tx has no
		// commit-without-DOLT_COMMIT primitive and DOLT_COMMIT rejects empty
		// messages. This is unreachable from the CLI on the spike path
		// (transactHonoringAutoCommit only blanks the message in embedded mode,
		// dolt_autocommit.go:31), so refuse loudly instead of diverging version
		// history.
		return fmt.Errorf("uowstore spike: RunInTransaction requires a non-empty commit message; deferred (blank-message) version commits are embedded-only (gastownhall/beads#4547)")
	}
	return uow.RunInTx(ctx, s.provider, commitMsg, func(u uow.UnitOfWork) error {
		return fn(&uowTransaction{u: u, storeActor: s.actor}) // view constructed INSIDE the retry closure — never hoist
	})
}
