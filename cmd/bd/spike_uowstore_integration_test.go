//go:build cgo

// DERISK SPIKE test for issue gastownhall/beads#4547 Route A (the uowstore
// adapter). It round-trips create -> get -> search -> ready -> close through the
// BD_SPIKE_UOWSTORE store (storage.DoltStorage implemented over the unit-of-work
// stack) and asserts the JSON output SHAPE matches the ordinary embedded store
// path for the same operations.
//
// Why this doesn't call `bd init --proxied-server`: that command is still gated
// (init.go: "--proxied-server is not yet implemented"), and this spike must not
// lift the gate. Instead the proxied workspace is bootstrapped the way the store
// path opens it: a metadata.json in proxied-server mode, then a first read
// command that boots the managed proxy + child dolt sql-server and auto-creates
// the schema (uow/dolt_sql_provider.go initSchema). issue_prefix — which normal
// `bd init` seeds via SetConfig — is seeded here directly over the proxy, since
// the spike store overrides only a read/write vertical slice (23 methods) and
// SetConfig is not among them.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage/dbproxy/proxy"
)

// spikeProxiedEnv is bdProxiedEnv plus the spike flag and a fixed actor so the
// two backends mint audit rows the same way.
func spikeProxiedEnv(dir string) []string {
	return append(bdProxiedEnv(dir),
		"BD_SPIKE_UOWSTORE=1",
		"BEADS_SKIP_IDENTITY_CHECK=1",
		"BEADS_ACTOR=spiketester",
	)
}

func spikeEmbeddedEnv(dir string) []string {
	return append(bdEnv(dir),
		"BEADS_SKIP_IDENTITY_CHECK=1",
		"BEADS_ACTOR=spiketester",
	)
}

func spikeRun(t *testing.T, bd, dir string, env []string, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command(bd, args...)
	cmd.Dir = dir
	cmd.Env = env
	return runCombined(cmd)
}

func runCombined(cmd *exec.Cmd) (string, string, error) {
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// jsonSlice extracts the first top-level JSON array from bd output and decodes
// it into a slice of maps. bd sometimes prefixes warnings before the payload.
func jsonSlice(t *testing.T, out string) []map[string]any {
	t.Helper()
	start := strings.Index(out, "[")
	if start < 0 {
		if strings.Contains(out, "null") {
			return nil
		}
		t.Fatalf("no JSON array in output:\n%s", out)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(out[start:]), &arr); err != nil {
		t.Fatalf("parse JSON array: %v\nraw: %s", err, out[start:])
	}
	return arr
}

// jsonObject extracts the first top-level JSON object (or first element of an
// array) from bd output.
func jsonObject(t *testing.T, out string) map[string]any {
	t.Helper()
	start := strings.IndexAny(out, "[{")
	if start < 0 {
		t.Fatalf("no JSON in output:\n%s", out)
	}
	s := out[start:]
	if strings.HasPrefix(s, "[") {
		var arr []map[string]any
		if err := json.Unmarshal([]byte(s), &arr); err != nil {
			t.Fatalf("parse JSON array: %v\nraw: %s", err, s)
		}
		if len(arr) == 0 {
			t.Fatalf("empty JSON array where object expected:\n%s", s)
		}
		return arr[0]
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		t.Fatalf("parse JSON object: %v\nraw: %s", err, s)
	}
	return obj
}

// volatileKeys are per-issue fields whose VALUES legitimately differ between two
// independent backends/runs (minted IDs, timestamps, per-workspace source repo).
// The spike compares the SHAPE (key set) and the stable fields, not these.
var volatileKeys = map[string]bool{
	"id":          true,
	"created_at":  true,
	"updated_at":  true,
	"closed_at":   true,
	"source_repo": true,
}

// normalizeIssue returns a copy with volatile values blanked and empty/null
// values dropped, so two backends can be compared on key set + stable values.
func normalizeIssue(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if isEmptyJSON(v) {
			continue
		}
		if volatileKeys[k] {
			out[k] = "<volatile>"
			continue
		}
		out[k] = v
	}
	return out
}

func isEmptyJSON(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	case float64:
		return false
	case bool:
		return false
	default:
		return false
	}
}

