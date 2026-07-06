package uowstore

import (
	"context"
	"fmt"

	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/internal/types"
)

// This file completes the mutating half of the gc-16 surface (SPIKE-CENSUS-GC16
// §MISSING-methods rollup). Every method opens ONE short unit-of-work via
// uow.RunInTxMsg — the same one-UOW-per-call shape as CreateIssue/CloseIssue in
// store.go — with an outcome-derived commit message so each store call becomes
// exactly one DOLT_COMMIT (§4.0: the description travels on Commit). Reads that
// back mutations (issue-vs-wisp routing) run inside the same tx. Every outgoing
// error is routed through mapUowError so the not-found contract is uniform.

// UpdateIssue applies whitelisted column writes through the matching use-case.
// cmd/bd/update.go strips the structured operations — add/remove/set-labels,
// --parent reparent, --claim, incremental metadata edits — into their OWN store
// calls (AddLabel/RemoveDependency/AddDependency/ClaimIssue) BEFORE the store's
// UpdateIssue is reached, so the updates map that lands here is plain column
// writes (status/priority/assignee/metadata/defer_until/…). The domain layer
// whitelists those columns and rejects anything else LOUDLY (db: Update: field
// %q is not allowed), so a stray structured key surfaces as an error rather
// than a silent no-op — the divergence class the spike exists to catch.
func (s *uowStore) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	return mapUowError(uow.RunInTxMsg(ctx, s.provider, func(u uow.UnitOfWork) (string, error) {
		isWisp, err := isWispInUOW(ctx, u, id)
		if err != nil {
			return "", err
		}
		if isWisp {
			err = u.IssueUseCase().UpdateWisp(ctx, id, updates, actor)
		} else {
			err = u.IssueUseCase().UpdateIssue(ctx, id, updates, actor)
		}
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("bd: update %s", id), nil
	}))
}

// ReopenIssue flips a closed issue back to open. The embedded store also records
// a non-empty reason as a comment; the domain ReopenIssueParams carries the
// reason into the row/event but this spike does NOT additionally write a comment
// (the reopen corpus scenario passes no reason). A full adapter that must
// round-trip the reopen-reason comment needs the CommentUseCase write path —
// SPIKE-REPORT §3 comment-write gap.
func (s *uowStore) ReopenIssue(ctx context.Context, id string, reason string, actor string) error {
	return mapUowError(uow.RunInTxMsg(ctx, s.provider, func(u uow.UnitOfWork) (string, error) {
		isWisp, err := isWispInUOW(ctx, u, id)
		if err != nil {
			return "", err
		}
		params := domain.ReopenIssueParams{Reason: reason}
		if isWisp {
			_, err = u.IssueUseCase().ReopenWisp(ctx, id, params, actor)
		} else {
			_, err = u.IssueUseCase().ReopenIssue(ctx, id, params, actor)
		}
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("bd: reopen %s", id), nil
	}))
}

// UpdateIssueType is a thin wrapper over UpdateIssue, mirroring the embedded
// store (issues.go). issue_type is a whitelisted update column, so it travels
// the same path with the domain layer's custom-type validation.
func (s *uowStore) UpdateIssueType(ctx context.Context, id string, issueType string, actor string) error {
	return s.UpdateIssue(ctx, id, map[string]interface{}{"issue_type": issueType}, actor)
}

// ClaimIssue routes issue-vs-wisp then compare-and-swap claims through the
// use-case. The domain surfaces storage.ErrAlreadyClaimed / ErrNotClaimable as
// domain errors (no retry).
func (s *uowStore) ClaimIssue(ctx context.Context, id string, actor string) error {
	return mapUowError(uow.RunInTxMsg(ctx, s.provider, func(u uow.UnitOfWork) (string, error) {
		isWisp, err := isWispInUOW(ctx, u, id)
		if err != nil {
			return "", err
		}
		if isWisp {
			_, err = u.IssueUseCase().ClaimWisp(ctx, id, actor)
		} else {
			_, err = u.IssueUseCase().ClaimIssue(ctx, id, actor)
		}
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("bd: claim %s", id), nil
	}))
}

