package uowstore

import (
	"context"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/types"
)

// This file completes the read half of the gc-16 surface (SPIKE-CENSUS-GC16).
// Every method opens ONE read UOW (NewUOW + deferred Close -> rollback, never
// Commit) and routes its outgoing error through mapUowError, matching the
// covered reads in store.go.

// GetDependencies returns the issues this issue depends on (delete pre-analysis,
// `del`). The domain seam exposes dependencies only as
// IssueWithDependencyMetadata; the store contract wants bare issues, so we drop
// the dependency-type facet. Issues-table direction only, matching the covered
// GetDependenciesWithMetadata.
func (s *uowStore) GetDependencies(ctx context.Context, issueID string) ([]*types.Issue, error) {
	u, err := s.provider.NewUOW(ctx)
	if err != nil {
		return nil, err
	}
	defer u.Close(ctx)
	items, err := u.DependencyUseCase().ListWithIssueMetadata(ctx, issueID, domain.DepListFilter{Direction: domain.DepDirectionOut})
	if err != nil {
		return nil, mapUowError(err)
	}
	return issuesFromDepMetadata(items), nil
}

// GetDependents returns the issues that depend on this issue (delete
// pre-analysis, `del`).
func (s *uowStore) GetDependents(ctx context.Context, issueID string) ([]*types.Issue, error) {
	u, err := s.provider.NewUOW(ctx)
	if err != nil {
		return nil, err
	}
	defer u.Close(ctx)
	items, err := u.DependencyUseCase().ListWithIssueMetadata(ctx, issueID, domain.DepListFilter{Direction: domain.DepDirectionIn})
	if err != nil {
		return nil, mapUowError(err)
	}
	return issuesFromDepMetadata(items), nil
}

// GetDependencyRecords returns the raw outgoing dependency edges for one issue
// (delete pre-delete edge enumeration; update --parent reparent). Backed by the
// bulk GetForIssueIDs, which unions the regular + wisp dependency tables.
func (s *uowStore) GetDependencyRecords(ctx context.Context, issueID string) ([]*types.Dependency, error) {
	u, err := s.provider.NewUOW(ctx)
	if err != nil {
		return nil, err
	}
	defer u.Close(ctx)
	m, err := u.DependencyUseCase().GetForIssueIDs(ctx, []string{issueID})
	if err != nil {
		return nil, mapUowError(err)
	}
	return m[issueID], nil
}

// GetDependencyCounts backs ready --claim enrichment (buildReadyIssueOutput,
// `rcp`). NOTE: CountsByIssueIDs counts the regular dependency table only — the
// documented wisp-union narrowing shared by the covered CountDependencies/
// CountDependents (SPIKE §3).
func (s *uowStore) GetDependencyCounts(ctx context.Context, issueIDs []string) (map[string]*types.DependencyCounts, error) {
	u, err := s.provider.NewUOW(ctx)
	if err != nil {
		return nil, err
	}
	defer u.Close(ctx)
	out, err := u.DependencyUseCase().CountsByIssueIDs(ctx, issueIDs)
	return out, mapUowError(err)
}

// GetCommentCounts backs ready --claim enrichment (`rcp`).
func (s *uowStore) GetCommentCounts(ctx context.Context, issueIDs []string) (map[string]int, error) {
	u, err := s.provider.NewUOW(ctx)
	if err != nil {
		return nil, err
	}
	defer u.Close(ctx)
	out, err := u.CommentUseCase().GetCommentCounts(ctx, issueIDs)
	return out, mapUowError(err)
}

// CountIssues backs `count --json` (tiers_ephemeral `t`,
// list_excludes_gate_and_infra_types `lx`). The domain seam exposes no
// cardinality-only count, so the filtered page is materialized (Limit/Offset
// cleared — the store contract ignores them for counts) and its length
// returned. Acceptable for the spike's small corpus; a full adapter needs a
// COUNT(*) use-case (SPIKE-REPORT §3). Wisps merge follows SearchIssues.
func (s *uowStore) CountIssues(ctx context.Context, query string, filter types.IssueFilter) (int64, error) {
	u, err := s.provider.NewUOW(ctx)
	if err != nil {
		return 0, err
	}
	defer u.Close(ctx)
	f := filter
	f.Limit = 0
	f.Offset = 0
	page, err := u.IssueUseCase().SearchIssues(ctx, query, f)
	if err != nil {
		return 0, mapUowError(err)
	}
	return int64(len(page.Items)), nil
}

// GetCustomStatuses is the non-Detailed store variant (update --status custom
// validation). GetCustomStatusesDetailed is covered in store.go; this one maps
// the same use-case result to bare names, mirroring the embedded store.
func (s *uowStore) GetCustomStatuses(ctx context.Context) ([]string, error) {
	u, err := s.provider.NewUOW(ctx)
	if err != nil {
		return nil, err
	}
	defer u.Close(ctx)
	detailed, err := u.ConfigUseCase().GetCustomStatuses(ctx)
	if err != nil {
		return nil, mapUowError(err)
	}
	return types.CustomStatusNames(detailed), nil
}

