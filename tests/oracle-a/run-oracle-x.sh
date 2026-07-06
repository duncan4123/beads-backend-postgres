#!/usr/bin/env bash
# Oracle X — CROSS-PLUMBING differential for the BD_SPIKE_UOWSTORE spike
# (gastownhall/beads#4547 Route A, slice 3).
#
# Oracle A compares two SEPARATELY BUILT bd binaries (before vs after) on the
# SAME plumbing (embedded). Oracle X is the orthogonal axis: ONE binary built
# from THIS working tree, run through TWO plumbings, to prove the spike's
# proxied-uowstore path is behaviorally equivalent to the ordinary embedded path
# over the gc-contract corpus.
#
#   REFERENCE  = the working-tree bd run NATIVELY (embedded, BD_SPIKE_UOWSTORE
#                unset) — exactly what the harness does for real bd. Goldens are
#                captured from it.
#   CANDIDATE  = a generated wrapper script (BTS_CANDIDATE) that, for the harness
#                `init -p <prefix> --quiet` bootstrap, stands up a PROXIED-SERVER
#                workspace the way cmd/bd/spike_uowstore_integration_test.go's
#                setupSpikeProxiedWorkspace does (metadata.json in proxied-server
#                mode + first-read boot of the managed proxy + child dolt
#                sql-server), and for EVERY other invocation execs the same bd
#                with the proxied env + BD_SPIKE_UOWSTORE=1 and args untouched.
#
# The harness `run_scenario` env_clear()s to {PATH,HOME,TMPDIR,BEADS_TEST_MODE=1},
# so the wrapper is the ONLY place the proxied env can be injected.
#
# Every divergence must be ATTRIBUTED (see CROSSPLUMB-REPORT.md). Class-(a)
# adapter bugs are NOT allowlistable — they are reported as findings.
#
# Usage:
#   tests/oracle-a/run-oracle-x.sh
#   BD_BIN=/abs/path/to/bd   tests/oracle-a/run-oracle-x.sh   # reuse a prebuilt bd (skip the ~1min build)
#   KEEP_ARTIFACTS=1         tests/oracle-a/run-oracle-x.sh   # keep the scratch dir
#
# Requirements: cargo (Rust), a CGO toolchain, go, dolt (in PATH), jq, mysql
# (for the commit-count observer). 43+ scenarios each boot a proxy + child dolt
# sql-server; the full capture+score can exceed 30 minutes.
#
# Exit status: 0 = run completed and every divergence is ATTRIBUTED — either an
#                  allowlist entry (`CROSSPLUMB-ALLOWLIST.md`, class b/c/d) or a
#                  named class-(a) finding row in `CROSSPLUMB-REPORT.md`;
#              1 = at least one UNATTRIBUTED divergence (a failing scenario named
#                  in neither doc), or the scoreboard itself crashed;
#              2 = setup/build error.
#
# The verdict section (step 7) machine-checks this: it parses the allowlisted
# scenario names from CROSSPLUMB-ALLOWLIST.md's `## AX-N — `name`` headers and the
# attributed scenario names from CROSSPLUMB-REPORT.md's attribution table, then
# refuses (exit 1) if any failing scenario is in neither. Nothing is trust-based.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HARNESS_DIR="$SCRIPT_DIR/harness"
REPO_ROOT="$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel)"

BUILD_TAGS="gms_pure_go"
RUN_ID="$(date +%Y%m%d-%H%M%S)-$$"
SCRATCH="${TMPDIR:-/tmp}/oracle-x-$RUN_ID"
# Scenario workspaces (and therefore the per-scenario proxy + child dolt
# sql-server data dirs) are forced under SCENWORK by exporting TMPDIR to it when
# we invoke the harness. That makes leaked servers reapable by an EXACT cmdline
# path match (this host runs many UNRELATED dolt sql-servers — never pkill by a
# bare "dolt sql-server").
SCENWORK="$SCRATCH/scenwork"
BD_DEFAULT="$SCRATCH/bd-worktree"
WRAPPER="$SCRATCH/bd-spike-proxied-wrapper.sh"
mkdir -p "$SCRATCH" "$SCENWORK"

