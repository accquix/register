package search_test

import (
	"context"
	"strings"
	"testing"

	"github.com/accquix/register/internal/normalize"
	"github.com/accquix/register/internal/search"
	"github.com/accquix/register/internal/testkit"
)

// D56: the key the API builds from a query in Go must be the key PostgreSQL builds from a stored
// name with register_name_key (migration 00007), or a taxpayer is never found. Both sort words by
// byte; letters that keep a non-ASCII form after folding are the case where a collation would differ.
func TestNameKeyMatchesSQL(t *testing.T) {
	ctx := context.Background()
	conn := testkit.Conn(t, testkit.Database(t))
	for _, name := range []string{
		"Jana Fiktívna", "FIKTÍVNA Jana", "Ján  Fiktívny - Kaderníctvo", "Łukasz Ølsen Ärger", "straße zeta Alfa",
		"ab Ab aB", "Zz aa Ä a", "12 Jana 3", "Ðorđe Þór Æsa", "", "jeden",
	} {
		core, _ := normalize.SplitName(name)
		want, _ := search.NameKey(core)
		var got string
		if err := conn.QueryRow(ctx, `SELECT register_name_key($1)`, core).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%q: SQL %q, Go %q", name, got, want)
		}
	}
	// the API's query can use the partial index on the key (an empty table: no sequential scan)
	var plan []string
	if _, err := conn.Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(ctx, `EXPLAIN SELECT dic FROM fs_tax_subject t
WHERE register_name_key(t.name_core) = 'fiktivna jana' AND t.ico IS NULL AND t.absent_since IS NULL`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	if p := strings.Join(plan, "\n"); !strings.Contains(p, "fs_tax_subject_name_key") {
		t.Fatalf("the taxpayer query must be able to use fs_tax_subject_name_key:\n%s", p)
	}
}

// D56: the namesake rule of the taxpayer search hides a full name, never scattered words.
func TestContainsFullName(t *testing.T) {
	cases := []struct {
		name, query string
		want        bool
	}{
		{"jan novak", "jan novak", true},
		{"jan novak", "novak jan", true},
		{"ing jan novak", "jan novak", true},
		{"jan novak stolarstvo", "jan novak", true},
		{"stolarstvo jan novak", "novak jan", true},
		{"jan novak stolarstvo peter", "jan peter", false}, // words of two names
		{"jan stolarstvo novak", "jan novak", false},       // not adjacent
		{"jana novakova", "jan novak", false},              // other words, not prefixes
		{"jan", "jan novak", false},
		{"jan jan novak", "jan novak", true},
		{"novak jan jan", "jan jan", true},
	}
	for _, c := range cases {
		key, _ := search.NameKey(c.query)
		if got := search.ContainsFullName(c.name, key); got != c.want {
			t.Errorf("%q holds %q: %v, want %v", c.name, c.query, got, c.want)
		}
	}
}
