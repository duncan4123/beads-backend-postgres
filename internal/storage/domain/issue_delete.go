package domain

import (
	"context"
	"fmt"
	"regexp"
	"sort"

	"github.com/steveyegge/beads/internal/storage/dberrors"
	"github.com/steveyegge/beads/internal/types"
)

func (u *issueUseCaseImpl) DeleteIssue(ctx context.Context, id, actor string) (DeleteIssuesResult, error) {
	if id == "" {
		return DeleteIssuesResult{}, fmt.Errorf("DeleteIssue: id must not be empty")
	}
	return u.deleteMany(ctx, DeleteIssuesParams{
		IDs:                  []string{id},
		UpdateTextReferences: true,
	}, actor)
}

func (u *issueUseCaseImpl) DeleteWisp(ctx context.Context, id, actor string) (DeleteIssuesResult, error) {
	if id == "" {
		return DeleteIssuesResult{}, fmt.Errorf("DeleteWisp: id must not be empty")
	}
	return u.deleteMany(ctx, DeleteIssuesParams{
		IDs:                  []string{id},
		UpdateTextReferences: true,
	}, actor)
}

func (u *issueUseCaseImpl) DeleteIssues(ctx context.Context, params DeleteIssuesParams, actor string) (DeleteIssuesResult, error) {
	return u.deleteMany(ctx, params, actor)
}

func (u *issueUseCaseImpl) DeleteWisps(ctx context.Context, params DeleteIssuesParams, actor string) (DeleteIssuesResult, error) {
	return u.deleteMany(ctx, params, actor)
}

func (u *issueUseCaseImpl) PreviewDelete(ctx context.Context, ids []string) (DeletePreview, error) {
	return u.previewDelete(ctx, ids)
}

func (u *issueUseCaseImpl) PreviewDeleteWisp(ctx context.Context, ids []string) (DeletePreview, error) {
	return u.previewDelete(ctx, ids)
}

func (u *issueUseCaseImpl) deleteMany(ctx context.Context, params DeleteIssuesParams, actor string) (DeleteIssuesResult, error) {
	if len(params.IDs) == 0 {
		return DeleteIssuesResult{}, nil
	}
	if params.EnforceCascadePolicy {
		return u.deleteManyWithPolicy(ctx, params, actor)
	}
	return u.deleteManyCascade(ctx, params, actor)
}

