package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accquix/register/internal/api"
	"github.com/accquix/register/internal/ingest"
	"github.com/accquix/register/internal/sources/fs"
	"github.com/accquix/register/internal/sources/rpo"
	"github.com/accquix/register/internal/syncer"
	"github.com/accquix/register/internal/testkit"
)

const key = "test-key-1"

type env struct {
	srv  *httptest.Server
	dsn  string
	logs *safeBuf
}

type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *safeBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// populate syncs a small synthetic mirror with dates around today, so that it is fresh.
func populate(t *testing.T, dsn string) {
	ctx := context.Background()
	conn := testkit.Conn(t, dsn)
	if err := ingest.ConfirmNoObjections(ctx, conn); err != nil { // a new installation
		t.Fatal(err)
	}
	day := func(d int) string { return time.Now().UTC().AddDate(0, 0, d).Format("2006-01-02") }
	sk := func(d int) string { return time.Now().UTC().AddDate(0, 0, d).Format("02.01.2006") }

	zlata := testkit.Company(1, "Zlatá Rybka, s.r.o.")
	striebro := testkit.Company(2, "Strieborná Rybka, a.s.")
	striebro.LegalCode = "121"
	zanik := testkit.Company(3, "Zaniknutá Rybka, s.r.o.")
	zanik.Terminated = day(-30)
	trader := testkit.Trader(4, "99900004", "Ján Fiktívny - Kaderníctvo Lucia")
	agent := testkit.Trader(5, "99900004", "Ján Fiktívny")
	agent.RegisterCode, agent.RegisterName = "9", "Register finančných agentov"

	b := testkit.NewBucket(t)
	b.PutSnapshot(day(-1), []testkit.Rec{zlata, striebro, zanik, trader, agent}, 100)
	bk := rpo.NewBucket(b.URL())
	if err := syncer.NewRPO(conn, syncer.RPOConfig{Bucket: bk, WorkDir: t.TempDir()}).Run(ctx); err != nil {
		t.Fatal(err)
	}

	group := func(n int, ico string) testkit.Item {
		it := testkit.VATItem(n, ico, "§4b", "01.01.2024", "01.01.2024")
		it["IC_DPH"] = "SK2099900100"
		return it
	}
	srv := testkit.NewFSServer(t)
	srv.Put("ds_dphs", testkit.FSZip("ds_dphs", day(0), []testkit.Item{
		testkit.VATItem(1, "99900001", "§4", "01.08.2020", "01.08.2020"),
		testkit.VATItem(4, "99900004", "§7a", "06.09.2023", ""),
		group(1001, "99900002"), group(1002, "99900003"),
	}))
	tax1 := testkit.TaxItem(1, "99900001")
	person := testkit.TaxItem(50, "") // a natural person without an IČO
	srv.Put("ds_dsrdp", testkit.FSZip("ds_dsrdp", day(0), []testkit.Item{tax1, person}))
	srv.Put("ds_iz_ran", testkit.FSZip("ds_iz_ran", day(0), []testkit.Item{{"DIC": "2099900001", "ICO": "99900001", "IDS": "spoľahlivý"}}))
	srv.Put("ds_dphz", testkit.FSZip("ds_dphz", day(0), nil))
	srv.Put("ds_dphv", testkit.FSZip("ds_dphv", day(0), nil))
	srv.Put("ds_dpho", testkit.FSZip("ds_dpho", day(0), []testkit.Item{{"IC_DPH": "SK2099900004", "ICO": "99900004", "DAT_ZACATIA": sk(-100)}}))
	if err := syncer.NewFS(conn, syncer.FSConfig{Client: fs.NewClient(srv.URL()), WorkDir: t.TempDir(), SkipMinRows: true}).Run(ctx); err != nil {
		t.Fatal(err)
	}
}

func start(t *testing.T, dsn string, mutate func(*api.Config)) env {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	cfg := api.Config{Version: "test", Keys: api.Keys{api.HashKey(key): "tester"}, Fuzzy: true,
		RatePerSecond: 1000, RateBurst: 1000, FuzzyEvery: time.Minute, FuzzyConcurrent: 1,
		FuzzyTimeout: 2 * time.Second, QueryTimeout: 3 * time.Second}
	if mutate != nil {
		mutate(&cfg)
	}
	logs := &safeBuf{}
	h, _ := api.New(pool, cfg, slog.New(slog.NewJSONHandler(logs, nil)))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return env{srv: srv, dsn: dsn, logs: logs}
}