func keySet(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func assertSameKeySet(t *testing.T, label string, a, b map[string]any) {
	t.Helper()
	na, nb := normalizeIssue(a), normalizeIssue(b)
	missing := map[string]bool{}
	for k := range na {
		if _, ok := nb[k]; !ok {
			missing["embedded-missing:"+k] = true
		}
	}
	for k := range nb {
		if _, ok := na[k]; !ok {
			missing["spike-missing:"+k] = true
		}
	}
	if len(missing) > 0 {
		diff := make([]string, 0, len(missing))
		for k := range missing {
			diff = append(diff, k)
		}
		t.Errorf("%s: normalized key sets differ: %v\n spike keys=%v\n embedded keys=%v",
			label, diff, keySet(na), keySet(nb))
	}
}

// assertStableFieldsEqual compares the non-volatile field values the round-trip
// controls (title, status, priority, issue_type). These MUST match exactly
// across the two backends for the same operation.
func assertStableFieldsEqual(t *testing.T, label string, a, b map[string]any) {
	t.Helper()
	for _, f := range []string{"title", "status", "priority", "issue_type"} {
		if fmt.Sprint(a[f]) != fmt.Sprint(b[f]) {
			t.Errorf("%s: field %q differs: spike=%v embedded=%v", label, f, a[f], b[f])
		}
	}
}

// setupSpikeProxiedWorkspace bootstraps a proxied-server workspace WITHOUT the
// gated init command, then boots the provider and seeds issue_prefix.
func setupSpikeProxiedWorkspace(t *testing.T, bd, prefix string) (dir string, env []string, proj proxiedProject) {
	t.Helper()
	dir = t.TempDir()
	initGitRepoAt(t, dir)
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	database := sanitizePrefixForDB(prefix)
	meta := fmt.Sprintf(`{"database":"beads.db","dolt_mode":"proxied-server","dolt_database":%q,"project_id":"spike-%s"}`,
		database, prefix)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}

	proxyRoot := filepath.Join(beadsDir, "proxieddb")
	proj = proxiedProject{dir: dir, beadsDir: beadsDir, proxyRoot: proxyRoot, database: database, prefix: prefix}
	t.Cleanup(func() {
		if err := proxy.Shutdown(proxyRoot); err != nil {
			t.Logf("proxy.Shutdown(%s): %v", proxyRoot, err)
		}
	})
	shutdownProxyOnInterrupt(t, proxyRoot)

	env = spikeProxiedEnv(dir)

	// Boot the managed proxy + child server (this also auto-creates the schema).
	if _, stderr, err := spikeRun(t, bd, dir, env, "list", "--json"); err != nil {
		t.Fatalf("spike boot (list) failed: %v\n%s", err, stderr)
	}

	// Seed issue_prefix / issue_id_mode directly over the proxy — the spike store
	// does not override SetConfig, and normal init (which would seed these) is gated.
	db := openProxiedDB(t, proj)
	if _, err := db.Exec(
		"REPLACE INTO config (`key`, value) VALUES ('issue_prefix', ?), ('issue_id_mode', 'hash')",
		prefix,
	); err != nil {
		t.Fatalf("seed issue_prefix: %v", err)
	}
	return dir, env, proj
}