log()  { printf '\033[1;36m[oracle-x]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[oracle-x]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[oracle-x]\033[0m %s\n' "$*" >&2; exit 2; }

# --- snapshot go.mod/go.sum BEFORE any build (never restore-to-HEAD) ------------------
# A build may rewrite go.mod/go.sum; the worktree deliberately carries an
# uncommitted go.mod edit, so we restore the pre-run BYTES, never `git checkout`.
GOMOD_SNAP="$SCRATCH/go.mod.snapshot"
GOSUM_SNAP="$SCRATCH/go.sum.snapshot"
[ -f "$REPO_ROOT/go.mod" ] && cp "$REPO_ROOT/go.mod" "$GOMOD_SNAP"
[ -f "$REPO_ROOT/go.sum" ] && cp "$REPO_ROOT/go.sum" "$GOSUM_SNAP"
restore_if_build_churned() {
  local snap="$1" live="$2"
  [ -f "$snap" ] || return 0
  if [ ! -f "$live" ] || ! cmp -s "$snap" "$live"; then
    cp "$snap" "$live"
  fi
}

# --- reap any proxy/child dolt processes rooted under our SCENWORK --------------------
# Match the FULL scenwork path in the process cmdline (db-proxy-child --root
# .../scenwork/... and dolt sql-server --config .../scenwork/...). This never
# touches unrelated dolt servers on the host.
reap_servers() {
  local pids
  pids="$(pgrep -f -- "$SCENWORK" 2>/dev/null | grep -v "^$$\$" || true)"
  [ -z "$pids" ] && return 0
  warn "reaping leaked proxy/dolt processes under $SCENWORK: $(echo "$pids" | tr '\n' ' ')"
  # shellcheck disable=SC2086
  kill $pids 2>/dev/null || true
  sleep 1
  pids="$(pgrep -f -- "$SCENWORK" 2>/dev/null | grep -v "^$$\$" || true)"
  # shellcheck disable=SC2086
  [ -n "$pids" ] && kill -9 $pids 2>/dev/null || true
}