// ClaimReadyIssue claims the first ready durable issue matching filter (the
// store contract is issues-table only; the wisp claim variant is a separate
// use-case method not on this seam). When nothing is claimable the closure
// makes no write, so the commit collapses to "nothing to commit" -> success and
// (nil, nil) is returned, matching the embedded store.
func (s *uowStore) ClaimReadyIssue(ctx context.Context, filter types.WorkFilter, actor string) (*types.Issue, error) {
	var claimed *types.Issue
	err := uow.RunInTxMsg(ctx, s.provider, func(u uow.UnitOfWork) (string, error) {
		res, err := u.IssueUseCase().ClaimReadyIssue(ctx, filter, actor)
		if err != nil {
			return "", err
		}
		if res.Issue == nil {
			// No write happened; a benign message is fine — the tx has no
			// changes, so Commit short-circuits on "nothing to commit".
			return "bd: claim-ready (none)", nil
		}
		claimed = res.Issue
		return fmt.Sprintf("bd: claim-ready %s", res.Issue.ID), nil
	})
	return claimed, mapUowError(err)
}

// AddDependency adds one edge (store-level, non-tx: dep.go's fromStore.AddDependency
// path). The wisp routing + blocking-cycle check live in the shared
// addDependencyInUOW helper, reused by the Transaction view.
func (s *uowStore) AddDependency(ctx context.Context, dep *types.Dependency, actor string) error {
	if dep == nil {
		return fmt.Errorf("dependency must not be nil")
	}
	return mapUowError(uow.RunInTxMsg(ctx, s.provider, func(u uow.UnitOfWork) (string, error) {
		if err := addDependencyInUOW(ctx, u, dep, actor); err != nil {
			return "", err
		}
		return fmt.Sprintf("bd: dep add %s -> %s", dep.IssueID, dep.DependsOnID), nil
	}))
}

// RemoveDependency tears down one edge. The domain Delete recomputes is_blocked
// for the surviving neighbors (db/dependency.go: AffectedByDepChange +
// RecomputeIsBlocked), which is what drives the `del`/`drm` scenarios' blocked
// -> ready flip.
func (s *uowStore) RemoveDependency(ctx context.Context, issueID, dependsOnID string, actor string) error {
	return mapUowError(uow.RunInTxMsg(ctx, s.provider, func(u uow.UnitOfWork) (string, error) {
		if err := removeDependencyInUOW(ctx, u, issueID, dependsOnID, actor); err != nil {
			return "", err
		}
		return fmt.Sprintf("bd: dep remove %s -> %s", issueID, dependsOnID), nil
	}))
}

// DeleteIssue is the single-issue non-tx path (delete's fallback + the routed
// single delete's own row removal). It uses the construction-time s.actor: the
// store contract's DeleteIssue carries no actor argument, exactly the
// §4.0 identity-on-the-seam case New()'s actor exists for.
func (s *uowStore) DeleteIssue(ctx context.Context, id string) error {
	return mapUowError(uow.RunInTxMsg(ctx, s.provider, func(u uow.UnitOfWork) (string, error) {
		if err := deleteIssueInUOW(ctx, u, id, s.actor); err != nil {
			return "", err
		}
		return fmt.Sprintf("bd: delete %s", id), nil
	}))
}

// DeleteIssues backs both purge paths: --dry-run (dryRun=true, purge.go:176) and
// --force (dryRun=false, purge.go:225), plus batch delete. cascade/force are
// threaded into the domain use-case's embedded-parity policy path
// (EnforceCascadePolicy): a non-cascade delete refuses when a dependent outside
// the deletion set would be stripped (force=false) or orphans it (force=true),
// matching issueops.DeleteIssuesInTx so the store contract can't silently
// cascade-delete durable dependents. UpdateTextReferences is left OFF: the
// embedded store's DeleteIssues does not rewrite references (cmd/bd/delete.go
// does that itself), so the store's ReferencesUpdated stays 0 on both plumbings.
// dry-run runs on a read-only UOW (no commit); a real delete opens a write tx.
func (s *uowStore) DeleteIssues(ctx context.Context, ids []string, cascade bool, force bool, dryRun bool) (*types.DeleteIssuesResult, error) {
	params := domain.DeleteIssuesParams{
		IDs:                  ids,
		DryRun:               dryRun,
		UpdateTextReferences: false,
		EnforceCascadePolicy: true,
		Cascade:              cascade,
		Force:                force,
	}
	if dryRun {
		u, err := s.provider.NewUOW(ctx)
		if err != nil {
			return nil, err
		}
		defer u.Close(ctx)
		res, err := u.IssueUseCase().DeleteIssues(ctx, params, s.actor)
		if err != nil {
			return nil, mapUowError(err)
		}
		return toStoreDeleteResult(res), nil
	}

	var out *types.DeleteIssuesResult
	err := uow.RunInTxMsg(ctx, s.provider, func(u uow.UnitOfWork) (string, error) {
		res, err := u.IssueUseCase().DeleteIssues(ctx, params, s.actor)
		if err != nil {
			return "", err
		}
		out = toStoreDeleteResult(res)
		return fmt.Sprintf("bd: delete %d issue(s)", len(ids)), nil
	})
	if err != nil {
		return nil, mapUowError(err)
	}
	return out, nil
}