func TestSpikeUOWStore_RoundTrip(t *testing.T) {
	requireProxiedServerEnv(t)
	bd := buildEmbeddedBD(t)

	// --- spike (proxied uowstore) workspace ---
	spikeDir, spikeEnv, _ := setupSpikeProxiedWorkspace(t, bd, "spikep")

	// --- embedded reference workspace ---
	embDir, _, _ := bdInit(t, bd, "--prefix", "embp")
	embEnv := spikeEmbeddedEnv(embDir)

	const title = "Round trip task"

	// create
	spikeCreate, se, err := spikeRun(t, bd, spikeDir, spikeEnv, "create", title, "--json", "-p", "1")
	if err != nil {
		t.Fatalf("spike create failed: %v\n%s", err, se)
	}
	embCreate, ee, err := spikeRun(t, bd, embDir, embEnv, "create", title, "--json", "-p", "1")
	if err != nil {
		t.Fatalf("embedded create failed: %v\n%s", err, ee)
	}
	spikeIssue := jsonObject(t, spikeCreate)
	embIssue := jsonObject(t, embCreate)
	assertSameKeySet(t, "create", spikeIssue, embIssue)
	assertStableFieldsEqual(t, "create", spikeIssue, embIssue)
	if got := fmt.Sprint(spikeIssue["status"]); got != "open" {
		t.Errorf("spike create status = %q, want open", got)
	}

	spikeID := fmt.Sprint(spikeIssue["id"])
	embID := fmt.Sprint(embIssue["id"])

	// get (show)
	spikeShow, se2, err := spikeRun(t, bd, spikeDir, spikeEnv, "show", spikeID, "--json")
	if err != nil {
		t.Fatalf("spike show failed: %v\n%s", err, se2)
	}
	embShow, ee2, err := spikeRun(t, bd, embDir, embEnv, "show", embID, "--json")
	if err != nil {
		t.Fatalf("embedded show failed: %v\n%s", err, ee2)
	}
	assertSameKeySet(t, "show", jsonObject(t, spikeShow), jsonObject(t, embShow))
	assertStableFieldsEqual(t, "show", jsonObject(t, spikeShow), jsonObject(t, embShow))

	// search (list)
	spikeList, _, err := spikeRun(t, bd, spikeDir, spikeEnv, "list", "--json")
	if err != nil {
		t.Fatalf("spike list failed: %v", err)
	}
	embList, _, err := spikeRun(t, bd, embDir, embEnv, "list", "--json")
	if err != nil {
		t.Fatalf("embedded list failed: %v", err)
	}
	spikeListArr, embListArr := jsonSlice(t, spikeList), jsonSlice(t, embList)
	if len(spikeListArr) != 1 || len(embListArr) != 1 {
		t.Fatalf("list count mismatch: spike=%d embedded=%d", len(spikeListArr), len(embListArr))
	}
	assertSameKeySet(t, "list", spikeListArr[0], embListArr[0])

	// ready
	spikeReady, _, err := spikeRun(t, bd, spikeDir, spikeEnv, "ready", "--json")
	if err != nil {
		t.Fatalf("spike ready failed: %v", err)
	}
	embReady, _, err := spikeRun(t, bd, embDir, embEnv, "ready", "--json")
	if err != nil {
		t.Fatalf("embedded ready failed: %v", err)
	}
	spikeReadyArr, embReadyArr := jsonSlice(t, spikeReady), jsonSlice(t, embReady)
	if len(spikeReadyArr) != 1 || len(embReadyArr) != 1 {
		t.Fatalf("ready count mismatch: spike=%d embedded=%d", len(spikeReadyArr), len(embReadyArr))
	}
	assertSameKeySet(t, "ready", spikeReadyArr[0], embReadyArr[0])

	// close
	spikeClose, se3, err := spikeRun(t, bd, spikeDir, spikeEnv, "close", spikeID, "--json")
	if err != nil {
		t.Fatalf("spike close failed: %v\n%s", err, se3)
	}
	embClose, ee3, err := spikeRun(t, bd, embDir, embEnv, "close", embID, "--json")
	if err != nil {
		t.Fatalf("embedded close failed: %v\n%s", err, ee3)
	}
	if got := fmt.Sprint(jsonObject(t, spikeClose)["status"]); got != "closed" {
		t.Errorf("spike close status = %q, want closed", got)
	}
	assertSameKeySet(t, "close", jsonObject(t, spikeClose), jsonObject(t, embClose))
	assertStableFieldsEqual(t, "close", jsonObject(t, spikeClose), jsonObject(t, embClose))

	// ready after close: both empty (denormalized is_blocked / ready recompute).
	spikeReady2, _, err := spikeRun(t, bd, spikeDir, spikeEnv, "ready", "--json")
	if err != nil {
		t.Fatalf("spike ready-after-close failed: %v", err)
	}
	if arr := jsonSlice(t, spikeReady2); len(arr) != 0 {
		t.Errorf("spike ready after close = %d issues, want 0", len(arr))
	}

	// labeled create: the adapter must thread issue.Labels into CreateIssueParams,
	// or bd create silently drops every label on the store path (#4547 red-team).
	// Assert the labels PERSIST by reading them back via bd show on BOTH backends.
	assertLabeledCreateRoundTrips(t, bd, spikeDir, spikeEnv, embDir, embEnv)

	// not-found read: exercises the adapter's bespoke wisp-fallback +
	// sql.ErrNoRows→storage.ErrNotFound translation in GetIssue, which the
	// happy-path round trip never reaches. Both backends must agree on exit code.
	assertNotFoundShowParity(t, bd, spikeDir, spikeEnv, embDir, embEnv)
}

