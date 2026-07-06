package uowstore

import (
	"context"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
)

// No env gates: a nil provider is safe because the guard returns before
// uow.RunInTx would call provider.NewUOW. See design §3.1.

func TestRunInTransaction_EmptyMessageGuard(t *testing.T) {
	ctx := context.Background()
	// nil provider: if the guard did NOT fire first, uow.RunInTx would call
	// provider.NewUOW on nil and panic — so a clean error return also proves the
	// guard runs before any UOW is opened.
	st := New(nil, "t")

	called := false
	err := st.RunInTransaction(ctx, "  ", func(tx storage.Transaction) error {
		called = true
		return nil
	})
	if err == nil {
		t.Fatal("expected the empty-message guard error, got nil")
	}
	if called {
		t.Error("fn was called; the guard must fire before fn runs")
	}
	const wantMsg = "uowstore spike: RunInTransaction requires a non-empty commit message; deferred (blank-message) version commits are embedded-only (gastownhall/beads#4547)"
	if got := err.Error(); got != wantMsg {
		t.Errorf("guard message mismatch:\n got  %q\n want %q", got, wantMsg)
	}
}
