//go:build cgo

// DERISK SPIKE test for gastownhall/beads#4547 Route A, slice 3 (the gc-16
// adapter completion). It drives the newly-implemented mutating + read slice of
// the storage.DoltStorage surface END-TO-END through the BD_SPIKE_UOWSTORE store
// and asserts parity with the ordinary embedded store path, exactly as
// TestSpikeUOWStore_RoundTrip does for the create/close core:
//
//	config set/get/unset  -> SetConfig / GetConfig / DeleteConfig
//	dep add / dep remove  -> AddDependency / RemoveDependency (+ ready recompute)
//	update --status       -> UpdateIssue
//	update --claim        -> ClaimIssue
//	reopen                -> ReopenIssue
//	count --json          -> CountIssues
//	ready --claim --json  -> ClaimReadyIssue + GetDependencyCounts/GetCommentCounts
//	delete --force        -> GetDependencies/GetDependents/GetDependencyRecords
//	                         + Transaction.UpdateIssue/RemoveDependency/DeleteIssue
//	purge --dry-run/-f    -> DeleteIssues (both paths)
//
// Bootstrapped WITHOUT the gated init command via setupSpikeProxiedWorkspace
// (shared with the round-trip test); the gate stays in place.
package main

import (
	"fmt"
	"strings"
	"testing"
)

// TestSpikeUOWStore_ConfigCrossPlumbing pins the load-bearing §Special(1) gap:
// config set/get must round-trip through the spike store (the cross-plumbing
// wrapper seeds issue_prefix this way). It also covers DeleteConfig via unset.
func TestSpikeUOWStore_ConfigCrossPlumbing(t *testing.T) {
	requireProxiedServerEnv(t)
	bd := buildEmbeddedBD(t)
	spikeDir, spikeEnv, _ := setupSpikeProxiedWorkspace(t, bd, "spikecfg")
	embDir, _, _ := bdInit(t, bd, "--prefix", "embcfg")
	embEnv := spikeEmbeddedEnv(embDir)

	const key, val = "custom.team", "platform"

	for _, tc := range []struct {
		name, dir string
		env       []string
	}{
		{"spike", spikeDir, spikeEnv},
		{"embedded", embDir, embEnv},
	} {
		if _, se, err := spikeRun(t, bd, tc.dir, tc.env, "config", "set", key, val); err != nil {
			t.Fatalf("%s config set failed: %v\n%s", tc.name, err, se)
		}
		out, se, err := spikeRun(t, bd, tc.dir, tc.env, "config", "get", key)
		if err != nil {
			t.Fatalf("%s config get failed: %v\n%s", tc.name, err, se)
		}
		if !strings.Contains(out, val) {
			t.Errorf("%s config get %s = %q, want it to contain %q", tc.name, key, out, val)
		}

		// unset -> DeleteConfig; a subsequent get reports "not set".
		if _, se, err := spikeRun(t, bd, tc.dir, tc.env, "config", "unset", key); err != nil {
			t.Fatalf("%s config unset failed: %v\n%s", tc.name, err, se)
		}
		out, _, _ = spikeRun(t, bd, tc.dir, tc.env, "config", "get", key)
		if strings.Contains(out, val) {
			t.Errorf("%s config get after unset = %q, still contains %q (DeleteConfig no-op?)", tc.name, out, val)
		}
	}
}