// assertLabeledCreateRoundTrips creates a labeled issue on both the spike and the
// embedded backend and asserts bd show reports the SAME persisted label set. This
// is the regression guard for the label-drop blocker: before the CreateIssueParams
// mapping, the spike side returned zero labels while embedded returned the label.
func assertLabeledCreateRoundTrips(t *testing.T, bd, spikeDir string, spikeEnv []string, embDir string, embEnv []string) {
	t.Helper()
	const label = "spikelabel"

	spikeOut, se, err := spikeRun(t, bd, spikeDir, spikeEnv,
		"create", "Labeled task", "--json", "-t", "task", "-l", label)
	if err != nil {
		t.Fatalf("spike labeled create failed: %v\n%s", err, se)
	}
	embOut, ee, err := spikeRun(t, bd, embDir, embEnv,
		"create", "Labeled task", "--json", "-t", "task", "-l", label)
	if err != nil {
		t.Fatalf("embedded labeled create failed: %v\n%s", err, ee)
	}
	spikeID := fmt.Sprint(jsonObject(t, spikeOut)["id"])
	embID := fmt.Sprint(jsonObject(t, embOut)["id"])

	spikeLabels := showLabelsForEnv(t, bd, spikeDir, spikeEnv, spikeID)
	embLabels := showLabelsForEnv(t, bd, embDir, embEnv, embID)
	if len(spikeLabels) == 0 {
		t.Fatalf("spike labeled create: bd show returned NO labels (label dropped) — want %q", label)
	}
	if !equalStringSets(spikeLabels, embLabels) {
		t.Errorf("labeled create: label sets differ across backends: spike=%v embedded=%v", spikeLabels, embLabels)
	}
	if !containsString(spikeLabels, label) {
		t.Errorf("spike labeled create: labels %v missing %q", spikeLabels, label)
	}
}

// showLabels reads the labels array from `bd show <id> --json`.
func showLabelsForEnv(t *testing.T, bd, dir string, env []string, id string) []string {
	t.Helper()
	out, se, err := spikeRun(t, bd, dir, env, "show", id, "--json")
	if err != nil {
		t.Fatalf("show %s failed: %v\n%s", id, err, se)
	}
	obj := jsonObject(t, out)
	raw, ok := obj["labels"].([]any)
	if !ok {
		return nil
	}
	labels := make([]string, 0, len(raw))
	for _, v := range raw {
		labels = append(labels, fmt.Sprint(v))
	}
	return labels
}

