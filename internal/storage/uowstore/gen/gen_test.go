package main

// Regen-idempotence gate for the typed-unsupported shell.
//
// The two compile-time assertions inside unsupported_gen.go (var _
// storage.DoltStorage = unsupportedDoltStorage{}, likewise for Transaction)
// already catch interface GROWTH and a hand-DELETED stub — the shell type must
// satisfy the full interface, so a dropped method breaks the build. What nothing
// else catches is a hand-edited stub BODY: a typo'd Op string, or a stub changed
// to return nil instead of the typed error. That is the drift class this test
// pins — regenerate to a temp path and byte-compare against the committed file,
// the same drift-check idiom scripts/check-cli-docs-drift.sh uses for CLI docs.
//
// It is an ordinary (ungated) unit test so plain `go test ./...` runs it; it
// needs no DB, no env, and no cgo — run() is pure stdlib go/ast codegen.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGeneratedShellIsUpToDate(t *testing.T) {
	// This test runs with CWD == the gen package dir, so the storage package is
	// two levels up and the committed shell is one level up.
	const (
		srcDir      = "../.."
		committedGo = "../" + defaultOutFile
	)

	want, err := os.ReadFile(committedGo)
	if err != nil {
		t.Fatalf("read committed %s: %v", committedGo, err)
	}

	// Regenerate into a temp file so a stale/edited committed copy is never
	// clobbered by the check itself.
	tmp := filepath.Join(t.TempDir(), defaultOutFile)
	if err := run(srcDir, tmp); err != nil {
		t.Fatalf("run(gen): %v", err)
	}
	got, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("read regenerated %s: %v", tmp, err)
	}

	if string(got) != string(want) {
		t.Fatalf("%s is stale or hand-edited: it does not match a fresh `go generate ./...`.\n"+
			"Regenerate it (do not hand-edit the DO-NOT-EDIT file) and commit the result.\n"+
			"committed length=%d, regenerated length=%d", defaultOutFile, len(want), len(got))
	}
}
