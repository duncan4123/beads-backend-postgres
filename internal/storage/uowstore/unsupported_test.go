package uowstore

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
)

// These unit tests need no env gates (nil provider is safe — the stub methods
// and the empty-message guard never touch it), so they run in ordinary CI and
// pin the typed-unsupported error contract from the design §2.6.

const wantFullSuffix = " (BD_SPIKE_UOWSTORE spike shell, gastownhall/beads#4547)"

func TestUnsupportedShell_TypedError(t *testing.T) {
	ctx := context.Background()
	st := New(nil, "t")

	cases := []struct {
		name    string
		call    func() error
		wantOp  string
		wantMsg string
	}{
		{
			name:    "store-level AddLabel",
			call:    func() error { return st.AddLabel(ctx, "x", "y", "z") },
			wantOp:  "AddLabel",
			wantMsg: `operation "AddLabel" not supported by the uowstore spike backend` + wantFullSuffix,
		},
		{
			name:    "store-level Commit (flushBatchCommitOnShutdown hazard)",
			call:    func() error { return st.Commit(ctx, "m") },
			wantOp:  "Commit",
			wantMsg: `operation "Commit" not supported by the uowstore spike backend` + wantFullSuffix,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatalf("expected an error, got nil")
			}
			var target *storage.ErrUnsupported
			if !errors.As(err, &target) {
				t.Fatalf("errors.As(*storage.ErrUnsupported) failed for %v", err)
			}
			if target.Op != tc.wantOp {
				t.Errorf("Op = %q, want %q", target.Op, tc.wantOp)
			}
			if target.Backend != SpikeBackend {
				t.Errorf("Backend = %q, want %q", target.Backend, SpikeBackend)
			}
			if got := err.Error(); got != tc.wantMsg {
				t.Errorf("full message mismatch:\n got  %q\n want %q", got, tc.wantMsg)
			}
		})
	}
}

func TestUnsupportedShell_TxTypedError(t *testing.T) {
	ctx := context.Background()
	// Exercise the generated Transaction shell directly: CreateIssues is a stub
	// that returns the typed error with the "Transaction." Op prefix.
	err := unsupportedTransaction{}.CreateIssues(ctx, nil, "actor")
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var target *storage.ErrUnsupported
	if !errors.As(err, &target) {
		t.Fatalf("errors.As(*storage.ErrUnsupported) failed for %v", err)
	}
	if target.Op != "Transaction.CreateIssues" {
		t.Errorf("Op = %q, want %q", target.Op, "Transaction.CreateIssues")
	}
	if target.Backend != SpikeBackend {
		t.Errorf("Backend = %q, want %q", target.Backend, SpikeBackend)
	}
	wantMsg := `operation "Transaction.CreateIssues" not supported by the uowstore spike backend` + wantFullSuffix
	if got := err.Error(); got != wantMsg {
		t.Errorf("full message mismatch:\n got  %q\n want %q", got, wantMsg)
	}
}