cleanup() {
  local rc=$?
  reap_servers
  restore_if_build_churned "$GOMOD_SNAP" "$REPO_ROOT/go.mod"
  restore_if_build_churned "$GOSUM_SNAP" "$REPO_ROOT/go.sum"
  if [ "${KEEP_ARTIFACTS:-0}" = "1" ]; then
    warn "KEEP_ARTIFACTS=1 — leaving scratch at $SCRATCH"
  else
    rm -rf "$SCRATCH"
  fi
  return $rc
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# --- preflight -----------------------------------------------------------------------
command -v cargo >/dev/null 2>&1 || die "cargo not found (Rust toolchain required)"
command -v go    >/dev/null 2>&1 || die "go not found"
command -v gcc   >/dev/null 2>&1 || command -v cc >/dev/null 2>&1 || die "no C compiler (CGO required)"
command -v dolt  >/dev/null 2>&1 || die "dolt not found in PATH (the proxied child server needs it)"
command -v jq    >/dev/null 2>&1 || die "jq not found"
command -v mysql >/dev/null 2>&1 || warn "mysql client not found — the commit-count observer will be skipped"

# bd's workspace discovery walks UP from each scenario workspace looking for a
# .beads dir; a stale .beads on ANY ancestor of $SCENWORK silently hijacks every
# scenario (foreign DB, schema skew) and both the harness and wrapper swallow the
# init failure. The default scratch root is ${TMPDIR:-/tmp}, so on a host with a
# stale /tmp/.beads the whole run is invalid. Refuse to start if one exists.
assert_no_ancestor_beads() {
  local dir; dir="$(cd "$1" && pwd -P)"
  while :; do
    if [ -e "$dir/.beads" ]; then
      die "ancestor .beads hijack: $dir/.beads exists and bd would bind every scenario workspace under $SCENWORK to it. Remove it, or set TMPDIR to a directory with no .beads on any ancestor, then re-run."
    fi
    [ "$dir" = "/" ] && break
    dir="$(dirname "$dir")"
  done
}
assert_no_ancestor_beads "$SCENWORK"

# --- 1. the ONE binary: working-tree bd ----------------------------------------------
BD="${BD_BIN:-}"
if [ -n "$BD" ]; then
  [ -x "$BD" ] || die "BD_BIN=$BD is not executable"
  log "reusing prebuilt bd: $BD"
else
  log "building working-tree bd (CGO_ENABLED=1 -tags $BUILD_TAGS) ..."
  BD="$BD_DEFAULT"
  ( cd "$REPO_ROOT" && CGO_ENABLED=1 go build -tags "$BUILD_TAGS" -o "$BD" ./cmd/bd ) \
    || die "bd build failed"
  restore_if_build_churned "$GOMOD_SNAP" "$REPO_ROOT/go.mod"
  restore_if_build_churned "$GOSUM_SNAP" "$REPO_ROOT/go.sum"
  [ -x "$BD" ] || die "bd not produced at $BD"
fi
log "bd: $BD ($("$BD" version 2>/dev/null | head -1))"

# --- 2. generate the spike-proxied wrapper (the CANDIDATE) ----------------------------
# When the harness calls `<wrapper> init -p <prefix> --quiet`, DO NOT run bd init
# (the --proxied-server init gate stays down). Instead bootstrap the proxied
# workspace exactly like setupSpikeProxiedWorkspace, then let the first read boot
# the managed proxy + child dolt sql-server and auto-create the schema.
#
# Identity + hygiene (stated deviations from spike_uowstore_integration_test.go,
# required for a clean cross-plumbing diff — see CROSSPLUMB-REPORT.md §Wrapper):
#   * BEADS_ACTOR="CI Bot" + GIT_AUTHOR_EMAIL="ci@beads.test" — the spike test
#     forces actor=spiketester for symmetric two-spike comparison; here the
#     REFERENCE is captured by the plain harness under the host git identity
#     (CI Bot / ci@beads.test, which the harness normalizes to <ACTOR>/<EMAIL>),
#     so the CANDIDATE must mint the SAME identity, NOT spiketester.
#   * `git config --global beads.role maintainer` under HOME=$WS suppresses the
#     GH#2950 role warning the reference never emits (it resolves the host global
#     beads.role=maintainer; HOME=$WS hides it) — a wrapper artifact, killed at
#     source so it can't pollute the stderr diff.
#   * chmod 700 .beads matches bd init's perms (mkdir default 0775 warns).
# We intentionally DO NOT set BEADS_TEST_MODE for the proxied side: the spike
# boot path must construct a real managed server (BEADS_TEST_MODE flips
# store/topology construction off). This asymmetry is a documented mode
# difference (the reference IS run with BEADS_TEST_MODE=1 by the harness).
cat > "$WRAPPER" <<WRAPEOF
#!/usr/bin/env bash
# GENERATED by run-oracle-x.sh — the spike-proxied CANDIDATE plumbing.
set -uo pipefail
BD="$BD"
WS="\$PWD"
export HOME="\$WS"
export BEADS_DOLT_PROXIED_SERVER=1
export BEADS_NO_DAEMON=1
export BD_DISABLE_METRICS=1
export BD_DISABLE_EVENT_FLUSH=1
export BD_SPIKE_UOWSTORE=1
export BEADS_SKIP_IDENTITY_CHECK=1
export BEADS_ACTOR="CI Bot"
export GIT_AUTHOR_EMAIL="ci@beads.test"
unset BEADS_TEST_MODE

# sanitizePrefixForDB (mirror of cmd/bd/proxied_integration_helpers_test.go).
sanitize() {
  local p="\$1"
  p="\${p##.}"; p="\${p%%-}"; p="\${p//./_}"
  [ -z "\$p" ] && p=bd
  case "\$p" in [a-zA-Z_]*) ;; *) p="bd_\$p";; esac
  printf '%s' "\$p"
}