// assertNotFoundShowParity runs `bd show <nonexistent> --json` on both backends
// and asserts they agree on exit code (both nonzero). This drives the adapter's
// ErrNotFound translation + wisp fallback, the riskiest hand-written path.
func assertNotFoundShowParity(t *testing.T, bd, spikeDir string, spikeEnv []string, embDir string, embEnv []string) {
	t.Helper()
	const missing = "nope-does-not-exist"
	_, _, spikeErr := spikeRun(t, bd, spikeDir, spikeEnv, "show", missing, "--json")
	_, _, embErr := spikeRun(t, bd, embDir, embEnv, "show", missing, "--json")
	spikeCode := exitCode(spikeErr)
	embCode := exitCode(embErr)
	if spikeCode == 0 {
		t.Errorf("spike show <missing> exited 0, want nonzero (not-found)")
	}
	if spikeCode != embCode {
		t.Errorf("show <missing> exit codes differ: spike=%d embedded=%d", spikeCode, embCode)
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestSpikeUOWStore_CrossPlumbFindings pins the four open class-(a) divergences
// from tests/oracle-a/CROSSPLUMB-REPORT.md (F-1 error wording, F-2 no_history
// tier visibility, F-3 infra-type auto-ephemeral for `message`, F-5 close-output
// label hydration) to embedded's behavior. EMBEDDED IS THE REFERENCE: every
// assertion runs the SAME command on both plumbings with identical explicit IDs
// and requires byte-identical observable output.
// The two workspaces mint IDs under their own issue_prefix; explicit --id
// values must therefore carry the matching prefix. Comparisons strip the
// per-workspace prefix so a spike ID (spikef-<suffix>) and its embedded twin
// (embf-<suffix>) compare equal on the stable suffix.
const (
	spikeFindPrefix = "spikef"
	embFindPrefix   = "embf"
)

type plumbing struct {
	dir    string
	env    []string
	prefix string
}

func (p plumbing) id(suffix string) string { return p.prefix + "-" + suffix }

// stripPrefix removes a known "<prefix>-" from every whitespace/arrow-delimited
// token in s, so error strings that embed workspace IDs compare across plumbings.
func stripPrefix(s, prefix string) string {
	return strings.ReplaceAll(s, prefix+"-", "")
}

func TestSpikeUOWStore_CrossPlumbFindings(t *testing.T) {
	requireProxiedServerEnv(t)
	bd := buildEmbeddedBD(t)

	spikeDir, spikeEnv, _ := setupSpikeProxiedWorkspace(t, bd, spikeFindPrefix)
	embDir, _, _ := bdInit(t, bd, "--prefix", embFindPrefix)
	embEnv := spikeEmbeddedEnv(embDir)

	spike := plumbing{dir: spikeDir, env: spikeEnv, prefix: spikeFindPrefix}
	emb := plumbing{dir: embDir, env: embEnv, prefix: embFindPrefix}

	t.Run("F1_dep_error_wording", func(t *testing.T) {
		assertDepErrorParity(t, bd, spike, emb)
	})
	t.Run("F3_infra_message_ephemeral", func(t *testing.T) {
		assertInfraMessageEphemeral(t, bd, spike, emb)
	})
	t.Run("F2_no_history_tier_visibility", func(t *testing.T) {
		assertNoHistoryTierParity(t, bd, spike, emb)
	})
	t.Run("F5_close_label_hydration", func(t *testing.T) {
		assertCloseLabelHydration(t, bd, spike, emb)
	})
	t.Run("create_validation_parity", func(t *testing.T) {
		assertCreateValidationParity(t, bd, spike, emb)
	})
}

// createExpectingError runs `bd create ... --json` requiring a nonzero exit and
// returns the prefix-normalized stderr message. On a validation failure `bd
// create --json` prints NOTHING to stdout and `Error: <msg>` to stderr, so the
// stderr message (not a JSON .error field) is the observable pinned across
// plumbings.
func createExpectingError(t *testing.T, bd string, p plumbing, idSuffix string, extraArgs ...string) string {
	t.Helper()
	args := append([]string{"create", "V " + idSuffix, "--id", p.id(idSuffix), "--force", "--json"}, extraArgs...)
	stdout, stderr, err := spikeRun(t, bd, p.dir, p.env, args...)
	if err == nil {
		t.Fatalf("create %v (%s): expected validation error, got success\nstdout=%s", extraArgs, p.prefix, stdout)
	}
	// The spike workspace prints boot-time warnings (dir perms, beads.role) to
	// stderr that the embedded workspace does not; pin the `Error:` line alone.
	line := errorLine(stderr)
	if line == "" {
		t.Fatalf("create %v (%s): no 'Error:' line in stderr\nstderr=%s", extraArgs, p.prefix, stderr)
	}
	return stripPrefix(line, p.prefix)
}

// errorLine returns the last stderr line beginning with "Error:", trimmed.
func errorLine(stderr string) string {
	var out string
	for _, ln := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "Error:") {
			out = strings.TrimSpace(ln)
		}
	}
	return out
}

// assertCreateValidationParity pins the Slice-4 create-validation findings: the
// spike CreateIssue path must run the SAME issue validation embedded runs via
// issueops.PrepareIssueForInsert -> Issue.ValidateWithCustom. Without it the
// spike silently accepted custom-only issue types (agent/role/bogus) and the
// ephemeral+no_history combo an infra type + --no-history produces — states the
// default embedded store (EmbeddedDoltStore) rejects with exit 1. Every case
// must fail with a byte-identical (prefix-normalized) message on both plumbings.
func assertCreateValidationParity(t *testing.T, bd string, spike, emb plumbing) {
	t.Helper()
	cases := []struct {
		name     string
		idSuffix string
		args     []string
		want     string
	}{
		{"type_agent", "vagent", []string{"-t", "agent"}, "invalid issue type: agent"},
		{"type_role", "vrole", []string{"-t", "role"}, "invalid issue type: role"},
		{"type_bogus", "vbogus", []string{"-t", "totallybogus"}, "invalid issue type: totallybogus"},
		{"infra_message_no_history", "vmnh", []string{"-t", "message", "--no-history"}, "ephemeral and no_history are mutually exclusive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			embMsg := createExpectingError(t, bd, emb, tc.idSuffix, tc.args...)
			spikeMsg := createExpectingError(t, bd, spike, tc.idSuffix, tc.args...)
			if !strings.Contains(embMsg, tc.want) {
				t.Fatalf("precondition: embedded message %q missing %q", embMsg, tc.want)
			}
			if spikeMsg != embMsg {
				t.Errorf("create validation message differs\n spike=%q\n   emb=%q", spikeMsg, embMsg)
			}
		})
	}
}

