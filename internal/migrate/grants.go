package migrate

import (
	"context"
	"fmt"
	"strings"

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
			{"SELECT", []string{"goose_db_version"}}, // serve checks at start that the schema is current
		}},
		{r.Sync, []grant{
			{write, join(mirror, search, stage, state)},
			// The sync needs which keys are objected, not why: no reason column, no notes.
			{"SELECT (id, ico, rpo_id, dic, created_at, revoked_at)", []string{"objection"}},
			{"SELECT (objection_id, rpo_id, basis, source, created_at, released_at)", []string{"objection_record"}},
			{"SELECT, UPDATE (search_rebuild_pending)", []string{"register_setup"}}, // the rebuild after an upgrade
			{"INSERT", []string{"objection_record"}},                                // links of records that carry an objected IČO
		}},
		{r.Operator, []grant{
			{"SELECT", join(mirror, []string{"sync_source"})},
			{"SELECT, INSERT, UPDATE, DELETE", search},
			// An objection is never rewritten: the operator adds one, revokes one, releases a link.
			{"SELECT, INSERT, UPDATE (revoked_at)", []string{"objection"}},
			{"SELECT, INSERT, UPDATE (released_at, released_reason)", []string{"objection_record"}},
			{"SELECT, UPDATE (objections_confirmed_at, objections_confirmed_as)", []string{"register_setup"}},
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
	if err := checkRoles(ctx, conn, r); err != nil {
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
	// The register's own tables and their sequences, nothing else: a shared database may hold
	// other applications' tables in the same schema, and their rights are not ours to change.
	own := tables(append(append([]string{}, allTables...), "goose_db_version"))
	seqRows, err := tx.Query(ctx, `SELECT DISTINCT s.relname FROM pg_class s
JOIN pg_depend d ON d.objid = s.oid AND d.classid = 'pg_class'::regclass
JOIN pg_class t ON t.oid = d.refobjid JOIN pg_namespace n ON n.oid = t.relnamespace
WHERE s.relkind = 'S' AND n.nspname = 'public' AND t.relname = ANY($1) ORDER BY 1`, append(append([]string{}, allTables...), "goose_db_version"))
	if err != nil {
		return err
	}
	seqNames, err := pgx.CollectRows(seqRows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	revoke := func(who string) error {
		if err := exec("REVOKE ALL ON " + own + " FROM " + who); err != nil {
			return err
		}
		if len(seqNames) == 0 {
			return nil
		}
		return exec("REVOKE ALL ON SEQUENCE " + tables(seqNames) + " FROM " + who)
	}
	// Rights given to everyone would reach every role, whatever is revoked from it by name.
	if err := revoke("PUBLIC"); err != nil {
		return err
	}
	for _, rg := range matrix(r, version >= 170000) {
		if rg.role == "" {
			continue
		}
		who := pgx.Identifier{rg.role}.Sanitize()
		if err := revoke(who); err != nil {
			return err
		}
		if err := exec("GRANT USAGE ON SCHEMA public TO " + who); err != nil {
			return err
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
	// The taxpayer name search calls register_name_key (D56). Name the right instead of relying on
	// PostgreSQL's default EXECUTE for PUBLIC, which a hardened database may have removed.
	if r.API != "" {
		if err := exec("GRANT EXECUTE ON FUNCTION public.register_name_key(text) TO " + pgx.Identifier{r.API}.Sanitize()); err != nil {
			return err
		}
	}
	if err := verify(ctx, tx, r); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// checkRoles refuses role names that would make the matrix meaningless or harmful: a role that
// does not exist, the connecting role or the owner of the tables (revoking from it would lock
// migrate out of its own schema), or a superuser. A membership in another role is not refused
// here (managed providers create roles with memberships): what a role inherits is judged by
// verify, which asks for the effective rights, inherited ones included.
func checkRoles(ctx context.Context, conn *pgx.Conn, r Roles) error {
	for _, name := range []string{r.API, r.Sync, r.Operator} {
		if name == "" {
			continue
		}
		var exists, super, self, owner bool
		err := conn.QueryRow(ctx, `SELECT
  EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1),
  coalesce((SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = $1), false),
  $1 = current_user,
  EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace JOIN pg_roles o ON o.oid = c.relowner
          WHERE n.nspname = 'public' AND o.rolname = $1 AND c.relname = ANY($2))`, name, append(append([]string{}, allTables...), "goose_db_version")).
			Scan(&exists, &super, &self, &owner)
		switch {
		case err != nil:
			return err
		case !exists:
			return fmt.Errorf("role %q does not exist", name)
		case self || owner:
			return fmt.Errorf("role %q runs migrate or owns the tables: name a separate role for the API, the sync and the operator", name)
		case super:
			return fmt.Errorf("role %q is a superuser (or bypasses row security): table rights do not bind it", name)
		}
	}
	return nil
}

// verify checks the effective rights after the grants, in the same transaction: what a role must
// not be able to do is asked of PostgreSQL, not assumed from the statements above. A failure
// rolls everything back.
func verify(ctx context.Context, tx pgx.Tx, r Roles) error {
	type check struct{ role, table, privileges, why string }
	var checks []check
	deny := func(role, why, privileges string, tables ...string) {
		for _, t := range tables {
			if role != "" {
				checks = append(checks, check{role, t, privileges, why})
			}
		}
	}
	all := "SELECT, INSERT, UPDATE, DELETE, TRUNCATE"
	write := "INSERT, UPDATE, DELETE, TRUNCATE"
	deny(r.API, "the API must not see objections", all, "objection", "objection_record")
	deny(r.API, "the API must not see the sync journal or staging", all, join(stage, []string{"sync_unit", "sync_run", "rpo_snapshot_seen", "rpo_seen"})...)
	deny(r.API, "the API never writes", write, join(mirror, search, []string{"sync_source", "register_setup"})...)
	deny(r.Sync, "the sync must not change objections", "INSERT, UPDATE, DELETE, TRUNCATE", "objection")
	deny(r.Sync, "the sync may only add links", "UPDATE, DELETE, TRUNCATE", "objection_record")
	deny(r.Operator, "the operator must not write register data", write, join(mirror, stage, state)...)
	deny(r.Operator, "an objection is never deleted", "DELETE, TRUNCATE", "objection", "objection_record")
	if r.API != "" {
		var can bool
		if err := tx.QueryRow(ctx, `SELECT has_function_privilege($1, 'public.register_name_key(text)', 'EXECUTE')`, r.API).Scan(&can); err != nil {
			return err
		}
		if !can {
			return fmt.Errorf("role %q cannot execute register_name_key: the taxpayer name search would fail", r.API)
		}
	}
	// The sync reads which keys are objected, never why.
	if r.Sync != "" {
		for table, column := range map[string]string{"objection": "reason", "objection_record": "released_reason"} {
			var held bool
			if err := tx.QueryRow(ctx, `SELECT has_column_privilege($1, $2, $3, 'SELECT')`, r.Sync, "public."+table, column).Scan(&held); err != nil {
				return err
			}
			if held {
				return fmt.Errorf("role %q can read %s.%s: the sync must not see the reasons of objections", r.Sync, table, column)
			}
		}
	}
	for _, c := range checks {
		held := false
		if err := tx.QueryRow(ctx, `SELECT has_table_privilege($1, $2, $3)`, c.role, "public."+c.table, c.privileges).Scan(&held); err != nil {
			return err
		}
		// a right on single columns counts too (INSERT, UPDATE and SELECT exist per column)
		if cols := columnPrivileges(c.privileges); !held && cols != "" {
			if err := tx.QueryRow(ctx, `SELECT has_any_column_privilege($1, $2, $3)`, c.role, "public."+c.table, cols).Scan(&held); err != nil {
				return err
			}
		}
		if held {
			return fmt.Errorf("role %q still holds one of %s on %s (%s). It has the right through another grant or through a role it is a member of (a role created in a provider's console often is): create the role with SQL, without memberships, or revoke the other grant, then run migrate again", c.role, c.privileges, c.table, c.why)
		}
	}
	return nil
}

// columnPrivileges keeps the privileges that exist per column; "" when none does.
func columnPrivileges(list string) string {
	out := ""
	for _, p := range []string{"SELECT", "INSERT", "UPDATE"} {
		if strings.Contains(list, p) {
			if out != "" {
				out += ", "
			}
			out += p
		}
	}
	return out
}

// Tables lists every table and view the matrix knows; a test checks it against the schema.
func Tables() []string { return allTables }
