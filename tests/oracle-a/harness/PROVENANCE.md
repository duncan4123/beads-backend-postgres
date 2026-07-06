# Harness provenance

The Rust sources under `src/` are a vendored copy of the bts-rs conformance
harness, **byte-identical to upstream except for the enumerated local deltas
below**:

- Upstream path: `/data/projects/bts-rs/crates/bts-conformance/src/`
- Upstream commit: `dffbcd9f4eb14457328dba50b6e196c39a441bcd`
- Files and their status:
  - `src/lib.rs` — crate root — **verbatim**
  - `src/bin/capture_golden.rs` — golden capture (`BTS_REFERENCE_BD`, `BTS_ONLY=`) — **verbatim**
  - `src/differential.rs` — scenario runner, `normalize()`, JSON-aware `diff()` — **local delta** (env scrub, see below)
  - `src/scenarios.rs` — curated `all()` + optional `catalog()` loader — **local delta** (added scenarios, see below)
  - `src/bin/scoreboard.rs` — candidate scorer, in-scope predicate — **local delta** (scope table, see below)

Only `Cargo.toml` is otherwise local (standalone deps; see its header comment).

## Local deltas (Oracle A adversarial-review fixes)

These diverge from upstream deliberately. On re-sync (below), re-apply them or
re-derive from this list:

1. **`differential.rs` — `run_scenario` env scrub.** Upstream inherits the FULL
   host environment; the run now `env_clear()`s and passes an explicit whitelist
   (`PATH`/`HOME`/`TMPDIR` + `BEADS_TEST_MODE=1`). Reason: host `BEADS_*`/`BD_*`
   vars silently reshape both binaries symmetrically, so the gate would certify a
   different configuration than users run. The `BEADS_TEST_MODE=1` store-construction
   delta is a documented ungated gap (see `../README.md`).
2. **`scoreboard.rs` — in-scope predicate.** Added `--label` to `IN_SCOPE_FLAGS`
   (the `labels` scenario's `list --label` filter was silently ungated), and added
   `comments`/`get` to `IN_SCOPE_CMDS` plus `comment`/`comments` to
   `JSON_OUTPUT_CMDS` for the coverage scenarios below.
3. **`scenarios.rs` — added coverage scenarios** (curated `all()`):
   `delete_unblocks_neighbour`, `comment_add_list`, `config_set_get_success`,
   `purge_real_then_reseed` — closing the delete / comment / config-success /
   real-purge gaps the review flagged.

If these deltas are upstreamed to bts-rs, drop them from this list on the next
re-sync.

## Why vendored instead of building bts-rs in place

Oracle A must **re-capture goldens from a fresh origin/main-built `bd`** into a
directory it owns, and must never modify or commit inside `/data/projects/bts-rs`.
`capture_golden`/`scoreboard` hardcode the golden dir to
`CARGO_MANIFEST_DIR/testdata/golden`. Building from a vendored copy makes
`CARGO_MANIFEST_DIR` point at *this* directory, so goldens land in
`tests/oracle-a/harness/testdata/golden/` (git-ignored, regenerated each run)
without touching bts-rs testdata.

## Re-syncing from upstream

If the upstream harness changes and you want to pull it in:

```sh
UP=/data/projects/bts-rs/crates/bts-conformance/src
cp "$UP"/differential.rs "$UP"/scenarios.rs "$UP"/lib.rs  tests/oracle-a/harness/src/
cp "$UP"/bin/capture_golden.rs "$UP"/bin/scoreboard.rs    tests/oracle-a/harness/src/bin/
# then update the commit hash above; leave Cargo.toml as-is.
```

The `scenarios::catalog()` loader reads `../../docs/scenarios/enumerated.json`
relative to `CARGO_MANIFEST_DIR`. That path does not exist here, so `catalog()`
returns empty (handled gracefully). Oracle A intentionally runs only the curated
`all()` set (no `BTS_CATALOG`), which is the in-scope gc-contract surface.