func (e env) get(t *testing.T, path string, withKey bool) (int, http.Header, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
	if withKey {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return do(t, req)
}

func (e env) post(t *testing.T, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	code, _, m := do(t, req)
	return code, m
}

func do(t *testing.T, req *http.Request) (int, http.Header, map[string]any) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return resp.StatusCode, resp.Header, m
}

func names(m map[string]any) []string {
	var out []string
	rs, _ := m["results"].([]any)
	for _, r := range rs {
		out = append(out, r.(map[string]any)["name"].(string))
	}
	return out
}

func path(m any, keys ...string) any {
	for _, k := range keys {
		mm, ok := m.(map[string]any)
		if !ok {
			return nil
		}
		m = mm[k]
	}
	return m
}

func q(s string) string { return url.QueryEscape(s) }

func TestAPI(t *testing.T) {
	dsn := testkit.Database(t)
	populate(t, dsn)
	e := start(t, dsn, nil)

	t.Run("auth", func(t *testing.T) {
		if code, _, m := e.get(t, "/v1/search?q=rybka", false); code != 401 || m["code"] != "unauthorized" {
			t.Fatalf("no key: %d %v", code, m)
		}
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/v1/search?q=rybka", nil)
		req.Header.Set("Authorization", "Bearer wrong")
		if code, h, _ := do(t, req); code != 401 || !strings.Contains(h.Get("Content-Type"), "problem+json") {
			t.Fatalf("wrong key: %d %v", code, h)
		}
		for _, p := range []string{"/livez", "/readyz", "/openapi.json"} {
			if code, _, _ := e.get(t, p, false); code != 200 {
				t.Errorf("%s without a key: %d", p, code)
			}
		}
	})

	t.Run("search default", func(t *testing.T) {
		code, _, m := e.get(t, "/v1/search?q="+q("zlata ryb"), true)
		if code != 200 || len(names(m)) == 0 || names(m)[0] != "Zlatá Rybka, s.r.o." {
			t.Fatalf("prefix: %d %v", code, m)
		}
		if m["attribution"] == nil || m["dataAsOf"] == "" {
			t.Fatal("attribution and dataAsOf are required")
		}
		_, _, m = e.get(t, "/v1/search?q=rybka", true)
		got := strings.Join(names(m), "|")
		if !strings.Contains(got, "Zlatá Rybka") || !strings.Contains(got, "Strieborná Rybka") || strings.Contains(got, "Zaniknutá") {
			t.Fatalf("word search must find both active subjects and no terminated one: %v", got)
		}
		_, _, m = e.get(t, "/v1/search?q="+q("lucia kadernictvo"), true) // any word order
		if ns := names(m); len(ns) != 1 || ns[0] != "Ján Fiktívny - Kaderníctvo Lucia" {
			t.Fatalf("word order: %v", ns)
		}
		_, _, m = e.get(t, "/v1/search?q="+q("Ján Fiktívny"), true) // the trader's other official name
		r0, _ := m["results"].([]any)
		if len(r0) != 1 || path(r0[0], "ico") != "99900004" {
			t.Fatalf("alternative name: %v", m)
		}
	})

	t.Run("search identifiers", func(t *testing.T) {
		for qq, want := range map[string]string{"99900001": "Zlatá Rybka, s.r.o.", "2099900001": "Zlatá Rybka, s.r.o."} {
			if _, _, m := e.get(t, "/v1/search?q="+qq, true); strings.Join(names(m), "|") != want {
				t.Errorf("%s: %v", qq, names(m))
			}
		}
		_, _, m := e.get(t, "/v1/search?q=SK2099900100", true) // VAT group
		if got := strings.Join(names(m), "|"); got != "Strieborná Rybka, a.s.|Zaniknutá Rybka, s.r.o." {
			t.Fatalf("VAT group: %v", got)
		}
		if r := m["results"].([]any); path(r[1], "active") != false {
			t.Fatal("a terminated subject found by identifier must be flagged inactive")
		}
		if _, _, m := e.get(t, "/v1/search?q=2099900050", true); len(names(m)) != 0 {
			t.Fatal("a natural person without an IČO must never appear in search")
		}
	})

	t.Run("fuzzy", func(t *testing.T) {
		code, _, m := e.get(t, "/v1/search?mode=fuzzy&q="+q("zlta rybka"), true)
		if code != 200 || len(names(m)) == 0 || names(m)[0] != "Zlatá Rybka, s.r.o." || m["corrections"] == nil {
			t.Fatalf("typo: %d %v", code, m)
		}
		e.get(t, "/v1/search?mode=fuzzy&q=striebrna", true)
		code, h, m := e.get(t, "/v1/search?mode=fuzzy&q=rybaka", true) // third within a minute: burst is 2
		if code != 429 || h.Get("Retry-After") == "" || m["code"] != "rate_limited" {
			t.Fatalf("fuzzy limit: %d %v %v", code, h, m)
		}
		off := start(t, dsn, func(c *api.Config) { c.Fuzzy = false })
		if code, _, m := off.get(t, "/v1/search?mode=fuzzy&q=x", true); code != 400 || m["code"] != "fuzzy_disabled" {
			t.Fatalf("disabled: %d %v", code, m)
		}
	})

	t.Run("lookup by IČO", func(t *testing.T) {
		code, _, m := e.get(t, "/v1/lookup/99900001", true)
		if code != 200 {
			t.Fatalf("%d %v", code, m)
		}
		s := m["subjects"].([]any)[0]
		if path(s, "identity", "state") != "present" || path(s, "identity", "name") != "Zlatá Rybka, s.r.o." ||
			path(s, "tax", "dic") != "2099900001" || path(s, "vat", "payer") != true || path(s, "vat", "payerFrom") != "2020-08-01" ||
			path(s, "risk", "reliabilityIndex") != "spoľahlivý" || path(s, "identity", "source", "version") == "" {
			t.Fatalf("subject: %v", s)
		}
		_, _, m = e.get(t, "/v1/lookup/99900004", true)
		s = m["subjects"].([]any)[0]
		if path(s, "vat", "paragraph") != "§7a" || path(s, "vat", "payer") != false || path(s, "vat", "payerFrom") != nil ||
			path(s, "vat", "registeredOn") != "2023-09-06" {
			t.Fatalf("§7a: registered, not a payer: %v", path(s, "vat"))
		}
		if regs := path(s, "identity", "registrations").([]any); len(regs) != 2 {
			t.Fatalf("one IČO, two registrations: %v", regs)
		}
		if path(s, "tax", "state") != "absent" || path(s, "risk", "state") != "present" {
			t.Fatalf("states: tax %v, risk %v", path(s, "tax", "state"), path(s, "risk", "state"))
		}
	})

	t.Run("lookup by DIČ and IČ DPH", func(t *testing.T) {
		if _, _, m := e.get(t, "/v1/lookup/2099900001", true); path(m["subjects"].([]any)[0], "ico") != "99900001" {
			t.Fatalf("DIČ of a company: %v", m)
		}
		code, _, m := e.get(t, "/v1/lookup/2099900050", true)
		if code != 200 || path(m, "taxpayer", "dic") != "2099900050" || len(m["subjects"].([]any)) != 0 {
			t.Fatalf("natural person (authenticated): %d %v", code, m)
		}
		anon := start(t, dsn, func(c *api.Config) { c.AllowAnonymous = true })
		if code, _, m := anon.get(t, "/v1/lookup/2099900050", false); code != 403 {
			t.Fatalf("natural person on an anonymous instance: %d %v", code, m)
		}
		if code, _, _ := anon.get(t, "/v1/search?q=rybka", false); code != 200 {
			t.Fatalf("anonymous search: %d", code)
		}
		_, _, m = e.get(t, "/v1/lookup/SK2099900100", true)
		if subs := m["subjects"].([]any); len(subs) != 2 || path(subs[0], "vat", "icDph") != "SK2099900100" {
			t.Fatalf("VAT group: %v", m)
		}
		if code, _, m := e.get(t, "/v1/lookup/12", true); code != 400 || m["code"] != "invalid_identifier" {
			t.Fatalf("invalid: %d %v", code, m)
		}
		if code, _, m := e.get(t, "/v1/lookup/99900099", true); code != 404 || m["code"] != "not_found" {
			t.Fatalf("not found on fresh data: %d %v", code, m)
		}
	})

	t.Run("batch versions", func(t *testing.T) {
		code, m := e.post(t, "/v1/lookup:batch", `{"ids":["99900001","12","99900099"],"fields":"versions"}`)
		if code != 200 {
			t.Fatalf("%d %v", code, m)
		}
		items := m["items"].([]any)
		v1 := path(items[0], "versions").([]any)[0]
		if path(v1, "identity", "version") == "" || path(items[1], "code") != "invalid_identifier" || path(items[2], "status") != float64(404) {
			t.Fatalf("batch: %v", items)
		}
		_, m2 := e.post(t, "/v1/lookup:batch", `{"ids":["99900001"],"fields":"versions"}`)
		if path(path(m2["items"].([]any)[0], "versions").([]any)[0], "vat", "version") != path(v1, "vat", "version") {
			t.Fatal("versions must be deterministic")
		}
	})

	t.Run("sources and capabilities", func(t *testing.T) {
		_, _, m := e.get(t, "/v1/sources", true)
		if m["bootstrapping"] != false || len(m["sources"].([]any)) != 7 {
			t.Fatalf("sources: %v", m)
		}
		_, _, m = e.get(t, "/v1/capabilities", true)
		if path(m, "fuzzy", "enabled") != true || !strings.Contains(path(m, "fuzzy", "note").(string), "never substitutes exact identifier") {
			t.Fatalf("capabilities: %v", m)
		}
	})

	t.Run("stale data never says not found", func(t *testing.T) {
		conn := testkit.Conn(t, dsn)
		if _, err := conn.Exec(context.Background(), `UPDATE sync_source SET published_as_of = published_as_of - 10 WHERE source = 'rpo'`); err != nil {
			t.Fatal(err)
		}
		defer conn.Exec(context.Background(), `UPDATE sync_source SET published_as_of = published_as_of + 10 WHERE source = 'rpo'`)
		if code, _, m := e.get(t, "/v1/lookup/99900099", true); code != 503 || m["reason"] != "source_stale" {
			t.Fatalf("stale: %d %v", code, m)
		}
		_, _, m := e.get(t, "/v1/lookup/99900001", true)
		if path(m["subjects"].([]any)[0], "identity", "source", "stale") != true {
			t.Fatal("a stale section must be flagged")
		}
	})

	t.Run("objection keeps a subject out of name search only", func(t *testing.T) {
		conn := testkit.Conn(t, dsn)
		id, records, err := ingest.AddObjection(context.Background(), conn, "99900002", "ticket-1")
		if err != nil || records != 1 {
			t.Fatalf("add: %d records, %v", records, err)
		}
		_, _, m := e.get(t, "/v1/search?q=rybka", true)
		if strings.Contains(strings.Join(names(m), "|"), "Strieborná") {
			t.Fatal("a suppressed subject is still in name search")
		}
		if code, _, _ := e.get(t, "/v1/lookup/99900002", true); code != 200 {
			t.Fatalf("exact lookup of a suppressed subject: %d", code)
		}
		if still, err := ingest.RevokeObjection(context.Background(), conn, id); err != nil || len(still) != 0 {
			t.Fatalf("revoke: %v %v", still, err)
		}
		_, _, m = e.get(t, "/v1/search?q=rybka", true)
		if !strings.Contains(strings.Join(names(m), "|"), "Strieborná") {
			t.Fatal("an unsuppressed subject did not come back")
		}
	})

	t.Run("an objection is enforced at query time, not only by the search layer", func(t *testing.T) {
		// The row is written without rebuilding the search layer, as after an interrupted refresh:
		// the subject's search rows and dictionary words are still there.
		conn := testkit.Conn(t, dsn)
		if _, err := conn.Exec(context.Background(), `INSERT INTO objection (ico) VALUES ('99900001')`); err != nil {
			t.Fatal(err)
		}
		defer conn.Exec(context.Background(), `UPDATE objection SET revoked_at = now() WHERE ico = '99900001' AND revoked_at IS NULL`)
		for _, qq := range []string{"zlata", "zlata ryb", "rybka"} {
			if _, _, m := e.get(t, "/v1/search?q="+q(qq), true); strings.Contains(strings.Join(names(m), "|"), "Zlatá") {
				t.Fatalf("%q returned a suppressed subject", qq)
			}
		}
		fz := start(t, dsn, func(c *api.Config) { c.FuzzyEvery = time.Millisecond })
		code, _, m := fz.get(t, "/v1/search?mode=fuzzy&q="+q("zlta rybka"), true)
		if b, _ := json.Marshal(m); code != 200 || strings.Contains(strings.ToLower(string(b)), "zlat") {
			t.Fatalf("fuzzy search revealed a word that only a suppressed subject uses: %d %s", code, b)
		}
	})

	t.Run("nothing is served until the operator confirms the objections", func(t *testing.T) {
		conn := testkit.Conn(t, dsn)
		ctx := context.Background()
		if _, err := conn.Exec(ctx, `UPDATE register_setup SET objections_confirmed_at = NULL, objections_confirmed_as = NULL WHERE only_row`); err != nil {
			t.Fatal(err)
		}
		defer conn.Exec(ctx, `UPDATE register_setup SET objections_confirmed_at = now(), objections_confirmed_as = 'new' WHERE only_row`)
		for _, p := range []string{"/v1/search?q=rybka", "/v1/lookup/99900001"} {
			if code, _, m := e.get(t, p, true); code != 503 || m["reason"] != "setup" {
				t.Fatalf("%s on an unconfirmed installation: %d %v", p, code, m)
			}
		}
		if code, _, _ := e.get(t, "/readyz", false); code != 503 {
			t.Fatalf("readyz on an unconfirmed installation: %d", code)
		}
		if _, _, m := e.get(t, "/v1/sources", true); m["setupPending"] != true {
			t.Fatalf("sources must report the pending setup: %v", m["setupPending"])
		}
	})

	t.Run("a source dated in the future is never fresh", func(t *testing.T) {
		conn := testkit.Conn(t, dsn)
		if _, err := conn.Exec(context.Background(), `UPDATE sync_source SET published_as_of = published_as_of + 30 WHERE source = 'rpo'`); err != nil {
			t.Fatal(err)
		}
		defer conn.Exec(context.Background(), `UPDATE sync_source SET published_as_of = published_as_of - 30 WHERE source = 'rpo'`)
		if code, _, m := e.get(t, "/v1/lookup/99900099", true); code != 503 || m["reason"] != "source_stale" {
			t.Fatalf("future as-of date: %d %v", code, m)
		}
	})

	t.Run("logs carry no queries or identifiers", func(t *testing.T) {
		logs := e.logs.String()
		for _, secret := range []string{"zlata", "rybka", "99900001", "2099900050", "SK2099900100"} {
			if strings.Contains(strings.ToLower(logs), strings.ToLower(secret)) {
				t.Fatalf("the access log contains %q", secret)
			}
		}
		if !strings.Contains(logs, `"route":"/v1/lookup/{id}"`) || !strings.Contains(logs, `"caller":"tester"`) {
			t.Fatalf("access log lacks route or caller: %s", logs[:min(400, len(logs))])
		}
	})
}

func TestBootstrapping(t *testing.T) {
	e := start(t, testkit.Database(t), nil)
	if code, _, m := e.get(t, "/v1/search?q=x", true); code != 503 || m["reason"] != "bootstrapping" {
		t.Fatalf("search before the first publication: %d %v", code, m)
	}
	if code, _, _ := e.get(t, "/readyz", false); code != 503 {
		t.Fatalf("readyz before the first publication: %d", code)
	}
	if code, _, _ := e.get(t, "/livez", false); code != 200 {
		t.Fatal("livez must not depend on data")
	}
}
