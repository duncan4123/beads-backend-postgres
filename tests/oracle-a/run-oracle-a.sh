#!/usr/bin/env bash
# Oracle A — refactor-safety differential conformance for Go bd.
#
# Runs the SAME gc-contract scenarios against:
#   REFERENCE  bd  — built from the merge base of HEAD and origin/main (the "before")
#   CANDIDATE  bd  — built from the current working tree      (the "after")
# and diffs each step (exit code, stderr, JSON-aware stdout) with volatile
# normalization (<TS>/<UUID>/<ACTOR>/<EMAIL>). ZERO in-scope divergences is the
# gate: any in-scope FAIL means the branch changed a user-visible bd behavior
# that Gas City's contract surface depends on.
#
# What this is NOT: it is not a bts-rs run. It reuses the bts-rs conformance
# harness (vendored under harness/, see harness/PROVENANCE.md) but points it at
# two Go bd binaries. bts-rs is never modified or committed to.
#
# Usage:
#   tests/oracle-a/run-oracle-a.sh              # ref = merge-base(HEAD, origin/main), candidate = working tree
#   REF_REF=<gitref> tests/oracle-a/run-oracle-a.sh   # override the reference ref
#   KEEP_ARTIFACTS=1 tests/oracle-a/run-oracle-a.sh    # keep the scratch build dir
#
# Requirements: cargo (Rust), a CGO toolchain (gcc), go. See README.md.
#
# Exit status: 0 = 100% in-scope pass; 1 = at least one in-scope divergence;
#              2 = setup/build error (could not produce a result).

set -euo pipefail

# --- locate the repo and this script -------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HARNESS_DIR="$SCRIPT_DIR/harness"
REPO_ROOT="$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel)"
# The reference is the MERGE BASE of HEAD and origin/main — "the code this branch
# forked from" — not origin/main's tip. Using the tip would fold upstream commits
# that are absent from the candidate into the diff, surfacing upstream behavior
# changes as in-scope FAILs falsely attributed to the branch (allowlist-rot, §7).
# Override with REF_REF for a deliberate comparison (a release tag, origin/main tip).
if [ -z "${REF_REF:-}" ]; then
  REF_REF="$(git -C "$REPO_ROOT" merge-base HEAD origin/main 2>/dev/null || echo origin/main)"
fi

# gms_pure_go is mandatory per docs/ICU-POLICY.md; CGO is required for embedded Dolt.
BUILD_TAGS="gms_pure_go"

# unique scratch dir per run — cp over an exec-mapped binary fails silently and
# would score a stale binary, so every binary gets a fresh, unique path.
RUN_ID="$(date +%Y%m%d-%H%M%S)-$$"
SCRATCH="${TMPDIR:-/tmp}/oracle-a-$RUN_ID"
REF_SRC="$SCRATCH/ref-src"
REF_BIN="$SCRATCH/bd-reference"
CAND_BIN="$SCRATCH/bd-candidate"
mkdir -p "$SCRATCH"

log()  { printf '\033[1;36m[oracle-a]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[oracle-a]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[oracle-a]\033[0m %s\n' "$*" >&2; exit 2; }

# --- snapshot go.mod/go.sum BEFORE any build ------------------------------------------
# The candidate build (`go build`) may rewrite go.mod/go.sum. The old rig ran
# `git checkout -- go.mod go.sum`, which restores to HEAD and so DESTROYS any
# pre-existing uncommitted user edit (the worktree may legitimately carry one —
# e.g. a dep bump that is itself the candidate under test). Instead we copy the
# exact pre-run bytes aside and restore ONLY the files the build actually changed,
# comparing by content — restore-to-pre-run, never restore-to-HEAD.
GOMOD_SNAP="$SCRATCH/go.mod.snapshot"
GOSUM_SNAP="$SCRATCH/go.sum.snapshot"
[ -f "$REPO_ROOT/go.mod" ] && cp "$REPO_ROOT/go.mod" "$GOMOD_SNAP"
[ -f "$REPO_ROOT/go.sum" ] && cp "$REPO_ROOT/go.sum" "$GOSUM_SNAP"

# restore_if_build_churned <snapshot> <live> — put back the pre-run bytes only if
# the build modified the file; leaves an untouched (incl. user-edited) file alone.
restore_if_build_churned() {
  local snap="$1" live="$2"
  [ -f "$snap" ] || return 0
  if [ ! -f "$live" ] || ! cmp -s "$snap" "$live"; then
    cp "$snap" "$live"
  fi
}

