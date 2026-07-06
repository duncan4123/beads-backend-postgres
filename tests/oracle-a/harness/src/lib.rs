//! Conformance harness for bts-rs.
//!
//! Two layers validate that bts-rs honors the beads contract Gas City depends on:
//!
//! - **Static corpus** (`tests/corpus_test.rs`): the vendored beads corpus is
//!   byte-stable golden JSON; we assert our canonicalizer reproduces it. Fast,
//!   but narrow — see `docs/CONFORMANCE_GAPS.md` for what it does NOT pin.
//! - **Live differential** (`differential` + `scenarios`): run gap-targeting
//!   command sequences against BOTH real `bd` and `bts-rs` in isolated
//!   workspaces and diff observable behavior (stdout/stderr/exit), with
//!   array-order-significant JSON comparison. This is the real parity oracle.

pub mod differential;
pub mod scenarios;

/// Path to the vendored static corpus, relative to this crate.
pub const CORPUS_REL: &str = "testdata/corpus";

/// Directory holding captured golden traces from the reference bd.
pub const GOLDEN_REL: &str = "testdata/golden";