// TestSpikeUOWStore_MutationSurface walks the mutating gc-16 methods on ONE
// spike workspace, with an embedded reference for the JSON-shape/value parity
// checks. Sequential by construction: each step depends on the previous state.
func TestSpikeUOWStore_MutationSurface(t *testing.T) {
	requireProxiedServerEnv(t)
	bd := buildEmbeddedBD(t)
	spikeDir, spikeEnv, _ := setupSpikeProxiedWorkspace(t, bd, "spikemut")
	embDir, _, _ := bdInit(t, bd, "--prefix", "embmut")
	embEnv := spikeEmbeddedEnv(embDir)

	// Seed A, B on the spike store (CreateIssue is covered). Explicit --id keeps
	// the two backends' IDs identical so dep/update/delete args match.
	mkIssue := func(dir string, env []string, id, title string) {
		if _, se, err := spikeRun(t, bd, dir, env, "create", title, "--json", "-t", "task", "-p", "1", "--id", id, "--force"); err != nil {
			t.Fatalf("create %s failed: %v\n%s", id, err, se)
		}
	}
	for _, id := range []struct{ id, title string }{{"m-a", "Issue A"}, {"m-b", "Issue B"}} {
		mkIssue(spikeDir, spikeEnv, id.id, id.title)
		mkIssue(embDir, embEnv, id.id, id.title)
	}

	// --- update --status (UpdateIssue) ---
	for _, tc := range []struct {
		name, dir string
		env       []string
	}{{"spike", spikeDir, spikeEnv}, {"embedded", embDir, embEnv}} {
		if _, se, err := spikeRun(t, bd, tc.dir, tc.env, "update", "m-a", "--status", "in_progress"); err != nil {
			t.Fatalf("%s update --status failed: %v\n%s", tc.name, err, se)
		}
		out, se, err := spikeRun(t, bd, tc.dir, tc.env, "show", "m-a", "--json")
		if err != nil {
			t.Fatalf("%s show m-a failed: %v\n%s", tc.name, err, se)
		}
		if got := fmt.Sprint(jsonObject(t, out)["status"]); got != "in_progress" {
			t.Errorf("%s update --status: m-a status = %q, want in_progress", tc.name, got)
		}
	}

	// --- reopen (ReopenIssue): close m-a then reopen -> open ---
	if _, se, err := spikeRun(t, bd, spikeDir, spikeEnv, "close", "m-a", "--json"); err != nil {
		t.Fatalf("spike close m-a failed: %v\n%s", err, se)
	}
	if _, se, err := spikeRun(t, bd, spikeDir, spikeEnv, "reopen", "m-a", "--json"); err != nil {
		t.Fatalf("spike reopen m-a failed: %v\n%s", err, se)
	}
	out, se, err := spikeRun(t, bd, spikeDir, spikeEnv, "show", "m-a", "--json")
	if err != nil {
		t.Fatalf("spike show after reopen failed: %v\n%s", err, se)
	}
	if got := fmt.Sprint(jsonObject(t, out)["status"]); got != "open" {
		t.Errorf("spike reopen: m-a status = %q, want open", got)
	}

	// --- dep add (AddDependency): m-b depends on (is blocked by) m-a ---
	if _, se, err := spikeRun(t, bd, spikeDir, spikeEnv, "dep", "add", "m-b", "m-a"); err != nil {
		t.Fatalf("spike dep add failed: %v\n%s", err, se)
	}
	// m-a open => m-b blocked => ready lists only m-a.
	readyIDs := readyIDSet(t, bd, spikeDir, spikeEnv)
	if !readyIDs["m-a"] || readyIDs["m-b"] {
		t.Errorf("after dep add: ready=%v, want m-a ready and m-b blocked", readyIDs)
	}

	// --- count --json (CountIssues) parity: 2 durable issues on each backend ---
	spikeCount := countIssues(t, bd, spikeDir, spikeEnv)
	embCount := countIssues(t, bd, embDir, embEnv)
	if spikeCount != 2 || embCount != 2 {
		t.Errorf("count mismatch: spike=%d embedded=%d, want 2/2", spikeCount, embCount)
	}

	// --- dep remove (RemoveDependency): m-b becomes ready again ---
	if _, se, err := spikeRun(t, bd, spikeDir, spikeEnv, "dep", "remove", "m-b", "m-a"); err != nil {
		t.Fatalf("spike dep remove failed: %v\n%s", err, se)
	}
	readyIDs = readyIDSet(t, bd, spikeDir, spikeEnv)
	if !readyIDs["m-b"] {
		t.Errorf("after dep remove: ready=%v, want m-b ready (unblocked)", readyIDs)
	}

	// --- update --claim (ClaimIssue): a fresh open issue becomes in_progress ---
	mkIssue(spikeDir, spikeEnv, "m-c", "Issue C")
	if _, se, err := spikeRun(t, bd, spikeDir, spikeEnv, "update", "m-c", "--claim"); err != nil {
		t.Fatalf("spike update --claim failed: %v\n%s", err, se)
	}
	out, se, err = spikeRun(t, bd, spikeDir, spikeEnv, "show", "m-c", "--json")
	if err != nil {
		t.Fatalf("spike show m-c failed: %v\n%s", err, se)
	}
	claimed := jsonObject(t, out)
	if got := fmt.Sprint(claimed["status"]); got != "in_progress" {
		t.Errorf("update --claim: m-c status = %q, want in_progress", got)
	}
	if got := fmt.Sprint(claimed["assignee"]); got == "" || got == "<nil>" {
		t.Errorf("update --claim: m-c assignee = %q, want non-empty", got)
	}

	// --- delete --force (GetDependencies/Dependents/Records + tx trio) ---
	if _, se, err := spikeRun(t, bd, spikeDir, spikeEnv, "delete", "m-b", "--force"); err != nil {
		t.Fatalf("spike delete m-b failed: %v\n%s", err, se)
	}
	if _, _, err := spikeRun(t, bd, spikeDir, spikeEnv, "show", "m-b", "--json"); exitCode(err) == 0 {
		t.Errorf("spike show m-b after delete exited 0, want nonzero (deleted)")
	}

	// --- ready --claim --json (ClaimReadyIssue + count enrichment) ---
	out, se, err = spikeRun(t, bd, spikeDir, spikeEnv, "ready", "--claim", "--json")
	if err != nil {
		t.Fatalf("spike ready --claim failed: %v\n%s", err, se)
	}
	if !strings.Contains(out, "m-") {
		t.Errorf("ready --claim produced no claimed issue:\n%s", out)
	}
}

