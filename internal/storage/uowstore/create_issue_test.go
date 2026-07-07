package uowstore

import (
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

func TestCreateDependencySpecsMapsIssueDependencies(t *testing.T) {
	issue := &types.Issue{
		ID: "bd-child",
		Dependencies: []*types.Dependency{
			{DependsOnID: "bd-blocker", Type: types.DepConditionalBlocks, Metadata: `{"gate":"ok"}`},
			{DependsOnID: "bd-default"},
			{IssueID: "bd-dependent", DependsOnID: "bd-child", Type: types.DepRelated, Metadata: `{"reverse":true}`},
			nil,
			{Type: types.DepBlocks},
		},
	}

	got := createDependencySpecs(issue)
	if len(got) != 3 {
		t.Fatalf("dependency spec count = %d, want 3: %#v", len(got), got)
	}

	if got[0].TargetID != "bd-blocker" || got[0].Type != types.DepConditionalBlocks || got[0].Metadata != `{"gate":"ok"}` || got[0].SwapDirection {
		t.Fatalf("first spec = %#v, want conditional blocker to bd-blocker with metadata", got[0])
	}
	if got[1].TargetID != "bd-default" || got[1].Type != types.DepBlocks || got[1].SwapDirection {
		t.Fatalf("second spec = %#v, want default blocks dep to bd-default", got[1])
	}
	if got[2].TargetID != "bd-dependent" || got[2].Type != types.DepRelated || got[2].Metadata != `{"reverse":true}` || !got[2].SwapDirection {
		t.Fatalf("third spec = %#v, want reverse related dep to bd-dependent", got[2])
	}
}