# --- cleanup: always remove the reference worktree; drop scratch unless asked to keep -
cleanup() {
  local rc=$?
  if git -C "$REPO_ROOT" worktree list --porcelain 2>/dev/null | grep -qF "$REF_SRC"; then
    git -C "$REPO_ROOT" worktree remove --force "$REF_SRC" 2>/dev/null || true
  fi
  # Undo build churn to go.mod/go.sum WITHOUT destroying pre-existing user edits:
  # restore the pre-run snapshot bytes, and only when the build changed the file.
  restore_if_build_churned "$GOMOD_SNAP" "$REPO_ROOT/go.mod"
  restore_if_build_churned "$GOSUM_SNAP" "$REPO_ROOT/go.sum"
  if [ "${KEEP_ARTIFACTS:-0}" = "1" ]; then
    warn "KEEP_ARTIFACTS=1 — leaving scratch at $SCRATCH"
  else
    rm -rf "$SCRATCH"
  fi
  return $rc
}
# EXIT fires on normal exit and die(); trap INT/TERM to a plain exit so the EXIT
# trap still runs (Ctrl-C during the multi-minute reference build otherwise leaks
# a registered git worktree under /tmp).
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# --- preflight -----------------------------------------------------------------------
command -v cargo >/dev/null 2>&1 || die "cargo not found (Rust toolchain required)"
command -v go    >/dev/null 2>&1 || die "go not found"
command -v gcc   >/dev/null 2>&1 || command -v cc >/dev/null 2>&1 || die "no C compiler (CGO required)"

REF_SHA="$(git -C "$REPO_ROOT" rev-parse "$REF_REF" 2>/dev/null)" || die "cannot resolve ref '$REF_REF' (need 'git fetch'?)"
CAND_SHA="$(git -C "$REPO_ROOT" rev-parse HEAD)"
log "reference ref : $REF_REF ($REF_SHA)"
log "candidate     : working tree (HEAD $CAND_SHA)"
if [ "$REF_SHA" = "$CAND_SHA" ] && git -C "$REPO_ROOT" diff --quiet; then
  log "note: candidate == reference (clean tree at $REF_REF) — this run proves the"
  log "      rig+normalization are leak-free (main-vs-main). Divergences here are"
  log "      harness bugs, not code changes."
fi

# --- 1. reference bd from origin/main (isolated worktree) ----------------------------
log "building REFERENCE bd from $REF_REF ..."
git -C "$REPO_ROOT" worktree add --detach "$REF_SRC" "$REF_SHA" >/dev/null 2>&1 \
  || die "git worktree add failed for $REF_SHA"
( cd "$REF_SRC" && CGO_ENABLED=1 go build -tags "$BUILD_TAGS" -o "$REF_BIN" ./cmd/bd ) \
  || die "reference bd build failed"
[ -x "$REF_BIN" ] || die "reference bd not produced at $REF_BIN"
log "reference bd : $REF_BIN ($($REF_BIN version 2>/dev/null | head -1))"

# --- 2. candidate bd from the working tree -------------------------------------------
log "building CANDIDATE bd from the working tree ..."
( cd "$REPO_ROOT" && CGO_ENABLED=1 go build -tags "$BUILD_TAGS" -o "$CAND_BIN" ./cmd/bd ) \
  || die "candidate bd build failed"
# restore any go.sum/go.mod build churn immediately (belt-and-suspenders; also in
# cleanup) — restore-to-pre-run bytes, so a pre-existing user edit is preserved.
restore_if_build_churned "$GOMOD_SNAP" "$REPO_ROOT/go.mod"
restore_if_build_churned "$GOSUM_SNAP" "$REPO_ROOT/go.sum"
[ -x "$CAND_BIN" ] || die "candidate bd not produced at $CAND_BIN"
log "candidate bd : $CAND_BIN ($($CAND_BIN version 2>/dev/null | head -1))"

# --- 3. build the vendored harness ---------------------------------------------------
log "building conformance harness (vendored bts-rs copy) ..."
( cd "$HARNESS_DIR" && cargo build --release --bins ) >/dev/null 2>&1 \
  || die "harness build failed (run 'cargo build --release --bins' in $HARNESS_DIR to see why)"
CAPTURE="$HARNESS_DIR/target/release/capture_golden"
SCOREBOARD="$HARNESS_DIR/target/release/scoreboard"