// deleteManyCascade is the legacy always-cascade path (proxied-server delete
// command). It unconditionally expands to transitive dependents.
func (u *issueUseCaseImpl) deleteManyCascade(ctx context.Context, params DeleteIssuesParams, actor string) (DeleteIssuesResult, error) {
	allIDs, err := u.issueRepo.FindAllDependents(ctx, params.IDs)
	if err != nil {
		return DeleteIssuesResult{}, fmt.Errorf("delete: cascade expansion: %w", err)
	}
	if len(allIDs) == 0 {
		return DeleteIssuesResult{}, nil
	}

	wispIDs, regularIDs, err := u.issueRepo.PartitionWispIDs(ctx, allIDs)
	if err != nil {
		return DeleteIssuesResult{}, fmt.Errorf("delete: partition: %w", err)
	}

	result := DeleteIssuesResult{}

	depIssue, err := u.depRepo.CountAllForIDs(ctx, regularIDs, DepCountsOpts{})
	if err != nil {
		return DeleteIssuesResult{}, fmt.Errorf("delete: count deps: %w", err)
	}
	depWisp, err := u.depRepo.CountAllForIDs(ctx, wispIDs, DepCountsOpts{UseWispsTable: true})
	if err != nil {
		return DeleteIssuesResult{}, fmt.Errorf("delete: count wisp deps: %w", err)
	}
	result.DependenciesCount = depIssue + depWisp

	labelIssue, err := u.labelRepo.CountAllForIDs(ctx, regularIDs, LabelOpts{})
	if err != nil {
		return DeleteIssuesResult{}, fmt.Errorf("delete: count labels: %w", err)
	}
	labelWisp, err := u.labelRepo.CountAllForIDs(ctx, wispIDs, LabelOpts{UseWispsTable: true})
	if err != nil {
		return DeleteIssuesResult{}, fmt.Errorf("delete: count wisp labels: %w", err)
	}
	result.LabelsCount = labelIssue + labelWisp

	evIssue, err := u.eventsRepo.CountAllForIDs(ctx, regularIDs, RecordEventOpts{})
	if err != nil {
		return DeleteIssuesResult{}, fmt.Errorf("delete: count events: %w", err)
	}
	evWisp, err := u.eventsRepo.CountAllForIDs(ctx, wispIDs, RecordEventOpts{UseWispsTable: true})
	if err != nil {
		return DeleteIssuesResult{}, fmt.Errorf("delete: count wisp events: %w", err)
	}
	result.EventsCount = evIssue + evWisp

	if params.DryRun {
		return result, nil
	}

	var connected map[string]*types.Issue
	var connectedIsWisp map[string]bool
	if params.UpdateTextReferences {
		deletedSet := make(map[string]bool, len(allIDs))
		for _, id := range allIDs {
			deletedSet[id] = true
		}
		connected, connectedIsWisp, err = u.collectConnectedIssues(ctx, allIDs, deletedSet)
		if err != nil {
			return result, err
		}
	}

	affectedIssues, affectedWisps, err := u.issueRepo.AffectedByDeletion(ctx, regularIDs, wispIDs)
	if err != nil {
		return result, fmt.Errorf("delete: affected by deletion: %w", err)
	}

	if _, err := u.depRepo.DeleteAllForIDs(ctx, regularIDs, DepInsertOpts{}); err != nil {
		return result, fmt.Errorf("delete: drop deps: %w", err)
	}
	if _, err := u.depRepo.DeleteAllForIDs(ctx, wispIDs, DepInsertOpts{UseWispsTable: true}); err != nil {
		return result, fmt.Errorf("delete: drop wisp deps: %w", err)
	}
	if _, err := u.labelRepo.DeleteAllForIDs(ctx, regularIDs, LabelOpts{}); err != nil {
		return result, fmt.Errorf("delete: drop labels: %w", err)
	}
	if _, err := u.labelRepo.DeleteAllForIDs(ctx, wispIDs, LabelOpts{UseWispsTable: true}); err != nil {
		return result, fmt.Errorf("delete: drop wisp labels: %w", err)
	}
	if _, err := u.eventsRepo.DeleteAllForIDs(ctx, regularIDs, RecordEventOpts{}); err != nil {
		return result, fmt.Errorf("delete: drop events: %w", err)
	}
	if _, err := u.eventsRepo.DeleteAllForIDs(ctx, wispIDs, RecordEventOpts{UseWispsTable: true}); err != nil {
		return result, fmt.Errorf("delete: drop wisp events: %w", err)
	}

	issuesDeleted, err := u.issueRepo.DeleteByIDs(ctx, regularIDs, IssueTableOpts{})
	if err != nil {
		return result, fmt.Errorf("delete: drop issue rows: %w", err)
	}
	wispsDeleted, err := u.issueRepo.DeleteByIDs(ctx, wispIDs, IssueTableOpts{UseWispsTable: true})
	if err != nil {
		return result, fmt.Errorf("delete: drop wisp rows: %w", err)
	}
	result.DeletedCount = issuesDeleted + wispsDeleted

	if params.UpdateTextReferences && len(connected) > 0 {
		refs, err := u.rewriteTextReferences(ctx, allIDs, connected, connectedIsWisp, actor)
		if err != nil {
			return result, fmt.Errorf("delete: rewrite text references: %w", err)
		}
		result.ReferencesUpdated = refs
	}

	if err := u.issueRepo.RecomputeIsBlocked(ctx, affectedIssues, affectedWisps); err != nil {
		return result, fmt.Errorf("delete: recompute is_blocked: %w", err)
	}

	return result, nil
}

