package sqlitedialect

import (
	"strings"
	"testing"
)

func TestTranslate(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string // substring that must appear
		notWant string // substring that must NOT appear
	}{
		{"insert ignore", "INSERT IGNORE INTO labels (a) VALUES (?)", "INSERT OR IGNORE INTO", "INSERT IGNORE INTO"},
		{"concat", "SELECT CONCAT(a, b)", "(a || b)", "CONCAT"},
		{"now", "SELECT NOW()", "CURRENT_TIMESTAMP", "NOW()"},
		{"utc_timestamp", "SELECT UTC_TIMESTAMP()", "CURRENT_TIMESTAMP", "UTC_TIMESTAMP"},
		{"json_arrayagg", "SELECT JSON_ARRAYAGG(x)", "json_group_array(x)", "JSON_ARRAYAGG"},
		{"json_unquote unwrap", "SELECT JSON_UNQUOTE(JSON_EXTRACT(m, '$.k'))", "JSON_EXTRACT(m, '$.k')", "JSON_UNQUOTE"},
		{"date_format", "SELECT DATE_FORMAT(created_at, '%Y-%m-%dT%H:%i:%sZ')", "strftime('%Y-%m-%dT%H:%M:%SZ', created_at)", "DATE_FORMAT"},
		{"update alias", "UPDATE issues i SET i.is_blocked = 1 WHERE i.id IN (?)", "UPDATE issues AS i SET", ""},
		{"on duplicate key", "INSERT INTO issues (id) VALUES (?) ON DUPLICATE KEY UPDATE title = VALUES(title)", "ON CONFLICT (id) DO UPDATE SET title = excluded.title", "DUPLICATE"},
		{"passthrough select", "SELECT * FROM issues WHERE id = ?", "SELECT * FROM issues WHERE id = ?", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Translate(tc.in)
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("Translate(%q) = %q; missing %q", tc.in, got, tc.want)
			}
			if tc.notWant != "" && strings.Contains(got, tc.notWant) {
				t.Errorf("Translate(%q) = %q; must not contain %q", tc.in, got, tc.notWant)
			}
		})
	}
}
