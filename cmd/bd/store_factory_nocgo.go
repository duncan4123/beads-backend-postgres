//go:build !cgo

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/dbproxy/util"
	"github.com/steveyegge/beads/internal/storage/dolt"
	beadsmysql "github.com/steveyegge/beads/internal/storage/mysql"
	"github.com/steveyegge/beads/internal/storage/postgres"
	beadssqlite "github.com/steveyegge/beads/internal/storage/sqlite"
	"github.com/steveyegge/beads/internal/storage/uowstore"
)

func usesSQLServer() bool {
	return true
}

// isEmbeddedMode reports whether the command is using embedded Dolt storage.
func isEmbeddedMode() bool {
	return false
}

// spikeUOWStore mirrors the cgo build's flag (see store_factory.go). Kept in
// both build tags so main.go compiles either way; behavior default-off.
func spikeUOWStore() bool {
	return os.Getenv("BD_SPIKE_UOWSTORE") == "1"
}

// newSpikeUOWStore builds the spike uowstore adapter over a proxied-server UOW
// provider for beadsDir (issue #4547 Route A derisk).
func newSpikeUOWStore(ctx context.Context, beadsDir string) (storage.DoltStorage, error) {
	provider, err := newProxiedServerUOWProvider(ctx, beadsDir)
	if err != nil {
		return nil, fmt.Errorf("spike uowstore: open uow provider: %w", err)
	}
	return uowstore.New(provider, getActor()), nil
}

func usesProxiedServer() bool {
	if spikeUOWStore() {
		return false
	}
	if shouldUseGlobals() {
		return proxiedServerMode
	}
	return cmdCtx != nil && cmdCtx.ProxiedServerMode
}

func newDoltStore(ctx context.Context, cfg *dolt.Config) (storage.DoltStorage, error) {
	if cfg.ProxiedServer {
		if spikeUOWStore() {
			return newSpikeUOWStore(ctx, cfg.BeadsDir)
		}
		// TODO: this should not be a store
		// it should be a uow provider
		return nil, fmt.Errorf("proxy server store should be uow provider")
	}
	if !cfg.ServerMode {
		return nil, fmt.Errorf("%s", nocgoEmbeddedErrMsg)
	}
	return dolt.New(ctx, cfg)
}

// acquireEmbeddedLock returns a no-op lock in non-CGO builds.
func acquireEmbeddedLock(_ string, _ bool) (util.Unlocker, error) {
	return util.NoopLock{}, nil
}

// newDoltStoreFromConfig creates a SQL-server-backed storage backend from config.
func newDoltStoreFromConfig(ctx context.Context, beadsDir string) (storage.DoltStorage, error) {
	cfg, err := configfile.Load(beadsDir)
	if err == nil && cfg != nil && cfg.GetBackend() == configfile.BackendPostgres {
		// Postgres needs no CGO (pure-Go pgx), so it works in the nocgo build too.
		return postgres.NewFromConfig(ctx, beadsDir)
	}
	if err == nil && cfg != nil && cfg.GetBackend() == configfile.BackendMySQL {
		// MySQL (go-sql-driver) needs no CGO either.
		return beadsmysql.NewFromConfig(ctx, beadsDir)
	}
	if err == nil && cfg != nil && cfg.GetBackend() == configfile.BackendSQLite {
		// SQLite (modernc.org/sqlite) is pure-Go; no CGO.
		return beadssqlite.NewFromConfig(ctx, beadsDir)
	}
	if err == nil && cfg != nil && cfg.IsDoltProxiedServerMode() {
		if spikeUOWStore() {
			return newSpikeUOWStore(ctx, beadsDir)
		}
		// TODO: this needs to be uow provider
		return nil, fmt.Errorf("proxy server store should be uow provider")
		// 	return newProxiedServerStore(ctx, &dolt.Config{
		// 		BeadsDir:      beadsDir,
		// 		Database:      cfg.GetDoltDatabase(),
		// 		ProxiedServer: true,
		// 	})
	}
	if err == nil && cfg != nil && cfg.IsDoltServerMode() {
		return dolt.NewFromConfig(ctx, beadsDir)
	}
	return nil, fmt.Errorf("%s", nocgoEmbeddedErrMsg)
}

// newReadOnlyStoreFromConfig creates a read-only SQL-server-backed storage backend.
func newReadOnlyStoreFromConfig(ctx context.Context, beadsDir string) (storage.DoltStorage, error) {
	cfg, err := configfile.Load(beadsDir)
	if err == nil && cfg != nil && cfg.GetBackend() == configfile.BackendPostgres {
		return postgres.NewFromConfig(ctx, beadsDir)
	}
	if err == nil && cfg != nil && cfg.GetBackend() == configfile.BackendMySQL {
		return beadsmysql.NewFromConfig(ctx, beadsDir)
	}
	if err == nil && cfg != nil && cfg.GetBackend() == configfile.BackendSQLite {
		return beadssqlite.NewFromConfig(ctx, beadsDir)
	}
	if err == nil && cfg != nil && cfg.IsDoltProxiedServerMode() {
		if spikeUOWStore() {
			return newSpikeUOWStore(ctx, beadsDir)
		}
		// TODO: this needs to be uow provider
		return nil, fmt.Errorf("proxy server store needs to be uow provider")
		// return newProxiedServerStore(ctx, &dolt.Config{
		// 	BeadsDir:      beadsDir,
		// 	Database:      cfg.GetDoltDatabase(),
		// 	ProxiedServer: true,
		// 	ReadOnly:      true,
		// })
	}
	if err == nil && cfg != nil && cfg.IsDoltServerMode() {
		return dolt.NewFromConfigWithOptions(ctx, beadsDir, &dolt.Config{ReadOnly: true})
	}
	return nil, fmt.Errorf("%s", nocgoEmbeddedErrMsg)
}

const nocgoEmbeddedErrMsg = `embedded Dolt requires a CGO build, but this bd binary was built with CGO_ENABLED=0.

Three options:

  1. Use the proxied dolt sql-server (no external server, no reinstall):
       bd init --proxied-server
     bd spawns a per-workspace proxy + child dolt sql-server under
     .beads/proxieddb/ and manages their lifecycle for you.

  2. Use external server mode (no reinstall needed):
       bd init --server
     Requires a running 'dolt sql-server'. See docs/DOLT.md.

  3. Reinstall with embedded-mode support:
       brew install beads                              # macOS / Linux
       npm install -g @beads/bd                        # any platform with Node
       curl -fsSL https://raw.githubusercontent.com/steveyegge/beads/main/scripts/install.sh | bash

See docs/INSTALLING.md for the full comparison.`