// GetNewlyUnblockedByClose backs close --suggest-next.
func (s *uowStore) GetNewlyUnblockedByClose(ctx context.Context, closedIssueID string) ([]*types.Issue, error) {
	u, err := s.provider.NewUOW(ctx)
	if err != nil {
		return nil, err
	}
	defer u.Close(ctx)
	out, err := u.IssueUseCase().GetNewlyUnblockedByClose(ctx, closedIssueID)
	return out, mapUowError(err)
}

// GetBlockedIssues backs ready's verbose/blocked paths.
func (s *uowStore) GetBlockedIssues(ctx context.Context, filter types.WorkFilter) ([]*types.BlockedIssue, error) {
	u, err := s.provider.NewUOW(ctx)
	if err != nil {
		return nil, err
	}
	defer u.Close(ctx)
	out, err := u.IssueUseCase().GetBlockedIssues(ctx, filter)
	return out, mapUowError(err)
}

// DetectCycles backs dep cycles / ready consistency. The in-corpus cycle
// rejection is enforced inside AddDependency's use-case, not here.
func (s *uowStore) DetectCycles(ctx context.Context) ([][]*types.Issue, error) {
	u, err := s.provider.NewUOW(ctx)
	if err != nil {
		return nil, err
	}
	defer u.Close(ctx)
	out, err := u.DependencyUseCase().DetectCycles(ctx)
	return out, mapUowError(err)
}

// GetDependencyTree backs dep tree. The store contract's reverse bool maps onto
// the domain direction (reverse -> dependents/In, else dependencies/Out).
func (s *uowStore) GetDependencyTree(ctx context.Context, issueID string, maxDepth int, showAllPaths bool, reverse bool) ([]*types.TreeNode, error) {
	u, err := s.provider.NewUOW(ctx)
	if err != nil {
		return nil, err
	}
	defer u.Close(ctx)
	dir := domain.DepDirectionOut
	if reverse {
		dir = domain.DepDirectionIn
	}
	out, err := u.DependencyUseCase().GetDependencyTree(ctx, issueID, domain.DepTreeOpts{
		MaxDepth:     maxDepth,
		ShowAllPaths: showAllPaths,
		Direction:    dir,
	})
	return out, mapUowError(err)
}

// GetBlockingInfoForIssues backs list's agent/long-text render. The domain
// BlockingInfo bundles the three maps the store contract returns positionally.
func (s *uowStore) GetBlockingInfoForIssues(ctx context.Context, issueIDs []string) (
	blockedByMap map[string][]string,
	blocksMap map[string][]string,
	parentMap map[string]string,
	err error,
) {
	u, uerr := s.provider.NewUOW(ctx)
	if uerr != nil {
		return nil, nil, nil, uerr
	}
	defer u.Close(ctx)
	info, ierr := u.DependencyUseCase().GetBlockingInfo(ctx, issueIDs)
	if ierr != nil {
		return nil, nil, nil, mapUowError(ierr)
	}
	return info.BlockedBy, info.Blocks, info.Parent, nil
}

// GetIssueComments returns an issue's comment bodies (show --json, best-effort:
// the caller discards the error). Issues-table only.
func (s *uowStore) GetIssueComments(ctx context.Context, issueID string) ([]*types.Comment, error) {
	u, err := s.provider.NewUOW(ctx)
	if err != nil {
		return nil, err
	}
	defer u.Close(ctx)
	out, err := u.CommentUseCase().GetCommentsForIssue(ctx, issueID)
	return out, mapUowError(err)
}

// IterIssueComments streams an issue's comments. The store contract returns an
// Iter the caller drains AFTER this method returns, but a streaming iterator
// would outlive the read UOW (its pinned connection is released by the deferred
// Close). We therefore materialize the comments inside the UOW and hand back a
// slice-backed iterator, which is safe post-Close — the same NewSliceIter shim
// the store uses to land Iter* methods complete over a slice source.
func (s *uowStore) IterIssueComments(ctx context.Context, issueID string) (storage.Iter[types.Comment], error) {
	comments, err := s.GetIssueComments(ctx, issueID)
	if err != nil {
		return nil, err
	}
	return storage.NewSliceIter(comments), nil
}

// GetLabelsForIssues backs stats' label breakdown.
func (s *uowStore) GetLabelsForIssues(ctx context.Context, issueIDs []string) (map[string][]string, error) {
	u, err := s.provider.NewUOW(ctx)
	if err != nil {
		return nil, err
	}
	defer u.Close(ctx)
	out, err := u.LabelUseCase().GetLabelsForIssues(ctx, issueIDs)
	return out, mapUowError(err)
}

// issuesFromDepMetadata drops the dependency-type facet, copying each embedded
// Issue into a fresh slice of pointers (the loop variable's address is not
// reused because we copy by value first).
func issuesFromDepMetadata(items []*types.IssueWithDependencyMetadata) []*types.Issue {
	out := make([]*types.Issue, 0, len(items))
	for _, it := range items {
		issue := it.Issue
		out = append(out, &issue)
	}
	return out
}