// deleteManyWithPolicy reproduces issueops.DeleteIssuesInTx (the embedded batch
// delete) over the domain repositories: it honors Cascade/Force, refuses when a
// non-cascade delete would strip a dependent not in the deletion set, orphans
// those dependents under --force, and matches embedded's child-row counting
// (wisp aux rows counted only for cascade-discovered wisps, not directly
// requested ones). This is the store-path parity contract the cross-plumbing
// oracle pins (cmd/bd/delete.go, cmd/bd/purge.go → store.DeleteIssues).
func (u *issueUseCaseImpl) deleteManyWithPolicy(ctx context.Context, params DeleteIssuesParams, actor string) (DeleteIssuesResult, error) {
	initialWispIDs, regularIDs, err := u.issueRepo.PartitionWispIDs(ctx, params.IDs)
	if err != nil {
		return DeleteIssuesResult{}, fmt.Errorf("delete: partition: %w", err)
	}

	idSet := make(map[string]bool, len(params.IDs))
	for _, id := range params.IDs {
		idSet[id] = true
	}

	result := DeleteIssuesResult{}

	// Decide the effective regular deletion set + any orphaned external dependents.
	expandedRegularIDs := regularIDs
	if params.Cascade {
		all, err := u.issueRepo.FindAllDependents(ctx, regularIDs)
		if err != nil {
			return DeleteIssuesResult{}, fmt.Errorf("delete: cascade expansion: %w", err)
		}
		expandedRegularIDs = all
	} else {
		externalBySource, err := u.externalDependents(ctx, regularIDs, idSet)
		if err != nil {
			return DeleteIssuesResult{}, err
		}
		if len(externalBySource) > 0 {
			if !params.Force {
				// Refuse: name the first requested id (in argv order) that has a
				// dependent outside the deletion set, matching embedded.
				for _, id := range regularIDs {
					if deps := externalBySource[id]; len(deps) > 0 {
						result.OrphanedIssues = deps
						return result, fmt.Errorf("issue %s has dependents not in deletion set; use --cascade to delete them or --force to orphan them", id)
					}
				}
			}
			// Force: orphan every external dependent (deduped, deterministic).
			orphanSet := make(map[string]bool)
			for _, deps := range externalBySource {
				for _, d := range deps {
					orphanSet[d] = true
				}
			}
			orphans := make([]string, 0, len(orphanSet))
			for id := range orphanSet {
				orphans = append(orphans, id)
			}
			sort.Strings(orphans)
			result.OrphanedIssues = orphans
		}
	}

	cascadeWispIDs, finalRegularIDs, err := u.issueRepo.PartitionWispIDs(ctx, expandedRegularIDs)
	if err != nil {
		return DeleteIssuesResult{}, fmt.Errorf("delete: partition expanded: %w", err)
	}
	allWispIDs := append(append([]string{}, initialWispIDs...), cascadeWispIDs...)

	// Child-row counts. Embedded counts wisp aux rows for cascade-discovered
	// wisps only (CROSSPLUMB finding on AX-4): the directly-requested wisps
	// (initialWispIDs) are excluded, so an all-ephemeral purge reports 0 events.
	depReg, err := u.depRepo.CountAllForIDs(ctx, finalRegularIDs, DepCountsOpts{})
	if err != nil {
		return DeleteIssuesResult{}, fmt.Errorf("delete: count deps: %w", err)
	}
	depWisp, err := u.depRepo.CountAllForIDs(ctx, cascadeWispIDs, DepCountsOpts{UseWispsTable: true})
	if err != nil {
		return DeleteIssuesResult{}, fmt.Errorf("delete: count wisp deps: %w", err)
	}
	result.DependenciesCount = depReg + depWisp

	labelReg, err := u.labelRepo.CountAllForIDs(ctx, finalRegularIDs, LabelOpts{})
	if err != nil {
		return DeleteIssuesResult{}, fmt.Errorf("delete: count labels: %w", err)
	}
	labelWisp, err := u.labelRepo.CountAllForIDs(ctx, cascadeWispIDs, LabelOpts{UseWispsTable: true})
	if err != nil {
		return DeleteIssuesResult{}, fmt.Errorf("delete: count wisp labels: %w", err)
	}
	result.LabelsCount = labelReg + labelWisp

	evReg, err := u.eventsRepo.CountAllForIDs(ctx, finalRegularIDs, RecordEventOpts{})
	if err != nil {
		return DeleteIssuesResult{}, fmt.Errorf("delete: count events: %w", err)
	}
	evWisp, err := u.eventsRepo.CountAllForIDs(ctx, cascadeWispIDs, RecordEventOpts{UseWispsTable: true})
	if err != nil {
		return DeleteIssuesResult{}, fmt.Errorf("delete: count wisp events: %w", err)
	}
	result.EventsCount = evReg + evWisp

	result.DeletedCount = len(finalRegularIDs) + len(allWispIDs)

	if params.DryRun {
		return result, nil
	}

	var connected map[string]*types.Issue
	var connectedIsWisp map[string]bool
	deletedAll := append(append([]string{}, finalRegularIDs...), allWispIDs...)
	if params.UpdateTextReferences {
		deletedSet := make(map[string]bool, len(deletedAll))
		for _, id := range deletedAll {
			deletedSet[id] = true
		}
		connected, connectedIsWisp, err = u.collectConnectedIssues(ctx, deletedAll, deletedSet)
		if err != nil {
			return result, err
		}
	}

	affectedIssues, affectedWisps, err := u.issueRepo.AffectedByDeletion(ctx, finalRegularIDs, allWispIDs)
	if err != nil {
		return result, fmt.Errorf("delete: affected by deletion: %w", err)
	}

	if _, err := u.depRepo.DeleteAllForIDs(ctx, finalRegularIDs, DepInsertOpts{}); err != nil {
		return result, fmt.Errorf("delete: drop deps: %w", err)
	}
	if _, err := u.depRepo.DeleteAllForIDs(ctx, allWispIDs, DepInsertOpts{UseWispsTable: true}); err != nil {
		return result, fmt.Errorf("delete: drop wisp deps: %w", err)
	}
	if _, err := u.labelRepo.DeleteAllForIDs(ctx, finalRegularIDs, LabelOpts{}); err != nil {
		return result, fmt.Errorf("delete: drop labels: %w", err)
	}
	if _, err := u.labelRepo.DeleteAllForIDs(ctx, allWispIDs, LabelOpts{UseWispsTable: true}); err != nil {
		return result, fmt.Errorf("delete: drop wisp labels: %w", err)
	}
	if _, err := u.eventsRepo.DeleteAllForIDs(ctx, finalRegularIDs, RecordEventOpts{}); err != nil {
		return result, fmt.Errorf("delete: drop events: %w", err)
	}
	if _, err := u.eventsRepo.DeleteAllForIDs(ctx, allWispIDs, RecordEventOpts{UseWispsTable: true}); err != nil {
		return result, fmt.Errorf("delete: drop wisp events: %w", err)
	}

	issuesDeleted, err := u.issueRepo.DeleteByIDs(ctx, finalRegularIDs, IssueTableOpts{})
	if err != nil {
		return result, fmt.Errorf("delete: drop issue rows: %w", err)
	}
	wispsDeleted, err := u.issueRepo.DeleteByIDs(ctx, allWispIDs, IssueTableOpts{UseWispsTable: true})
	if err != nil {
		return result, fmt.Errorf("delete: drop wisp rows: %w", err)
	}
	result.DeletedCount = issuesDeleted + wispsDeleted

	if params.UpdateTextReferences && len(connected) > 0 {
		refs, err := u.rewriteTextReferences(ctx, deletedAll, connected, connectedIsWisp, actor)
		if err != nil {
			return result, fmt.Errorf("delete: rewrite text references: %w", err)
		}
		result.ReferencesUpdated = refs
	}

	if err := u.issueRepo.RecomputeIsBlocked(ctx, affectedIssues, affectedWisps); err != nil {
		return result, fmt.Errorf("delete: recompute is_blocked: %w", err)
	}

	return result, nil
}

