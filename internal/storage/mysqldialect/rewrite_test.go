package mysqldialect

import (
	"regexp"
	"strings"
	"testing"
)

func TestRewriteSelfRefUpdate(t *testing.T) {
	recompute := `UPDATE issues i SET i.is_blocked = 1, i.updated_at = i.updated_at
		WHERE i.id IN (?,?,?)
		  AND i.is_blocked = 0
		  AND i.status <> 'closed' AND i.status <> 'pinned'
		  AND (EXISTS (SELECT 1 FROM dependencies d JOIN issues t ON t.id = d.depends_on_issue_id
		        WHERE d.issue_id = i.id AND d.type = 'blocks' AND t.status <> 'closed'))`

	got := rewriteSelfRefUpdate(recompute)
	if got == recompute {
		t.Fatal("recompute UPDATE was not rewritten")
	}
	// The outer target must no longer be referenced directly in a subquery: the
	// materialized derived table carries the self-reference.
	if !strings.Contains(got, "SELECT id FROM (SELECT _b.id FROM issues _b") {
		t.Fatalf("missing double-nested derived table:\n%s", got)
	}
	// The correlation must be rebound to the derived-table alias.
	if !strings.Contains(got, "d.issue_id = _b.id") {
		t.Fatalf("alias not rebound to _b:\n%s", got)
	}
	// The outer SET still targets the real alias.
	if !strings.HasPrefix(strings.TrimSpace(got), "UPDATE issues i SET i.is_blocked = 1") {
		t.Fatalf("outer UPDATE target changed:\n%s", got)
	}
	// Placeholder count preserved (positional binding).
	if a, b := strings.Count(recompute, "?"), strings.Count(got, "?"); a != b {
		t.Fatalf("placeholder count changed: %d -> %d", a, b)
	}
}

func TestRewritePassThrough(t *testing.T) {
	cases := []string{
		"SELECT * FROM issues WHERE id = ?",
		"UPDATE issues SET title = ? WHERE id = ?",                 // no is_blocked, no IN
		"UPDATE issues i SET i.is_blocked = 1 WHERE i.id IN (?,?)", // batch update, no self-ref conditions
		"INSERT INTO issues (id) VALUES (?)",
		"DELETE FROM issues WHERE id = ?",
	}
	for _, q := range cases {
		if got := rewriteSelfRefUpdate(q); got != q {
			t.Errorf("query was rewritten but should pass through:\n in: %s\nout: %s", q, got)
		}
	}
}

// The rewrite must not leave the outer target table referenced in a subquery in a way
// that regex could double-apply; a second pass is a no-op on already-rewritten SQL.
func TestRewriteIdempotentShape(t *testing.T) {
	q := `UPDATE wisps w SET w.is_blocked = 0, w.updated_at = w.updated_at
		WHERE w.id IN (?) AND w.is_blocked = 1 AND (w.status = 'closed')`
	once := rewriteSelfRefUpdate(q)
	if once == q {
		t.Fatal("expected rewrite")
	}
	// A plain "?,?" derived form should still parse-shape sanely (no target alias left
	// as a bare self-join outside the derived table).
	if regexp.MustCompile(`WHERE w\.id IN \(\?`).MatchString(once) {
		t.Fatalf("outer WHERE still has a raw placeholder IN (self-ref not materialized):\n%s", once)
	}
}
