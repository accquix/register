package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accquix/register/internal/api"
	"github.com/accquix/register/internal/ingest"
	"github.com/accquix/register/internal/migrate"
	"github.com/accquix/register/internal/sources/fs"
	"github.com/accquix/register/internal/sources/rpo"
	"github.com/accquix/register/internal/syncer"
	"github.com/accquix/register/internal/testkit"
)

// The whole service runs with the three separate roles of the privilege matrix: the sync as the
// sync role, objections as the operator, the API as the API role. Then each role is denied what it
// must not do. This is the test that fails when a new table or statement is missing from the
// matrix, or when a role can do more than its job.
func TestSeparateRoles(t *testing.T) {
	ctx := context.Background()
	apiRole, syncRole, opRole := testkit.Roles(t)
	dsn := testkit.Database(t)
	if err := migrate.Grant(ctx, dsn, migrate.Roles{API: apiRole, Sync: syncRole, Operator: opRole}); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Grant(ctx, dsn, migrate.Roles{API: apiRole, Sync: syncRole, Operator: opRole}); err != nil {
		t.Fatalf("grants must be idempotent: %v", err)
	}
	if err := migrate.Grant(ctx, dsn, migrate.Roles{API: apiRole, Sync: apiRole}); err == nil {
		t.Fatal("one role named twice must be refused")
	}
	// Role names that would make the matrix meaningless are refused, and nothing changes
	// (review findings D6 and D7).
	admin := testkit.Conn(t, dsn)
	var me string
	if err := admin.QueryRow(ctx, `SELECT current_user`).Scan(&me); err != nil {
		t.Fatal(err)
	}
	group := apiRole + "_grp"
	if _, err := admin.Exec(ctx, "CREATE ROLE "+group); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+group) })
	for why, roles := range map[string]migrate.Roles{
		"the role that runs migrate and owns the tables": {API: me},
		"a role that does not exist":                     {API: "reg_t_missing"},
	} {
		if err := migrate.Grant(ctx, dsn, roles); err == nil {
			t.Fatalf("%s must be refused", why)
		}
	}
	// A membership alone is fine (managed providers create roles with memberships). What counts
	// is what the role inherits: a group that can read objections makes the API role unfit, and
	// that is found by asking for the effective rights.
	if _, err := admin.Exec(ctx, "GRANT "+group+" TO "+apiRole); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Grant(ctx, dsn, migrate.Roles{API: apiRole, Sync: syncRole, Operator: opRole}); err != nil {
		t.Fatalf("a harmless membership must be accepted, got %v", err)
	}
	if _, err := admin.Exec(ctx, "GRANT SELECT ON objection TO "+group); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Grant(ctx, dsn, migrate.Roles{API: apiRole, Sync: syncRole, Operator: opRole}); err == nil || !strings.Contains(err.Error(), "member of") {
		t.Fatalf("a role that inherits a forbidden right must be refused, got %v", err)
	}
	if _, err := admin.Exec(ctx, "REVOKE ALL ON objection FROM "+group+"; REVOKE "+group+" FROM "+apiRole); err != nil {
		t.Fatal(err)
	}
	// Tables of another application in the same schema are not touched.
	if _, err := admin.Exec(ctx, `CREATE TABLE other_app (x int); GRANT SELECT ON other_app TO PUBLIC`); err != nil {
		t.Fatal(err)
	}
	// A right given to everyone would reach the API role whatever is revoked from it by name:
	// migrate takes it away.
	if _, err := admin.Exec(ctx, `GRANT SELECT, UPDATE ON objection TO PUBLIC`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Grant(ctx, dsn, migrate.Roles{API: apiRole, Sync: syncRole, Operator: opRole}); err != nil {
		t.Fatalf("grants after a PUBLIC grant: %v", err)
	}
	var leaked, otherKept bool
	if err := admin.QueryRow(ctx, `SELECT has_table_privilege($1, 'objection', 'SELECT, UPDATE'), has_table_privilege($1, 'other_app', 'SELECT')`, apiRole).Scan(&leaked, &otherKept); err != nil || leaked {
		t.Fatalf("the API role still reaches objection through PUBLIC: %v %v", leaked, err)
	}
	if !otherKept {
		t.Fatal("migrate revoked a right on another application's table")
	}
	if _, err := admin.Exec(ctx, `DROP TABLE other_app`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Grant(ctx, dsn, migrate.Roles{API: me}); err == nil {
		t.Fatal("the owner named as a role must be refused again")
	}
	if _, err := admin.Exec(ctx, `SELECT 1 FROM goose_db_version LIMIT 1`); err != nil {
		t.Fatalf("a refused grant took the owner's own rights: %v", err)
	}
	owner := testkit.Conn(t, dsn)
	// every table and view of the schema is in the matrix (or is goose's own table)
	var missing []string
	rows, err := owner.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public' AND c.relkind IN ('r', 'v', 'p') AND c.relname <> 'goose_db_version' AND c.relname <> ALL($1)`, migrate.Tables())
	if err != nil {
		t.Fatal(err)
	}
	if missing, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || len(missing) > 0 {
		t.Fatalf("tables missing from the privilege matrix: %v %v", missing, err)
	}

	// --- the operator confirms the installation and records an objection before the first sync
	operator := testkit.As(t, dsn, opRole)
	if err := ingest.ConfirmNoObjections(ctx, operator); err != nil {
		t.Fatalf("operator: init-empty: %v", err)
	}
	if _, _, err := ingest.AddObjection(ctx, operator, "99900002", "ticket"); err != nil {
		t.Fatalf("operator: objection: %v", err)
	}

	// --- the sync role: bootstrap, daily, reconcile with absence marking, the FS lists
	syncConn := testkit.As(t, dsn, syncRole)
	day := func(d int) string { return time.Now().UTC().AddDate(0, 0, d).Format("2006-01-02") }
	zlata := testkit.Company(1, "Zlatá Rybka, s.r.o.")
	striebro := testkit.Company(2, "Strieborná Rybka, a.s.")
	gone := testkit.Company(3, "Dočasná Rybka, s.r.o.")
	b := testkit.NewBucket(t)
	bk := rpo.NewBucket(b.URL())
	rcfg := syncer.RPOConfig{Bucket: bk, WorkDir: t.TempDir(), AbsenceMaxRatio: 0.9, SnapshotMinRatio: 0.1}
	b.PutSnapshot(day(-5), []testkit.Rec{zlata, striebro, gone}, 100)
	renamed := zlata
	renamed.Name = "Zlatá Rybka Plus, s.r.o."
	b.PutDaily(day(-4), []testkit.Rec{renamed})
	if err := syncer.NewRPO(syncConn, rcfg).Run(ctx); err != nil {
		t.Fatalf("sync role: bootstrap and daily: %v", err)
	}
	b.PutSnapshot(day(-1), []testkit.Rec{renamed, striebro}, 100) // "gone" is absent: reconcile marks it
	if err := syncer.NewRPO(syncConn, rcfg).Run(ctx); err != nil {
		t.Fatalf("sync role: reconcile: %v", err)
	}
	srv := testkit.NewFSServer(t)
	for _, l := range fs.Lists {
		var items []testkit.Item
		switch l.Slug {
		case "ds_dphs":
			items = []testkit.Item{testkit.VATItem(1, "99900001", "§4", "01.08.2020", "01.08.2020")}
		case "ds_dsrdp":
			items = []testkit.Item{testkit.TaxItem(1, "99900001")}
		}
		srv.Put(l.Slug, testkit.FSZip(l.Slug, day(0), items))
	}
	if err := syncer.NewFS(syncConn, syncer.FSConfig{Client: fs.NewClient(srv.URL()), WorkDir: t.TempDir(), SkipMinRows: true}).Run(ctx); err != nil {
		t.Fatalf("sync role: FS lists: %v", err)
	}

	// --- the operator again, on a published database: revoke and record
	ids, err := ingest.ActiveObjections(ctx, operator, "99900002")
	if err != nil || len(ids) != 1 {
		t.Fatalf("operator: list: %v %v", ids, err)
	}
	if _, err := ingest.RevokeObjection(ctx, operator, ids[0]); err != nil {
		t.Fatalf("operator: revoke: %v", err)
	}
	if _, _, err := ingest.AddObjection(ctx, operator, "99900002", "ticket 2"); err != nil {
		t.Fatalf("operator: objection on published data: %v", err)
	}
	if _, err := ingest.ListObjections(ctx, operator); err != nil {
		t.Fatalf("operator: export: %v", err)
	}
	// release one link and withdraw the release; restore a backup
	ids, _ = ingest.ActiveObjections(ctx, operator, "99900002")
	if _, err := ingest.ReleaseLink(ctx, operator, ids[0], striebro.ID, "ticket 3", true); err != nil {
		t.Fatalf("operator: release: %v", err)
	}
	if _, err := ingest.ReleaseLink(ctx, operator, ids[0], striebro.ID, "", false); err != nil {
		t.Fatalf("operator: unrelease: %v", err)
	}
	at := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	if _, err := ingest.RestoreObjections(ctx, operator, []ingest.Objection{{Key: "r900000003", CreatedAt: at,
		Links: []ingest.Link{{RecordID: striebro.ID, Basis: ingest.BasisRestored, CreatedAt: at, ReleasedAt: &at}}}}); err != nil {
		t.Fatalf("operator: restore: %v", err)
	}
	// a daily file after the objections: the sync links a new carrier of the objected IČO
	twin := testkit.Company(12, "Strieborná Rybka Dva, a.s.")
	twin.ICO = striebro.ICO
	b.PutDaily(day(0), []testkit.Rec{twin})
	if err := syncer.NewRPO(syncConn, rcfg).Run(ctx); err != nil {
		t.Fatalf("sync role: daily with a link: %v", err)
	}
	var linked int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM objection_record WHERE rpo_id = $1 AND basis = 'carried_ico'`, twin.ID).Scan(&linked); err != nil || linked != 1 {
		t.Fatalf("sync role: the new carrier was not linked: %d %v", linked, err)
	}

	// --- the API role serves search (default and fuzzy) and lookup
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pc.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE "+apiRole)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	h, _ := api.New(pool, api.Config{Version: "test", Keys: api.Keys{api.HashKey(key): "tester"}, Fuzzy: true,
		RatePerSecond: 1000, RateBurst: 1000, FuzzyEvery: time.Millisecond, FuzzyConcurrent: 1, SearchTaxpayers: true,
		FuzzyTimeout: 2 * time.Second, QueryTimeout: 3 * time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	web := httptest.NewServer(h)
	t.Cleanup(web.Close)
	e := env{srv: web, dsn: dsn}
	for _, p := range []string{"/v1/search?q=rybka", "/v1/search?mode=fuzzy&q=zlta", "/v1/lookup/99900001", "/v1/sources", "/readyz",
		"/v1/search?q=fiktivna+osoba"} {
		code, _, m := e.get(t, p, true)
		if code != http.StatusOK {
			t.Fatalf("API role: %s: %d %v", p, code, m)
		}
		if strings.Contains(p, "osoba") && m["taxpayers"] != "searched" { // D56: the function and objection_active.dic
			t.Fatalf("API role: taxpayer search: %v", m)
		}
		if p == "/v1/search?q=rybka" && (len(names(m)) != 1 || !strings.Contains(names(m)[0], "Zlatá Rybka Plus")) {
			t.Fatalf("API role: search must return the renamed subject and not the suppressed or absent ones: %v", names(m))
		}
	}

	// --- and what each role must not be able to do
	denied := func(role, sql string) {
		t.Helper()
		c := testkit.As(t, dsn, role)
		if _, err := c.Exec(ctx, sql); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s must be denied %q, got %v", role, sql, err)
		}
	}
	for _, sql := range []string{
		`SELECT reason FROM objection`, `SELECT * FROM objection_record`, `SELECT * FROM sync_unit`, `SELECT * FROM rpo_record_stage`,
		`INSERT INTO objection (ico) VALUES ('99900009')`, `UPDATE rpo_record SET name = 'x' WHERE id = 1`,
		`DELETE FROM search_name WHERE skey = 'x'`, `TRUNCATE search_word`, `UPDATE register_setup SET objections_confirmed_at = now() WHERE only_row`,
		`CREATE TABLE public.t (x int)`,
	} {
		denied(apiRole, sql)
	}
	for _, sql := range []string{
		`INSERT INTO objection (ico) VALUES ('99900009')`, `UPDATE objection SET revoked_at = now() WHERE id = 1`,
		`DELETE FROM objection WHERE id = 1`, `TRUNCATE objection`, `DELETE FROM objection_record WHERE rpo_id = 1`,
		`SELECT reason FROM objection`, `SELECT released_reason FROM objection_record`,
		`UPDATE objection_record SET released_at = now() WHERE rpo_id = 1`,
		`UPDATE register_setup SET objections_confirmed_at = now() WHERE only_row`, `CREATE TABLE public.t (x int)`,
	} {
		denied(syncRole, sql)
	}
	for _, sql := range []string{
		`UPDATE rpo_record SET name = 'x' WHERE id = 1`, `DELETE FROM objection WHERE id = 1`, `TRUNCATE objection`,
		`UPDATE objection SET created_at = now() WHERE id = 1`, `UPDATE objection SET reason = 'x' WHERE id = 1`, `UPDATE objection SET ico = '99900001' WHERE id = 1`, `UPDATE objection SET dic = '2099900001' WHERE id = 1`,
		`DELETE FROM objection_record WHERE rpo_id = 1`, `UPDATE objection_record SET basis = 'at_objection' WHERE rpo_id = 1`,
		`UPDATE sync_source SET state = 'empty' WHERE source = 'rpo'`, `INSERT INTO fs_vat_payer (vkey, ic_dph, paragraph, source_as_of, row_hash) VALUES ('x', 'SK2099999999', '§4', now(), '\x00')`,
	} {
		denied(opRole, sql)
	}
}
