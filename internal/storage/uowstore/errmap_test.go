package uowstore

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
)

// No env gates: mapUowError is pure. This pins the load-bearing §6.1 contract —
// EVERY override routes not-found through here, so errors.Is(err, ErrNotFound)
// must hold and the raw "db: …Repository.…: sql: no rows" wrapper must not leak.
func TestMapUowError_NotFoundContract(t *testing.T) {
	t.Run("nil passes through", func(t *testing.T) {
		if got := mapUowError(nil); got != nil {
			t.Errorf("mapUowError(nil) = %v, want nil", got)
		}
	})

	t.Run("wrapped sql.ErrNoRows becomes ErrNotFound", func(t *testing.T) {
		// The shape the domain repositories emit: a db-prefixed wrap of sql.ErrNoRows.
		leak := fmt.Errorf("db: IssueSQLRepository.Close bd-x: %w", sql.ErrNoRows)
		got := mapUowError(leak)
		if !errors.Is(got, storage.ErrNotFound) {
			t.Fatalf("errors.Is(mapUowError(...), ErrNotFound) = false; err = %v", got)
		}
		if msg := got.Error(); wantNoDBPrefix(msg) == false {
			t.Errorf("mapped error still leaks db-prefixed text: %q", msg)
		}
	})

	t.Run("already ErrNotFound passes through unchanged", func(t *testing.T) {
		in := fmt.Errorf("%w: issue bd-x", storage.ErrNotFound)
		got := mapUowError(in)
		if !errors.Is(got, storage.ErrNotFound) {
			t.Fatalf("errors.Is = false for pre-translated ErrNotFound: %v", got)
		}
		if got.Error() != in.Error() {
			t.Errorf("pre-translated ErrNotFound was rewritten: got %q want %q", got.Error(), in.Error())
		}
	})

	t.Run("non-not-found error is returned verbatim", func(t *testing.T) {
		other := errors.New("db: some infra failure")
		got := mapUowError(other)
		if got != other {
			t.Errorf("mapUowError rewrote a non-not-found error: got %v want %v", got, other)
		}
		if errors.Is(got, storage.ErrNotFound) {
			t.Errorf("non-not-found error mis-classified as ErrNotFound: %v", got)
		}
	})
}

// wantNoDBPrefix reports whether msg is free of the raw "db:" repository prefix
// the store contract never emits.
func wantNoDBPrefix(msg string) bool {
	return msg == storage.ErrNotFound.Error()
}