// SetConfig persists one config key/value. Load-bearing for the cross-plumbing
// wrapper's `bd config set issue_prefix <p>` workspace seed and the corpus
// config_set_get_success (custom.team) scenario. NOTE: the embedded SetConfig
// additionally syncs the custom_statuses / custom_types normalized tables when
// key is status.custom / types.custom; the domain ConfigUseCase.SetConfig does
// NOT, so custom-status/type config writes would not refresh those tables on
// this path. The corpus config-set keys (issue_prefix, custom.team) need no such
// sync, so this is a documented latent gap, not a corpus blocker.
func (s *uowStore) SetConfig(ctx context.Context, key, value string) error {
	return mapUowError(uow.RunInTxMsg(ctx, s.provider, func(u uow.UnitOfWork) (string, error) {
		if err := u.ConfigUseCase().SetConfig(ctx, key, value); err != nil {
			return "", err
		}
		return fmt.Sprintf("bd: config set %s", key), nil
	}))
}

// DeleteConfig removes one config key (config unset).
func (s *uowStore) DeleteConfig(ctx context.Context, key string) error {
	return mapUowError(uow.RunInTxMsg(ctx, s.provider, func(u uow.UnitOfWork) (string, error) {
		if err := u.ConfigUseCase().DeleteConfig(ctx, key); err != nil {
			return "", err
		}
		return fmt.Sprintf("bd: config unset %s", key), nil
	}))
}

// ---- shared write helpers (package-level so the Transaction view and the
// store methods share ONE wisp-routing/mutation implementation) ----

// addDependencyInUOW mirrors embeddedTransaction's IsActiveWispInTx routing: a
// wisp source uses the wisp-dependency table, otherwise the regular table. The
// use-case runs its blocking-cycle check (depRepo.HasCycle) in the same tx.
func addDependencyInUOW(ctx context.Context, u uow.UnitOfWork, dep *types.Dependency, actor string) error {
	if dep == nil {
		return fmt.Errorf("dependency must not be nil")
	}
	isWisp, err := isWispInUOW(ctx, u, dep.IssueID)
	if err != nil {
		return err
	}
	if isWisp {
		return u.DependencyUseCase().AddWispDependency(ctx, dep, actor)
	}
	return u.DependencyUseCase().AddDependency(ctx, dep, actor)
}

// removeDependencyInUOW routes edge teardown by the source's table, matching
// addDependencyInUOW. Shared by (*uowStore).RemoveDependency and the tx view.
func removeDependencyInUOW(ctx context.Context, u uow.UnitOfWork, issueID, dependsOnID, actor string) error {
	isWisp, err := isWispInUOW(ctx, u, issueID)
	if err != nil {
		return err
	}
	if isWisp {
		return u.DependencyUseCase().RemoveWispDependency(ctx, issueID, dependsOnID, actor)
	}
	return u.DependencyUseCase().RemoveDependency(ctx, issueID, dependsOnID, actor)
}

// deleteIssueInUOW routes issue-vs-wisp then delegates to the cascade delete
// use-case. Shared by (*uowStore).DeleteIssue and (*uowTransaction).DeleteIssue.
// In the delete-command tx body the caller has already torn down every edge
// touching id before this runs, so FindAllDependents resolves to {id} alone and
// no unintended cascade occurs (cmd/bd/delete.go removes deps first, then
// DeleteIssue).
func deleteIssueInUOW(ctx context.Context, u uow.UnitOfWork, id, actor string) error {
	isWisp, err := isWispInUOW(ctx, u, id)
	if err != nil {
		return err
	}
	if isWisp {
		_, err = u.IssueUseCase().DeleteWisp(ctx, id, actor)
	} else {
		_, err = u.IssueUseCase().DeleteIssue(ctx, id, actor)
	}
	return err
}

// toStoreDeleteResult maps the domain delete tallies onto the store contract's
// result type, including the orphaned-dependent set the policy path reports
// under --force.
func toStoreDeleteResult(r domain.DeleteIssuesResult) *types.DeleteIssuesResult {
	return &types.DeleteIssuesResult{
		DeletedCount:      r.DeletedCount,
		DependenciesCount: r.DependenciesCount,
		LabelsCount:       r.LabelsCount,
		EventsCount:       r.EventsCount,
		OrphanedIssues:    r.OrphanedIssues,
	}
}
