//! Run EVERY scenario (curated `all()` + enumerated `catalog()`) against the
//! bts-rs candidate, diff each against its captured bd golden, and print a
//! pass/fail scoreboard — the exhaustive-wiring backlog.
//!
//!   BTS_CANDIDATE=/abs/bts BTS_DATABASE_URL=postgres://... \
//!     cargo run -p bts-conformance --bin scoreboard
//!
//! Goldens must exist (run `BTS_CATALOG=1 BTS_REFERENCE_BD=... capture_golden`
//! first). Scenarios without a golden are reported as `no-golden` (skipped).

use bts_conformance::differential::{diff, run_scenario, Trace};
use bts_conformance::scenarios;
use std::collections::BTreeMap;
use std::path::PathBuf;

/// gc-contract commands (everything else is out-of-scope bd surface).
const IN_SCOPE_CMDS: &[&str] = &[
    "init", "create", "show", "list", "ready", "update", "close", "reopen", "delete", "purge",
    "dep", "count", "query", "config", "version", "sql", "comment", "comments", "add", "remove",
    "set", "get",
];
/// gc-contract flags (denominator §B + globals). A scenario is in-scope iff every
/// command is in IN_SCOPE_CMDS and every flag is here.
const IN_SCOPE_FLAGS: &[&str] = &[
    "--id", "--force", "-p", "--priority", "-t", "--type", "-d", "--description", "--json",
    "-a", "--assignee", "--deps", "-l", "--labels", "--label", "--metadata", "--metadata-field",
    "--parent",
    "--ephemeral", "--no-history", "--defer", "--graph", "--title", "--all", "-n", "--limit",
    "-s", "--status", "--created-before", "--include-infra", "--include-gates",
    "--include-templates", "--skip-labels", "--include-ephemeral", "-u", "--unassigned", "--claim",
    "--add-label", "--remove-label", "--set-metadata", "-r", "--reason", "--dry-run", "-q",
    "--quiet",
];

/// Commands whose output Gas City parses — gc ALWAYS passes `--json` to these,
/// so a scenario exercising their human/plain output is testing something gc
/// never consumes (out of scope). `dep add`/`remove`, `config set`, `init` are
/// excluded: gc calls those without `--json`.
const JSON_OUTPUT_CMDS: &[&str] = &[
    "create", "show", "list", "ready", "count", "update", "close", "reopen", "delete", "purge",
    "query", "version", "stats", "comment", "comments",
];

fn in_scope(sc: &bts_conformance::differential::Scenario) -> bool {
    for step in &sc.steps {
        let cmd = match step.first() {
            Some(c) => c.as_str(),
            None => continue,
        };
        if !IN_SCOPE_CMDS.contains(&cmd) {
            return false;
        }
        // gc-parsed commands must be exercised in --json mode to be in scope.
        if JSON_OUTPUT_CMDS.contains(&cmd) && !step.iter().any(|t| t == "--json") {
            return false;
        }
        for tok in step.iter().filter(|t| t.starts_with('-')) {
            let name = tok.split('=').next().unwrap_or(tok);
            if !IN_SCOPE_FLAGS.contains(&name) {
                return false;
            }
        }
    }
    true
}

fn main() {
    let bin = match std::env::var("BTS_CANDIDATE") {
        Ok(p) => PathBuf::from(p),
        Err(_) => {
            eprintln!("set BTS_CANDIDATE (+ BTS_DATABASE_URL)");
            std::process::exit(2);
        }
    };
    let golden_dir = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("testdata/golden");

    let mut scens = scenarios::all();
    scens.extend(scenarios::catalog());

    let (mut in_pass, mut in_fail, mut oos_pass, mut oos_fail, mut nogolden) = (0u32, 0, 0, 0, 0);
    let mut in_fail_by_verb: BTreeMap<String, u32> = BTreeMap::new();
    let mut in_first_fails: Vec<String> = Vec::new();

    for sc in &scens {
        let gpath = golden_dir.join(format!("{}.trace.json", sc.name));
        let golden: Trace = match std::fs::read_to_string(&gpath).ok().and_then(|s| serde_json::from_str(&s).ok()) {
            Some(t) => t,
            None => {
                nogolden += 1;
                continue;
            }
        };
        let scope = in_scope(sc);
        let passed = match run_scenario(&bin, sc) {
            Ok(t) => diff(&golden, &t, sc.ordered).is_empty(),
            Err(_) => false,
        };
        match (scope, passed) {
            (true, true) => in_pass += 1,
            (true, false) => {
                in_fail += 1;
                let verb = sc.steps.iter().flatten().find(|t| !t.starts_with('-')).cloned().unwrap_or_default();
                *in_fail_by_verb.entry(verb).or_default() += 1;
                let d = run_scenario(&bin, sc)
                    .ok()
                    .map(|t| diff(&golden, &t, sc.ordered))
                    .and_then(|v| v.into_iter().next())
                    .map(|d| d.to_string().replace('\n', " | "))
                    .unwrap_or_else(|| "run error".into());
                in_first_fails.push(format!("{} — {d}", sc.name));
            }
            (false, true) => oos_pass += 1,
            (false, false) => oos_fail += 1,
        }
    }

    println!("\n=== bts-rs scoreboard ===");
    println!("total scenarios: {}  (no-golden: {nogolden})", scens.len());
    println!("\nIN-SCOPE (gc contract) — the pass target:");
    println!("  PASS: {in_pass}   FAIL: {in_fail}   ({:.0}%)", 100.0 * in_pass as f64 / (in_pass + in_fail).max(1) as f64);
    println!("\nOUT-OF-SCOPE (bd-only features bts-rs intentionally omits):");
    println!("  pass: {oos_pass}   fail: {oos_fail}  (informational; not a target)");
    println!("\nin-scope failures by command:");
    for (verb, n) in &in_fail_by_verb {
        println!("  {verb:<12} {n}");
    }
    // Dump ALL in-scope failures to a file for offline categorization.
    let dump = PathBuf::from("/tmp/bts-failures.txt");
    let _ = std::fs::write(&dump, in_first_fails.join("\n"));
    println!("\nall {} in-scope failures written to {}", in_first_fails.len(), dump.display());
}