// externalDependents returns, keyed by requested source id, the issues that
// depend on that source but are NOT themselves in idSet — the direct dependents
// that a non-cascade delete would orphan. It spans both the regular and wisp
// dependency tables, mirroring embedded's findExternalDependents.
func (u *issueUseCaseImpl) externalDependents(ctx context.Context, ids []string, idSet map[string]bool) (map[string][]string, error) {
	bySource := make(map[string][]string)
	if len(ids) == 0 {
		return bySource, nil
	}
	for _, useWisp := range []bool{false, true} {
		res, err := u.depRepo.ListByIssueIDs(ctx, ids, DepListOpts{Direction: DepDirectionIn, UseWispsTable: useWisp})
		if err != nil {
			if useWisp && dberrors.IsTableNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("delete: external dependents: %w", err)
		}
		for _, deps := range res.Incoming {
			for _, d := range deps {
				if d.IssueID == "" || idSet[d.IssueID] {
					continue
				}
				bySource[d.DependsOnID] = append(bySource[d.DependsOnID], d.IssueID)
			}
		}
	}
	return bySource, nil
}

func (u *issueUseCaseImpl) previewDelete(ctx context.Context, ids []string) (DeletePreview, error) {
	preview := DeletePreview{
		Issues:          map[string]*types.Issue{},
		ConnectedIssues: map[string]*types.Issue{},
		DepRecords:      map[string][]*types.Dependency{},
	}
	if len(ids) == 0 {
		return preview, nil
	}

	fromIssues, err := u.issueRepo.GetByIDs(ctx, ids, IssueTableOpts{})
	if err != nil {
		return preview, fmt.Errorf("previewDelete: load issues: %w", err)
	}
	for _, iss := range fromIssues {
		preview.Issues[iss.ID] = iss
	}
	fromWisps, err := u.issueRepo.GetByIDs(ctx, ids, IssueTableOpts{UseWispsTable: true})
	if err != nil && !dberrors.IsTableNotExist(err) {
		return preview, fmt.Errorf("previewDelete: load wisps: %w", err)
	}
	for _, iss := range fromWisps {
		preview.Issues[iss.ID] = iss
	}

	for _, id := range ids {
		if _, ok := preview.Issues[id]; !ok {
			preview.NotFound = append(preview.NotFound, id)
		}
	}

	depRes, err := u.depRepo.ListByIssueIDs(ctx, ids, DepListOpts{Direction: DepDirectionOut})
	if err != nil {
		return preview, fmt.Errorf("previewDelete: list deps: %w", err)
	}
	for id, deps := range depRes.Outgoing {
		preview.DepRecords[id] = deps
	}
	wispDepRes, err := u.depRepo.ListByIssueIDs(ctx, ids, DepListOpts{Direction: DepDirectionOut, UseWispsTable: true})
	if err != nil && !dberrors.IsTableNotExist(err) {
		return preview, fmt.Errorf("previewDelete: list wisp deps: %w", err)
	}
	for id, deps := range wispDepRes.Outgoing {
		preview.DepRecords[id] = append(preview.DepRecords[id], deps...)
	}

	allIDs, err := u.issueRepo.FindAllDependents(ctx, ids)
	if err != nil {
		return preview, fmt.Errorf("previewDelete: cascade expansion: %w", err)
	}
	deletedSet := make(map[string]bool, len(allIDs))
	for _, id := range allIDs {
		deletedSet[id] = true
	}
	connected, _, err := u.collectConnectedIssues(ctx, allIDs, deletedSet)
	if err != nil {
		return preview, err
	}
	preview.ConnectedIssues = connected
	return preview, nil
}

