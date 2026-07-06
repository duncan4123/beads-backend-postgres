package pgdialect

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

// kvPasswordRe matches a libpq keyword/value password token (password= or
// sslpassword=), whose value is either single-quoted or a run of non-space chars.
var kvPasswordRe = regexp.MustCompile(`(?i)(^|\s)(?:password|sslpassword)\s*=\s*(?:'(?:[^'\\]|\\.)*'|\S*)`)

// RedactPassword returns dsn with the password removed, or an error if a password
// cannot be safely removed. It strips every known password location — URL userinfo,
// URL query params (password, sslpassword), and libpq keyword/value tokens — then
// VERIFIES that no EMBEDDED password survives, failing closed rather than persisting a
// secret in a DSN shape we did not anticipate. The check is embedded-only (HasPassword,
// not pgx.ParseConfig.Password) so an ambient PGPASSWORD/~/.pgpass — which pgx would
// fold in but which is never written to disk — does not falsely block init.
//
// Callers persist the returned string (e.g. to metadata.json) and re-supply the
// password at open time via the credential ladder (see the backend's resolveDSNCredential),
// so nothing operational is lost by never writing the password to disk. The hard
// invariant — a password must never be persisted — is enforced here, not assumed.
func RedactPassword(dsn string) (string, error) {
	stripped := stripPasswordBestEffort(dsn)
	// It must still parse, or we cannot reason about it — refuse a shape we mangled.
	if _, err := pgx.ParseConfig(stripped); err != nil {
		return "", fmt.Errorf("cannot verify the connection string is free of a password (unparseable after redaction): %w", err)
	}
	if HasPassword(stripped) {
		return "", fmt.Errorf("refusing to persist a connection string that still carries a password; supply it via BEADS_PG_PASSWORD instead of embedding it in --pg-url")
	}
	return stripped, nil
}

// stripPasswordBestEffort removes passwords from the two connection-string shapes
// pgx accepts. The authoritative guarantee is RedactPassword's post-parse check; this
// only has to handle the common forms well enough not to trip that check falsely.
func stripPasswordBestEffort(dsn string) string {
	// URL form (scheme://...): strip userinfo password and query password/sslpassword.
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" {
		if u.User != nil {
			if name := u.User.Username(); name == "" {
				u.User = nil
			} else {
				u.User = url.User(name)
			}
		}
		q := u.Query()
		q.Del("password")
		q.Del("sslpassword")
		u.RawQuery = q.Encode()
		return u.String()
	}
	// libpq keyword/value form (host=... password=... ...): drop the password tokens
	// and collapse surrounding whitespace so the remainder still parses.
	out := kvPasswordRe.ReplaceAllString(dsn, "$1")
	return strings.TrimSpace(strings.Join(strings.Fields(out), " "))
}
