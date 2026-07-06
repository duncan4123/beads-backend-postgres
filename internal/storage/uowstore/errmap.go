package uowstore

import (
	"database/sql"
	"errors"

	"github.com/steveyegge/beads/internal/storage"
)

// mapUowError translates a use-case/domain error into the storage.DoltStorage
// contract's error vocabulary (SPIKE-REPORT §6.1). EVERY override in this
// package routes its outgoing error through this helper so the adapter presents
// one consistent not-found contract instead of the per-method leakage the
// census cataloged:
//
//   - The domain repositories wrap a bare sql.ErrNoRows for a missing row (e.g.
//     "db: IssueSQLRepository.Close …: sql: no rows in result set"). The store
//     contract requires storage.ErrNotFound so that errors.Is(err,
//     storage.ErrNotFound) holds for direct consumers AND so the raw
//     "db: …Repository.…" text — which the embedded store never emits — does
//     not leak into the differential harness. We therefore collapse any
//     not-found to the bare sentinel, dropping the db-prefixed wrapper.
//   - An error that is ALREADY storage.ErrNotFound (getIssueInUOW's translated,
//     wisp-fallback result) is passed through unchanged; re-wrapping would only
//     re-add noise.
//   - Every other error (genuine infra failure) is returned verbatim. These are
//     rare, not the silent-divergence class the harness targets, and sanitizing
//     arbitrary infra text would risk hiding real faults.
//
// The wisp fallback itself is NOT handled here — it is specific to issue
// resolution and lives in getIssueInUOW; mapUowError only owns the error-code
// translation shared by every override.
func mapUowError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		return storage.ErrNotFound
	}
	return err
}