// depAddError runs `bd dep add from to [--type typ] --json`, requires a nonzero
// exit, and returns the `.error` string the CLI emitted on stdout, with the
// workspace prefix stripped so the two plumbings' messages compare equal.
func depAddError(t *testing.T, bd string, p plumbing, fromSuffix, toSuffix, typ string) string {
	t.Helper()
	from, to := p.id(fromSuffix), p.id(toSuffix)
	args := []string{"dep", "add", from, to, "--json"}
	if typ != "" {
		args = append(args, "--type", typ)
	}
	stdout, stderr, err := spikeRun(t, bd, p.dir, p.env, args...)
	if err == nil {
		t.Fatalf("dep add %s %s: expected error, got success\nstdout=%s", from, to, stdout)
	}
	obj := jsonObject(t, stdout)
	msg, _ := obj["error"].(string)
	if msg == "" {
		t.Fatalf("dep add %s %s: no .error in stdout\nstdout=%s\nstderr=%s", from, to, stdout, stderr)
	}
	return stripPrefix(msg, p.prefix)
}

// assertDepErrorParity pins F-1: the cycle, self-dependency, and retype error
// strings must be byte-identical across plumbings (prefix-normalized).
func assertDepErrorParity(t *testing.T, bd string, spike, emb plumbing) {
	t.Helper()
	seed := func(p plumbing) {
		for _, suf := range []string{"ca", "cb"} {
			if _, se, err := spikeRun(t, bd, p.dir, p.env, "create", "Dep "+suf, "--id", p.id(suf), "--force", "-t", "task", "--json"); err != nil {
				t.Fatalf("seed create %s: %v\n%s", p.id(suf), err, se)
			}
		}
		// Forward blocking edge so reverse creates a cycle and a retype conflicts.
		if _, se, err := spikeRun(t, bd, p.dir, p.env, "dep", "add", p.id("ca"), p.id("cb"), "--json"); err != nil {
			t.Fatalf("seed dep add ca cb: %v\n%s", err, se)
		}
	}
	seed(spike)
	seed(emb)

	cases := []struct {
		name          string
		from, to, typ string
		wantContains  string
	}{
		{"cycle", "cb", "ca", "", "adding dependency would create a cycle"},
		{"self", "ca", "ca", "", "cannot add self-dependency: ca cannot depend on itself"},
		{"retype", "ca", "cb", "related", "already exists with type"},
	}
	for _, tc := range cases {
		spikeMsg := depAddError(t, bd, spike, tc.from, tc.to, tc.typ)
		embMsg := depAddError(t, bd, emb, tc.from, tc.to, tc.typ)
		if spikeMsg != embMsg {
			t.Errorf("F-1 %s: error strings differ\n spike=%q\n   emb=%q", tc.name, spikeMsg, embMsg)
		}
		if !strings.Contains(embMsg, tc.wantContains) {
			t.Errorf("F-1 %s: embedded error %q missing expected substring %q", tc.name, embMsg, tc.wantContains)
		}
	}
}

// assertInfraMessageEphemeral pins F-3: `create -t message` must auto-mark the
// issue ephemeral on both plumbings.
func assertInfraMessageEphemeral(t *testing.T, bd string, spike, emb plumbing) {
	t.Helper()
	create := func(p plumbing) any {
		out, se, err := spikeRun(t, bd, p.dir, p.env, "create", "Msg", "--id", p.id("msg1"), "--force", "-t", "message", "--json")
		if err != nil {
			t.Fatalf("create -t message (%s): %v\n%s", p.prefix, err, se)
		}
		return jsonObject(t, out)["ephemeral"]
	}
	embEph := create(emb)
	spikeEph := create(spike)
	if embEph != true {
		t.Fatalf("F-3 precondition: embedded -t message ephemeral=%v, want true", embEph)
	}
	if spikeEph != embEph {
		t.Errorf("F-3: -t message ephemeral differs: spike=%v embedded=%v", spikeEph, embEph)
	}
}