# fresh goldens every run: the reference is resolved above (merge-base by
# default), not a pinned snapshot — so goldens always reflect the current "before".
rm -rf "$HARNESS_DIR/testdata/golden"

# --- 4. capture goldens from the reference bd ----------------------------------------
log "capturing goldens from REFERENCE bd ..."
BTS_REFERENCE_BD="$REF_BIN" "$CAPTURE" \
  || die "golden capture failed"

# --- 4b. FLOOR ASSERTIONS — goldens must represent WORKING behavior ------------------
# Without a floor, green proves nothing: goldens captured from a stub that fails
# every step (e.g. broken workspace init in CI, missing HOME, sandboxed Dolt file
# locks) still score 100% because the diff is empty on both sides. capture_golden
# only fails on process-spawn IO errors, not on bd exit codes. So before scoring,
# assert the reference actually did the work: every scenario's `create` steps must
# exit 0 and produce a parseable JSON object carrying an "id". A floor violation is
# a SETUP error (exit 2), never a pass — it means the environment, not the branch,
# is what the goldens captured.
GOLDEN_DIR="$HARNESS_DIR/testdata/golden"
command -v jq >/dev/null 2>&1 || die "jq not found (required for floor assertions)"
log "checking golden floor (reference create steps must exit 0 with an id) ..."
floor_violations=0
shopt -s nullglob
for trace in "$GOLDEN_DIR"/*.trace.json; do
  scen="$(basename "$trace" .trace.json)"
  # number of create steps, and how many of them exited 0 with a JSON object id.
  n_create="$(jq '[.steps[] | select(.args[0]=="create")] | length' "$trace")"
  n_ok="$(jq '[.steps[]
                | select(.args[0]=="create")
                | select(.exit==0)
                | select((.stdout | length) > 0)
                | select((.stdout | fromjson? | if type=="array" then .[0] else . end | .id? // empty) != "")]
              | length' "$trace")"
  if [ "$n_create" -gt 0 ] && [ "$n_ok" -lt "$n_create" ]; then
    warn "  FLOOR: $scen — $((n_create - n_ok))/$n_create create step(s) did not exit 0 with a JSON id"
    floor_violations=$((floor_violations + 1))
  fi
done
shopt -u nullglob
if [ "$floor_violations" -gt 0 ]; then
  die "golden floor FAILED: $floor_violations scenario(s) captured broken reference behavior — the environment, not the branch, is under test. Refusing to score."
fi
log "golden floor OK — reference create steps all exit 0 with an id."

# --- 5. score the candidate against the reference goldens ----------------------------
log "scoring CANDIDATE bd against reference goldens ..."
SCORE_OUT="$SCRATCH/scoreboard.out"
BTS_CANDIDATE="$CAND_BIN" "$SCOREBOARD" | tee "$SCORE_OUT"

# --- 6. verdict from the IN-SCOPE line ------------------------------------------------
# scoreboard prints:  "  PASS: <n>   FAIL: <m>   (<p>%)"  under the IN-SCOPE header.
IN_LINE="$(grep -E '^\s*PASS:.*FAIL:' "$SCORE_OUT" | head -1)"
IN_PASS="$(printf '%s' "$IN_LINE" | sed -E 's/.*PASS:\s*([0-9]+).*/\1/')"
IN_FAIL="$(printf '%s' "$IN_LINE" | sed -E 's/.*FAIL:\s*([0-9]+).*/\1/')"

echo
if [ -z "${IN_FAIL:-}" ]; then
  die "could not parse scoreboard output"
elif [ "$IN_FAIL" -eq 0 ]; then
  log "RESULT: IN-SCOPE PASS ($IN_PASS scenarios, 0 divergences) — refactor is behavior-preserving on the gc-contract surface."
  exit 0
else
  warn "RESULT: IN-SCOPE FAIL — $IN_FAIL divergence(s) vs $REF_REF (ref $REF_SHA; pass: $IN_PASS)."
  warn "Per-divergence detail:"
  # The harness writes failures to a fixed /tmp path that concurrent runs on a
  # shared host would clobber; copy it into this run's scratch before printing so
  # each run owns its artifact.
  FAIL_DETAIL="$SCRATCH/bts-failures.txt"
  cp /tmp/bts-failures.txt "$FAIL_DETAIL" 2>/dev/null || FAIL_DETAIL=/tmp/bts-failures.txt
  sed 's/^/  /' "$FAIL_DETAIL" >&2 2>/dev/null || true
  exit 1
fi