func (u *issueUseCaseImpl) collectConnectedIssues(
	ctx context.Context, allIDs []string, deletedSet map[string]bool,
) (map[string]*types.Issue, map[string]bool, error) {
	out := map[string]*types.Issue{}
	isWisp := map[string]bool{}
	if len(allIDs) == 0 {
		return out, isWisp, nil
	}

	issueRes, err := u.depRepo.ListByIssueIDs(ctx, allIDs, DepListOpts{Direction: DepDirectionBoth})
	if err != nil {
		return nil, nil, fmt.Errorf("collectConnected (issues): %w", err)
	}
	wispRes, err := u.depRepo.ListByIssueIDs(ctx, allIDs, DepListOpts{Direction: DepDirectionBoth, UseWispsTable: true})
	if err != nil && !dberrors.IsTableNotExist(err) {
		return nil, nil, fmt.Errorf("collectConnected (wisps): %w", err)
	}

	neighbors := map[string]bool{}
	accumulate := func(m map[string][]*types.Dependency) {
		for _, deps := range m {
			for _, d := range deps {
				for _, candidate := range [2]string{d.IssueID, d.DependsOnID} {
					if candidate == "" || deletedSet[candidate] {
						continue
					}
					neighbors[candidate] = true
				}
			}
		}
	}
	accumulate(issueRes.Outgoing)
	accumulate(issueRes.Incoming)
	accumulate(wispRes.Outgoing)
	accumulate(wispRes.Incoming)

	if len(neighbors) == 0 {
		return out, isWisp, nil
	}
	ids := make([]string, 0, len(neighbors))
	for id := range neighbors {
		ids = append(ids, id)
	}

	fromIssues, err := u.issueRepo.GetByIDs(ctx, ids, IssueTableOpts{})
	if err != nil {
		return nil, nil, fmt.Errorf("hydrate neighbors (issues): %w", err)
	}
	for _, iss := range fromIssues {
		out[iss.ID] = iss
	}
	fromWisps, err := u.issueRepo.GetByIDs(ctx, ids, IssueTableOpts{UseWispsTable: true})
	if err != nil && !dberrors.IsTableNotExist(err) {
		return nil, nil, fmt.Errorf("hydrate neighbors (wisps): %w", err)
	}
	for _, iss := range fromWisps {
		out[iss.ID] = iss
		isWisp[iss.ID] = true
	}
	return out, isWisp, nil
}

