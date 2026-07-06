package sqlite

import "strings"

// dsn builds a modernc.org/sqlite DSN for a database file path, enabling foreign-key
// enforcement (off by default in SQLite) and an immediate-lock transaction mode so
// concurrent writers fail fast rather than deadlock.
func dsn(path string) string {
	if strings.HasPrefix(path, "file:") {
		return path
	}
	return "file:" + path + "?_pragma=foreign_keys(1)&_txlock=immediate"
}