// assertNoHistoryTierParity pins F-2: a --no-history issue is hidden from
// `list --all` and `count` but visible in `ready` on BOTH plumbings.
func assertNoHistoryTierParity(t *testing.T, bd string, spike, emb plumbing) {
	t.Helper()
	seed := func(p plumbing) {
		if _, se, err := spikeRun(t, bd, p.dir, p.env, "create", "Normal", "--id", p.id("tn"), "--force", "-t", "task", "--json"); err != nil {
			t.Fatalf("seed tn: %v\n%s", err, se)
		}
		if _, se, err := spikeRun(t, bd, p.dir, p.env, "create", "NoHist", "--id", p.id("th"), "--force", "-t", "task", "--no-history", "--json"); err != nil {
			t.Fatalf("seed th: %v\n%s", err, se)
		}
	}
	seed(spike)
	seed(emb)

	// idSuffixes returns the prefix-stripped IDs from a JSON-array command.
	idSuffixes := func(p plumbing, args ...string) []string {
		out, se, err := spikeRun(t, bd, p.dir, p.env, args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, se)
		}
		ids := []string{}
		for _, m := range jsonSlice(t, out) {
			ids = append(ids, stripPrefix(fmt.Sprint(m["id"]), p.prefix))
		}
		return ids
	}

	spikeList := idSuffixes(spike, "list", "--all", "--json")
	embList := idSuffixes(emb, "list", "--all", "--json")
	if !equalStringSets(spikeList, embList) {
		t.Errorf("F-2 list --all id sets differ: spike=%v embedded=%v", spikeList, embList)
	}
	if containsString(spikeList, "th") {
		t.Errorf("F-2 list --all should hide no-history issue th, got %v", spikeList)
	}

	spikeReady := idSuffixes(spike, "ready", "--json")
	embReady := idSuffixes(emb, "ready", "--json")
	if !equalStringSets(spikeReady, embReady) {
		t.Errorf("F-2 ready id sets differ: spike=%v embedded=%v", spikeReady, embReady)
	}
	if !containsString(spikeReady, "th") {
		t.Errorf("F-2 ready should include no-history issue th, got %v", spikeReady)
	}

	count := func(p plumbing) string {
		out, se, err := spikeRun(t, bd, p.dir, p.env, "count", "--json")
		if err != nil {
			t.Fatalf("count (%s): %v\n%s", p.prefix, err, se)
		}
		return fmt.Sprint(jsonObject(t, out)["count"])
	}
	if sc, ec := count(spike), count(emb); sc != ec {
		t.Errorf("F-2 count differs: spike=%s embedded=%s", sc, ec)
	}
}

// assertCloseLabelHydration pins F-5: `close --json` returns the closed issue
// with its labels hydrated (not null) on both plumbings.
func assertCloseLabelHydration(t *testing.T, bd string, spike, emb plumbing) {
	t.Helper()
	const label = "red"
	closeLabels := func(p plumbing) []string {
		if _, se, err := spikeRun(t, bd, p.dir, p.env, "create", "Closable", "--id", p.id("cl"), "--force", "-t", "task", "-l", label, "--json"); err != nil {
			t.Fatalf("create cl: %v\n%s", err, se)
		}
		out, se, err := spikeRun(t, bd, p.dir, p.env, "close", p.id("cl"), "--json")
		if err != nil {
			t.Fatalf("close cl: %v\n%s", err, se)
		}
		obj := jsonObject(t, out)
		// close --json may return the issue directly or under {"closed":[...]}.
		if raw, ok := obj["closed"].([]any); ok && len(raw) > 0 {
			if m, ok := raw[0].(map[string]any); ok {
				obj = m
			}
		}
		labels := []string{}
		if raw, ok := obj["labels"].([]any); ok {
			for _, v := range raw {
				labels = append(labels, fmt.Sprint(v))
			}
		}
		return labels
	}
	embLabels := closeLabels(emb)
	spikeLabels := closeLabels(spike)
	if !containsString(embLabels, label) {
		t.Fatalf("F-5 precondition: embedded close labels %v missing %q", embLabels, label)
	}
	if !equalStringSets(spikeLabels, embLabels) {
		t.Errorf("F-5 close label sets differ: spike=%v embedded=%v", spikeLabels, embLabels)
	}
}