// TestSpikeUOWStore_PurgeBothPaths pins §Special(2): both purge paths call
// DeleteIssues — --dry-run (dryRun=true) and --force (dryRun=false).
func TestSpikeUOWStore_PurgeBothPaths(t *testing.T) {
	requireProxiedServerEnv(t)
	bd := buildEmbeddedBD(t)
	spikeDir, spikeEnv, _ := setupSpikeProxiedWorkspace(t, bd, "spikeprg")

	// A closed ephemeral (infra type routes to the wisps table) is the purge
	// candidate. create then close so it is closed-ephemeral.
	if _, se, err := spikeRun(t, bd, spikeDir, spikeEnv, "create", "ephemeral one", "--json", "-t", "agent", "--id", "p-e1", "--force"); err != nil {
		t.Fatalf("spike create ephemeral failed: %v\n%s", err, se)
	}
	if _, se, err := spikeRun(t, bd, spikeDir, spikeEnv, "close", "p-e1", "--json"); err != nil {
		t.Fatalf("spike close ephemeral failed: %v\n%s", err, se)
	}

	// dry-run: DeleteIssues(dryRun=true) — must exit 0 and NOT delete.
	if _, se, err := spikeRun(t, bd, spikeDir, spikeEnv, "purge", "--dry-run"); err != nil {
		t.Fatalf("spike purge --dry-run failed: %v\n%s", err, se)
	}
	// force: DeleteIssues(dryRun=false) — must exit 0.
	if _, se, err := spikeRun(t, bd, spikeDir, spikeEnv, "purge", "--force"); err != nil {
		t.Fatalf("spike purge --force failed: %v\n%s", err, se)
	}
}

// readyIDSet returns the set of issue IDs from `bd ready --json`.
func readyIDSet(t *testing.T, bd, dir string, env []string) map[string]bool {
	t.Helper()
	out, se, err := spikeRun(t, bd, dir, env, "ready", "--json")
	if err != nil {
		t.Fatalf("ready --json failed: %v\n%s", err, se)
	}
	set := map[string]bool{}
	for _, m := range jsonSlice(t, out) {
		set[fmt.Sprint(m["id"])] = true
	}
	return set
}

// countIssues parses the {"count": N} object from `bd count --json`.
func countIssues(t *testing.T, bd, dir string, env []string) int64 {
	t.Helper()
	out, se, err := spikeRun(t, bd, dir, env, "count", "--json")
	if err != nil {
		t.Fatalf("count --json failed: %v\n%s", err, se)
	}
	obj := jsonObject(t, out)
	n, ok := obj["count"].(float64)
	if !ok {
		t.Fatalf("count --json missing numeric count field:\n%s", out)
	}
	return int64(n)
}