if [ "\${1:-}" = "init" ]; then
  # Extract --prefix / -p from the harness init argv.
  prefix=""; args=("\$@"); n=\${#args[@]}; i=0
  while [ \$i -lt \$n ]; do
    case "\${args[\$i]}" in
      -p|--prefix) j=\$((i+1)); [ \$j -lt \$n ] && prefix="\${args[\$j]}";;
    esac
    i=\$((i+1))
  done
  mkdir -p "\$WS/.beads"; chmod 700 "\$WS/.beads"
  git config --global beads.role maintainer >/dev/null 2>&1 || true
  db="\$(sanitize "\$prefix")"
  printf '{"database":"beads.db","dolt_mode":"proxied-server","dolt_database":"%s","project_id":"spike-%s"}' \
    "\$db" "\$prefix" > "\$WS/.beads/metadata.json"
  # First read boots the managed proxy + child dolt sql-server + auto-creates schema.
  "\$BD" list --json >/dev/null 2>&1 || true
  # Seed issue_prefix the way the task specifies. NOTE: bd rejects issue_prefix as
  # a protected key (cmd/bd/config.go rejectProtectedConfigKey) in BOTH plumbings,
  # so this is a no-op reject; seeding is unnecessary because every corpus create
  # passes an explicit --id and ValidateIDPrefixAllowed short-circuits on an empty
  # dbPrefix. Kept for fidelity to the brief; failure is swallowed.
  "\$BD" config set issue_prefix "\$prefix" >/dev/null 2>&1 || true
  exit 0
fi

# Every other invocation: the real bd, proxied env + spike flag, args untouched.
exec "\$BD" "\$@"
WRAPEOF
chmod +x "$WRAPPER"
log "wrapper: $WRAPPER"

# --- 3. build the vendored harness ---------------------------------------------------
log "building conformance harness ..."
( cd "$HARNESS_DIR" && cargo build --release --bins ) >/dev/null 2>&1 \
  || die "harness build failed (run 'cargo build --release --bins' in $HARNESS_DIR)"
CAPTURE="$HARNESS_DIR/target/release/capture_golden"
SCOREBOARD="$HARNESS_DIR/target/release/scoreboard"

rm -rf "$HARNESS_DIR/testdata/golden"

# --- 4. capture goldens from the REFERENCE (embedded, flag off) ----------------------
# TMPDIR=$SCENWORK forces scenario workspaces under our scratch. The reference is
# embedded (no server), so nothing to reap here.
log "capturing goldens from REFERENCE (embedded working-tree bd) ..."
TMPDIR="$SCENWORK" BTS_REFERENCE_BD="$BD" "$CAPTURE" \
  || die "golden capture failed"

