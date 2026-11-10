package migrate

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Roles names the database roles of a deployment with separate roles. An empty name is skipped.
type Roles struct {
	API      string // serve: reads the published data and which keys are suppressed, nothing else
	Sync     string // sync: writes the mirror and the search layer; reads the objections
	Operator string // suppress: changes the objections and the search rows they affect
}

// Any reports whether at least one role is named.
func (r Roles) Any() bool { return r.API != "" || r.Sync != "" || r.Operator != "" }

// The privilege matrix. It lives here, next to the schema, so that every migration that adds a
// table also decides who may touch it; a table missing from the matrix is reachable by the owner
// only. `migrate` applies it after the schema migrations, as the schema owner.
var (
	mirror = []string{"rpo_record", "fs_tax_subject", "fs_vat_payer", "fs_list_entry"}
	search = []string{"search_name", "search_token", "search_word"}
	stage  = []string{"rpo_record_stage", "fs_stage"}
	state  = []string{"sync_source", "sync_unit", "sync_run", "rpo_snapshot_seen", "rpo_seen"}

	allTables = join(mirror, search, stage, state, []string{"objection", "objection_record", "objection_active", "register_setup"})
)

func join(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

type grant struct {
	privileges string
	tables     []string
}

type roleGrants struct {
	role   string
	grants []grant
}

func matrix(r Roles, maintain bool) []roleGrants {
	write := "SELECT, INSERT, UPDATE, DELETE, TRUNCATE"
	if maintain {
		write += ", MAINTAIN" // PostgreSQL 17+: lets the sync ANALYZE the tables it rebuilds
	}
	return []roleGrants{
		{r.API, []grant{
			{"SELECT", join(mirror, search, []string{"sync_source", "objection_active", "register_setup"})},
		}},
		{r.Sync, []grant{
			{write, join(mirror, search, stage, state)},
			{"SELECT", []string{"objection", "objection_record", "register_setup"}},
			{"INSERT", []string{"objection_record"}}, // anchors of records created with a suppressed IČO
		}},
		{r.Operator, []grant{
			{"SELECT", join(mirror, []string{"sync_source"})},
			{"SELECT, INSERT, UPDATE, DELETE", search},
			{"SELECT, INSERT, UPDATE", []string{"objection", "objection_record", "register_setup"}},
		}},
	}
}

// Grant applies the privilege matrix for the named roles: it first revokes everything the roles
// hold on the register's tables and sequences, then grants exactly what each needs. It is
// idempotent and runs in one transaction. The connection must own the tables.
func Grant(ctx context.Context, dsn string, r Roles) error {
	if !r.Any() {
		return nil
	}
	seen := map[string]bool{}
	for _, name := range []string{r.API, r.Sync, r.Operator} {
		if name != "" && seen[name] {
			return fmt.Errorf("the roles must differ: %q is named twice", name)
		}
		seen[name] = true
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer conn.Close(ctx)
	var version int
	if err := conn.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&version); err != nil {
		return err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	exec := func(sql string) error {
		if _, err := tx.Exec(ctx, sql); err != nil {
			return fmt.Errorf("%s: %w", sql, err)
		}
		return nil
	}
	tables := func(names []string) string {
		s := ""
		for i, n := range names {
			if i > 0 {
				s += ", "
			}
			s += pgx.Identifier{"public", n}.Sanitize()
		}
		return s
	}
	for _, rg := range matrix(r, version >= 170000) {
		if rg.role == "" {
			continue
		}
		who := pgx.Identifier{rg.role}.Sanitize()
		for _, sql := range []string{
			"REVOKE ALL ON ALL TABLES IN SCHEMA public FROM " + who,
			"REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM " + who,
			"GRANT USAGE ON SCHEMA public TO " + who,
		} {
			if err := exec(sql); err != nil {
				return err
			}
		}
		for _, g := range rg.grants {
			if err := exec("GRANT " + g.privileges + " ON " + tables(g.tables) + " TO " + who); err != nil {
				return err
			}
		}
	}
	if r.Sync != "" {
		if err := exec("GRANT USAGE ON SEQUENCE public.sync_run_id_seq TO " + pgx.Identifier{r.Sync}.Sanitize()); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Tables lists every table and view the matrix knows; a test checks it against the schema.
func Tables() []string { return allTables }
