package sqlite

import (
	"context"
	"database/sql"

	"github.com/steveyegge/beads/internal/storage/sqlitedialect"
)

// sqliteDialect is the SQLite SQL flavor: it opens a *sql.DB over modernc.org/sqlite
// whose driver translates bd's canonical MySQL SQL to SQLite (small: mainly
// INSERT IGNORE → INSERT OR IGNORE). The dsn already carries the file path + pragmas.
type sqliteDialect struct{ dsn string }

func (d sqliteDialect) Name() string { return "sqlite" }

func (d sqliteDialect) Open(_ context.Context) (*sql.DB, error) {
	return sqlitedialect.Open(d.dsn)
}