# --- 4b. golden floor: reference create steps must actually work ---------------------
GOLDEN_DIR="$HARNESS_DIR/testdata/golden"
floor_violations=0
shopt -s nullglob
for trace in "$GOLDEN_DIR"/*.trace.json; do
  scen="$(basename "$trace" .trace.json)"
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
[ "$floor_violations" -gt 0 ] && die "golden floor FAILED ($floor_violations scenario(s)) — refusing to score."
log "golden floor OK."

# --- 5. score the CANDIDATE (spike-proxied wrapper) ----------------------------------
# 43+ scenarios each boot a proxy + child dolt sql-server — slow. TMPDIR=$SCENWORK
# so every server's data dir (and cmdline) lives under our scratch for reaping.
log "scoring CANDIDATE (spike-proxied) — booting a proxy+child per scenario, this is slow ..."
SCORE_OUT="$SCRATCH/scoreboard.out"
set +e
TMPDIR="$SCENWORK" BTS_CANDIDATE="$WRAPPER" "$SCOREBOARD" | tee "$SCORE_OUT"
score_rc=${PIPESTATUS[0]}
set -e
reap_servers
# A scoreboard crash must not masquerade as a clean run (the `| tee` used to hide
# it). Surface it in the verdict rather than swallowing it here.
[ "$score_rc" -ne 0 ] && warn "scoreboard exited non-zero ($score_rc) — verdict will fail the run."

# preserve the per-failure detail this run produced (scoreboard writes a fixed path).
FAIL_DETAIL="$SCRATCH/crossplumb-failures.txt"
cp /tmp/bts-failures.txt "$FAIL_DETAIL" 2>/dev/null || true

# --- 6. commit-count observer (create -> update -> close on both plumbings) -----------
# Counts dolt commits per command. Embedded: `dolt log` against
# .beads/embeddeddolt/<db> between commands (no server holds the lock). Proxied:
# the child dolt sql-server stays up, so query it live —
# `SELECT COUNT(*) FROM dolt_log` over the port recorded in proxieddb/proxy.pid.
OBS_OUT="$SCRATCH/commit-observer.txt"
commit_observer() {
  command -v mysql >/dev/null 2>&1 || { echo "mysql client unavailable — observer skipped"; return 0; }
  local root="$SCENWORK/observer"; rm -rf "$root"; mkdir -p "$root"
  # EMBEDDED
  local emb="$root/emb"; mkdir -p "$emb"
  ( cd "$emb" && git init -q && git config user.email test@test.com && git config user.name Test )
  ebd() { ( cd "$emb" && env -i PATH="$PATH" HOME="$emb" BEADS_DOLT_AUTO_START=0 BEADS_NO_DAEMON=1 \
      BD_DISABLE_METRICS=1 BD_DISABLE_EVENT_FLUSH=1 BEADS_ACTOR="CI Bot" GIT_AUTHOR_EMAIL="ci@beads.test" \
      BEADS_SKIP_IDENTITY_CHECK=1 "$BD" "$@" ); }
  ecount() { ( cd "$emb/.beads/embeddeddolt/obs" 2>/dev/null && dolt log --oneline 2>/dev/null | wc -l ) || echo 0; }
  ebd init --quiet -p obs >/dev/null 2>&1
  local e0 e1 e2 e3; e0=$(ecount)
  ebd create "T" --id obs-1 --force -t task -p 1 --json >/dev/null 2>&1; e1=$(ecount)
  ebd update obs-1 --status in_progress --json      >/dev/null 2>&1; e2=$(ecount)
  ebd close  obs-1 --force --json                   >/dev/null 2>&1; e3=$(ecount)
  # PROXIED
  local prx="$root/prx"; mkdir -p "$prx"
  pw() { ( cd "$prx" && env -i PATH="$PATH" HOME=/root TMPDIR="$SCENWORK" BEADS_TEST_MODE=1 "$WRAPPER" "$@" ); }
  pw init -p obs --quiet >/dev/null 2>&1
  local port; port=$(jq -r '.port' "$prx/.beads/proxieddb/proxy.pid" 2>/dev/null || echo "")
  pcount() { [ -n "$port" ] && mysql -h 127.0.0.1 -P "$port" -u root obs -N -e "SELECT COUNT(*) FROM dolt_log" 2>/dev/null || echo 0; }
  local p0 p1 p2 p3; p0=$(pcount)
  pw create "T" --id obs-1 --force -t task -p 1 --json >/dev/null 2>&1; p1=$(pcount)
  pw update obs-1 --status in_progress --json      >/dev/null 2>&1; p2=$(pcount)
  pw close  obs-1 --force --json                   >/dev/null 2>&1; p3=$(pcount)
  {
    echo "command   embedded(delta)   spike-proxied(delta)"
    echo "create    $e1 (+$((e1-e0)))         $p1 (+$((p1-p0)))"
    echo "update    $e2 (+$((e2-e1)))         $p2 (+$((p2-p1)))"
    echo "close     $e3 (+$((e3-e2)))         $p3 (+$((p3-p2)))"
    echo "base(init+schema): embedded=$e0  spike-proxied=$p0"
  }
}
log "running commit-count observer ..."
commit_observer | tee "$OBS_OUT" || true
reap_servers

# --- 7. verdict ----------------------------------------------------------------------
IN_LINE="$(grep -E '^\s*PASS:.*FAIL:' "$SCORE_OUT" | head -1)"
IN_PASS="$(printf '%s' "$IN_LINE" | sed -E 's/.*PASS:\s*([0-9]+).*/\1/')"
IN_FAIL="$(printf '%s' "$IN_LINE" | sed -E 's/.*FAIL:\s*([0-9]+).*/\1/')"