func (u *issueUseCaseImpl) rewriteTextReferences(
	ctx context.Context, deletedIDs []string,
	connected map[string]*types.Issue, isWisp map[string]bool, actor string,
) (int, error) {
	touched := make(map[string]bool)
	for _, id := range deletedIDs {
		pattern := `(^|[^A-Za-z0-9_-])(` + regexp.QuoteMeta(id) + `)($|[^A-Za-z0-9_-])`
		re := regexp.MustCompile(pattern)
		replacement := `$1[deleted:` + id + `]$3`
		for connID, conn := range connected {
			updates := map[string]any{}
			if re.MatchString(conn.Description) {
				updates["description"] = re.ReplaceAllString(conn.Description, replacement)
			}
			if conn.Notes != "" && re.MatchString(conn.Notes) {
				updates["notes"] = re.ReplaceAllString(conn.Notes, replacement)
			}
			if conn.Design != "" && re.MatchString(conn.Design) {
				updates["design"] = re.ReplaceAllString(conn.Design, replacement)
			}
			if conn.AcceptanceCriteria != "" && re.MatchString(conn.AcceptanceCriteria) {
				updates["acceptance_criteria"] = re.ReplaceAllString(conn.AcceptanceCriteria, replacement)
			}
			if len(updates) == 0 {
				continue
			}
			opts := IssueTableOpts{UseWispsTable: isWisp[connID]}
			if err := u.issueRepo.Update(ctx, connID, updates, actor, opts); err != nil {
				return len(touched), fmt.Errorf("rewrite refs %s: %w", connID, err)
			}
			touched[connID] = true
			if desc, ok := updates["description"].(string); ok {
				conn.Description = desc
			}
			if notes, ok := updates["notes"].(string); ok {
				conn.Notes = notes
			}
			if design, ok := updates["design"].(string); ok {
				conn.Design = design
			}
			if ac, ok := updates["acceptance_criteria"].(string); ok {
				conn.AcceptanceCriteria = ac
			}
		}
	}
	return len(touched), nil
}
