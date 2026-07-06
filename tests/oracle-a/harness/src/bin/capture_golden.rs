//! Capture golden traces from a reference `bd` binary.
//!
//!   BTS_REFERENCE_BD=/path/to/bd cargo run -p bts-conformance --bin capture_golden
//!
//! Writes one `<scenario>.trace.json` per scenario under `testdata/golden/`.
//! These are version-controlled ground truth: the real behavior of bd that the
//! bts-rs differential test will diff against.

use bts_conformance::differential::{run_scenario, Trace};
use bts_conformance::scenarios;
use std::path::PathBuf;

fn main() {
    let bd = match std::env::var("BTS_REFERENCE_BD") {
        Ok(p) => PathBuf::from(p),
        Err(_) => {
            eprintln!("set BTS_REFERENCE_BD=/path/to/bd");
            std::process::exit(2);
        }
    };
    let golden_dir = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("testdata/golden");
    std::fs::create_dir_all(&golden_dir).expect("create golden dir");

    let mut scens = scenarios::all();
    if std::env::var("BTS_CATALOG").is_ok() {
        scens.extend(scenarios::catalog());
    }
    // BTS_ONLY=name1,name2 re-captures just those scenarios (no churn elsewhere).
    if let Ok(only) = std::env::var("BTS_ONLY") {
        let want: Vec<String> = only.split(',').map(|s| s.trim().to_string()).collect();
        scens.retain(|s| want.contains(&s.name));
    }
    eprintln!("capturing {} scenarios...", scens.len());

    let mut failures = 0;
    for sc in scens {
        match run_scenario(&bd, &sc) {
            Ok(trace) => {
                let path = golden_dir.join(format!("{}.trace.json", sc.name));
                let json = serde_json::to_string_pretty(&trace).expect("serialize trace");
                std::fs::write(&path, json + "\n").expect("write golden");
                let exits: Vec<String> = trace.steps.iter().map(|s| s.exit.to_string()).collect();
                println!(
                    "captured {:<18} {} steps  exits=[{}]",
                    sc.name,
                    trace.steps.len(),
                    exits.join(",")
                );
            }
            Err(e) => {
                eprintln!("FAILED {}: {e}", sc.name);
                failures += 1;
            }
        }
    }
    // Touch Trace so the type is exercised even if scenarios is empty.
    let _ = std::mem::size_of::<Trace>();
    if failures > 0 {
        std::process::exit(1);
    }
}