echo
log "IN-SCOPE PASS=$IN_PASS FAIL=$IN_FAIL"
log "scoreboard : $SCORE_OUT"
log "failures   : $FAIL_DETAIL"
log "observer   : $OBS_OUT"

# --- machine-checked attribution gate ------------------------------------------------
# Every FAILING scenario must be ATTRIBUTED: named either in an allowlist header
# (`## AX-N — `name``) or in the report's attribution table (`| N | name | …`). A
# failing scenario named in neither is an UNATTRIBUTED divergence and fails the run.
ALLOWLIST_MD="$SCRIPT_DIR/CROSSPLUMB-ALLOWLIST.md"
REPORT_MD="$SCRIPT_DIR/CROSSPLUMB-REPORT.md"

attributed="$(
  { grep -oE '^## AX-[0-9]+ — `[^`]+`' "$ALLOWLIST_MD" 2>/dev/null \
      | sed -E 's/.*`([^`]+)`.*/\1/'
    grep -oE '^\| *[0-9]+ *\| *[a-zA-Z0-9_]+ *\|' "$REPORT_MD" 2>/dev/null \
      | sed -E 's/^\| *[0-9]+ *\| *([a-zA-Z0-9_]+).*/\1/'
  } | sort -u
)"

failing=""
if [ -s "$FAIL_DETAIL" ]; then
  failing="$(sed -E 's/ +— .*//; s/[[:space:]]+$//' "$FAIL_DETAIL" | grep -v '^$' | sort -u)"
fi

# Stale attribution: an allowlisted/attributed scenario that did NOT diverge.
# Informational only (a fixed adapter gap) — never fails the run.
while IFS= read -r scen; do
  [ -z "$scen" ] && continue
  printf '%s\n' "$failing" | grep -qxF "$scen" || \
    warn "attributed scenario '$scen' did not diverge this run (fixed? prune it from the allowlist/report)."
done <<ATTR
$attributed
ATTR

unattributed=""
while IFS= read -r scen; do
  [ -z "$scen" ] && continue
  printf '%s\n' "$attributed" | grep -qxF "$scen" || unattributed="$unattributed $scen"
done <<FAILS
$failing
FAILS

rc=0
if [ -n "${unattributed// /}" ]; then
  warn "UNATTRIBUTED cross-plumbing divergence(s) — in neither CROSSPLUMB-ALLOWLIST.md nor CROSSPLUMB-REPORT.md:"
  for s in $unattributed; do warn "  - $s"; done
  warn "attribute each as an allowlist entry (class b/c/d) or a report finding row (class a), then re-run."
  rc=1
fi
if [ "${score_rc:-0}" -ne 0 ]; then
  warn "scoreboard crashed (exit $score_rc) — run invalid."
  rc=1
fi
[ "$rc" -eq 0 ] && log "attribution gate OK — every divergence is attributed."
exit "$rc"
