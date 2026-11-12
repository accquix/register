package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

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
	if ok, err := ingest.SetupConfirmed(ctx, conn); err != nil {
		t.Fatal(err)
	} else if !ok {
		if err := ingest.ConfirmNoObjections(ctx, conn); err != nil { // a new installation
			t.Fatal(err)
		}
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
	person := testkit.TaxItem(50, "")                                              // a natural person without an IČO
	noID := testkit.Item{"NAZOV_DS": "Fiktívna Osoba Bez Identifikátora"}          // OQ-37: left out and counted
	newBiz := testkit.Item{"ICO": "99900008", "NAZOV_DS": "Nová Fiktívna Živnosť"} // D52: an IČO, no DIČ yet
	// D56: taxpayers without an IČO for the full-name search: two namesakes (the same words in
	// another order) and a longer name that contains theirs
	named := func(n int, name string) testkit.Item { it := testkit.TaxItem(n, ""); it["NAZOV_DS"] = name; return it }
	srv.Put("ds_dsrdp", testkit.FSZip("ds_dsrdp", day(0), []testkit.Item{tax1, noID, person, noID, newBiz,
		named(51, "Jana Fiktívna"), named(52, "FIKTÍVNA Jana"), named(53, "Jana Fiktívna Malá"),
		named(54, "Ján Fiktívny")})) // the sole trader 99900004's own name, under a personal DIČ
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

	t.Run("sources report items left out for having no identifier", func(t *testing.T) {
		_, _, m := e.get(t, "/v1/sources", true)
		for _, x := range m["sources"].([]any) {
			src := x.(map[string]any)
			want := 0.0
			if src["source"] == "fs:ds_dsrdp" {
				want = 2
			}
			got, _ := src["skippedItems"].(float64)
			if got != want {
				t.Fatalf("%v: skippedItems %v, want %v", src["source"], src["skippedItems"], want)
			}
			wantNew := 0.0
			if src["source"] == "fs:ds_dsrdp" {
				wantNew = 1 // reported apart: a new business is not a fault
			}
			if got, _ := src["itemsWithoutDic"].(float64); got != wantNew {
				t.Fatalf("%v: itemsWithoutDic %v, want %v", src["source"], src["itemsWithoutDic"], wantNew)
			}
		}
	})

	t.Run("search cards carry the full address, the same as the lookup", func(t *testing.T) {
		for _, p := range []string{"/v1/search?q=" + q("kadernictvo lucia"), "/v1/search?q=99900004"} {
			_, _, m := e.get(t, p, true)
			rs, _ := m["results"].([]any)
			if len(rs) == 0 {
				t.Fatalf("%s: no results %v", p, m)
			}
			card := rs[0].(map[string]any)
			_, _, lk := e.get(t, "/v1/lookup/"+card["ico"].(string), true)
			want := path(lk["subjects"].([]any)[0], "identity", "address")
			got := card["address"]
			if got == nil || path(got, "street") != "Živnostenská" || path(got, "postalCode") != "988 88" {
				t.Fatalf("%s: card address %v", p, got)
			}
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(want)
			if !bytes.Equal(gb, wb) {
				t.Fatalf("%s: card address %s differs from the lookup's %s", p, gb, wb)
			}
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
		// D26: a natural person without an IČO is never found by name, but the exact DIČ typed into
		// the box gives the same as the lookup: name and municipality, never an address
		if _, _, m := e.get(t, "/v1/search?q=2099900050", true); len(names(m)) != 1 {
			t.Fatalf("exact DIČ of a taxpayer: %v", m)
		} else if r := m["results"].([]any)[0]; path(r, "kind") != "taxpayer" || path(r, "dic") != "2099900050" || path(r, "ico") != nil || path(r, "address") != nil {
			t.Fatalf("taxpayer card: %v", r)
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
		if code, _, m := anon.get(t, "/v1/search?q=2099900050", false); code != 200 || len(names(m)) != 0 {
			t.Fatalf("a taxpayer's DIČ in the box on an anonymous instance: %d %v", code, m)
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

	t.Run("per-key limits are off by default, anonymous callers are always limited", func(t *testing.T) {
		def := start(t, dsn, func(c *api.Config) {
			c.RatePerSecond, c.RateBurst, c.FuzzyEvery = 0, 0, 0
			c.AllowAnonymous, c.AnonRatePerSecond, c.AnonRateBurst = true, 0.001, 3
		})
		for i := range 50 {
			if code, _, m := def.get(t, "/v1/search?q=rybka", true); code != 200 {
				t.Fatalf("request %d with a key and no per-key limit: %d %v", i, code, m)
			}
		}
		for i := range 4 {
			if code, _, m := def.get(t, "/v1/search?mode=fuzzy&q="+q("zlta rybka"), true); code != 200 {
				t.Fatalf("fuzzy %d with a key and no per-key limit: %d %v", i, code, m)
			}
		}
		_, _, m := def.get(t, "/v1/capabilities", true)
		if r, _ := m["rate"].(map[string]any); len(r) != 0 || path(m, "anonymousRate", "burst") != float64(3) ||
			path(m, "fuzzy", "perKeyEvery") != nil || path(m, "fuzzy", "anonymousEvery") != "5s" {
			t.Fatalf("capabilities: %v", m)
		}
		var last int
		for range 4 {
			last, _, _ = def.get(t, "/v1/search?q=rybka", false)
		}
		if last != 429 {
			t.Fatalf("the fourth anonymous request over a burst of 3: %d", last)
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
		id, cov, err := ingest.AddObjection(context.Background(), conn, "99900002", "ticket-1")
		if err != nil || cov.Records != 1 || cov.Hidden != 1 {
			t.Fatalf("add: %+v, %v", cov, err)
		}
		_, _, m := e.get(t, "/v1/search?q=rybka", true)
		if strings.Contains(strings.Join(names(m), "|"), "Strieborná") {
			t.Fatal("a suppressed subject is still in name search")
		}
		if code, _, _ := e.get(t, "/v1/lookup/99900002", true); code != 200 {
			t.Fatalf("exact lookup of a suppressed subject: %d", code)
		}
		if cov, err := ingest.RevokeObjection(context.Background(), conn, id); err != nil || cov.Hidden != 0 {
			t.Fatalf("revoke: %+v %v", cov, err)
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

// D56: taxpayers without an IČO are found by name only when the operator turns it on, only by the
// full name, only in the default mode, only with an API key, within their own limit, and never
// when they objected. Exact identifiers are not affected by any of it.
func TestTaxpayerNameSearch(t *testing.T) {
	ctx := context.Background()
	dsn := testkit.Database(t)
	populate(t, dsn)
	conn := testkit.Conn(t, dsn)
	taxpayers := func(m map[string]any) []string {
		var out []string
		for _, r := range m["results"].([]any) {
			if path(r, "kind") == "taxpayer" {
				if path(r, "address") != nil || path(r, "ico") != nil {
					t.Fatalf("a taxpayer card carries only the DIČ, name and municipality: %v", r)
				}
				out = append(out, path(r, "dic").(string))
			}
		}
		sort.Strings(out)
		return out
	}
	want := func(t *testing.T, e env, qq, status string, dics ...string) {
		t.Helper()
		code, _, m := e.get(t, "/v1/search?q="+q(qq), true)
		if got := strings.Join(taxpayers(m), ","); code != 200 || got != strings.Join(dics, ",") || (m["taxpayers"] != nil || status != "") && m["taxpayers"] != status {
			t.Fatalf("%q: %d, taxpayers %q (%v), want %q (%s)", qq, code, got, m["taxpayers"], strings.Join(dics, ","), status)
		}
	}

	t.Run("off by default", func(t *testing.T) {
		e := start(t, dsn, nil)
		want(t, e, "Jana Fiktívna", "")
		if _, _, m := e.get(t, "/v1/capabilities", true); path(m, "taxpayerSearch", "enabled") != false {
			t.Fatalf("capabilities: %v", m)
		}
		d := start(t, dsn, func(c *api.Config) { c.SearchTaxpayers = true })
		if _, _, m := d.get(t, "/v1/capabilities", true); path(m, "taxpayerSearch", "perSecond") != float64(1) || path(m, "taxpayerSearch", "burst") != float64(10) {
			t.Fatalf("default taxpayer limit: %v", m)
		}
	})

	on := start(t, dsn, func(c *api.Config) {
		c.SearchTaxpayers, c.TaxpayerRatePerSecond, c.TaxpayerRateBurst = true, 1000, 1000
	})
	t.Run("the full name only, in any word order", func(t *testing.T) {
		want(t, on, "Jana Fiktívna", "searched", "2099900051", "2099900052")
		want(t, on, "fiktivna JANA", "searched", "2099900051", "2099900052")
		want(t, on, "Jana Fiktívna Malá", "searched", "2099900053")
		want(t, on, "jana", "not_searched")           // one word is never a full name
		want(t, on, "jana fikt", "searched")          // no prefixes
		want(t, on, "jana fiktivna mal", "searched")  // no prefixes
		want(t, on, "jana fiktivna nova", "searched") // no other words
		want(t, on, "jana fiktivn", "searched")       // no typos
		want(t, on, "zlata rybka", "searched")        // a company name finds no taxpayer
		if _, _, m := on.get(t, "/v1/search?mode=fuzzy&q="+q("jana fiktivna"), true); len(taxpayers(m)) != 0 || m["taxpayers"] != "not_searched" {
			t.Fatalf("fuzzy never searches taxpayers: %v", m)
		}
		if _, _, m := on.get(t, "/v1/capabilities", true); path(m, "taxpayerSearch", "enabled") != true || path(m, "taxpayerSearch", "match") != "full_name" ||
			path(m, "taxpayerSearch", "burst") != float64(1000) {
			t.Fatalf("capabilities: %v", m)
		}
	})

	t.Run("after the businesses, never instead of them", func(t *testing.T) {
		code, _, m := on.get(t, "/v1/search?limit=1&q="+q("Ján Fiktívny"), true)
		if code != 200 || len(names(m)) != 1 || len(taxpayers(m)) != 0 || m["taxpayers"] != "not_searched" {
			t.Fatalf("a business that fills the limit stays: %v", m)
		}
		want(t, on, "Ján Fiktívny", "searched", "2099900054")
	})

	t.Run("a stale DIČ list never answers for taxpayers", func(t *testing.T) {
		if _, err := conn.Exec(ctx, `UPDATE sync_source SET published_as_of = published_as_of - 30 WHERE source = 'fs:ds_dsrdp'`); err != nil {
			t.Fatal(err)
		}
		want(t, on, "Jana Fiktívna", "stale")
		if _, err := conn.Exec(ctx, `UPDATE sync_source SET published_as_of = published_as_of + 30 WHERE source = 'fs:ds_dsrdp'`); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("their own limit, and the search still answers", func(t *testing.T) {
		lim := start(t, dsn, func(c *api.Config) { c.SearchTaxpayers, c.TaxpayerRatePerSecond, c.TaxpayerRateBurst = true, 0.001, 1 })
		want(t, lim, "jana", "not_searched") // a query that cannot match costs nothing
		want(t, lim, "Jana Fiktívna", "searched", "2099900051", "2099900052")
		want(t, lim, "Jana Fiktívna", "rate_limited")
		if _, _, m := lim.get(t, "/v1/search?q=rybka", true); len(names(m)) == 0 {
			t.Fatal("over the taxpayer limit, companies are still found")
		}
	})

	t.Run("an objection by DIČ keeps the taxpayer out of name search only", func(t *testing.T) {
		id, cov, err := ingest.AddObjection(ctx, conn, "2099900051", "ticket-9")
		if err != nil || cov.Records != 1 || cov.Hidden != 1 {
			t.Fatalf("objection: %v %+v", err, cov)
		}
		want(t, on, "Jana Fiktívna", "searched", "2099900052")
		if code, _, m := on.get(t, "/v1/lookup/2099900051", true); code != 200 || path(m, "taxpayer", "dic") != "2099900051" {
			t.Fatalf("exact lookup is unaffected: %d %v", code, m)
		}
		if _, _, m := on.get(t, "/v1/search?q=2099900051", true); strings.Join(taxpayers(m), ",") != "2099900051" {
			t.Fatalf("the exact DIČ in the box is unaffected: %v", m)
		}
		if _, err := ingest.ReleaseLink(ctx, conn, id, 51, "", true); err == nil {
			t.Fatal("an objection by DIČ has no links to release")
		}
		if _, err := ingest.RevokeObjection(ctx, conn, id); err != nil {
			t.Fatal(err)
		}
		want(t, on, "Jana Fiktívna", "searched", "2099900051", "2099900052")
		if _, _, err := ingest.AddObjection(ctx, conn, "2099900001", ""); !errors.Is(err, ingest.ErrDICHasICO) {
			t.Fatalf("the DIČ of a subject with an IČO: object by the IČO: %v", err)
		}
	})

	t.Run("an objection by IČO also hides a taxpayer row with the objected subject's name", func(t *testing.T) {
		id, _, err := ingest.AddObjection(ctx, conn, "99900004", "")
		if err != nil {
			t.Fatal(err)
		}
		// the sole trader's records are out of name search, and so is the DIČ row that may be theirs
		want(t, on, "Ján Fiktívny", "searched")
		if _, err := ingest.RevokeObjection(ctx, conn, id); err != nil {
			t.Fatal(err)
		}
		want(t, on, "Ján Fiktívny", "searched", "2099900054")
	})

	t.Run("a person who objected by DIČ and then appears in RPO stays hidden", func(t *testing.T) {
		id, _, err := ingest.AddObjection(ctx, conn, "2099900054", "")
		if err != nil {
			t.Fatal(err)
		}
		dsrdp, _ := fs.ListBySlug("ds_dsrdp")
		day := time.Now().UTC().Format("2006-01-02")
		named := func(n int, ico, name string) testkit.Item {
			it := testkit.TaxItem(n, ico)
			it["NAZOV_DS"] = name
			return it
		}
		version := func(ico54 string) {
			t.Helper()
			srv := testkit.NewFSServer(t)
			srv.Put("ds_dsrdp", testkit.FSZip("ds_dsrdp", day, []testkit.Item{testkit.TaxItem(1, "99900001"), testkit.TaxItem(50, ""),
				named(51, "", "Jana Fiktívna"), named(52, "", "FIKTÍVNA Jana"), named(53, "", "Jana Fiktívna Malá"), named(54, ico54, "Ján Fiktívny")}))
			if err := syncer.NewFS(conn, syncer.FSConfig{Client: fs.NewClient(srv.URL()), WorkDir: t.TempDir(), SkipMinRows: true, MaxDrop: 1,
				Lists: []fs.List{dsrdp}}).Run(ctx); err != nil {
				t.Fatal(err)
			}
		}
		visible := func() bool {
			_, _, m := on.get(t, "/v1/search?q="+q("kadernictvo lucia"), true)
			return len(names(m)) > 0
		}
		if !visible() {
			t.Fatal("the sole trader must be in name search before the DIČ list ties the DIČ to the IČO")
		}
		version("99900004") // the DIČ list now gives the objected DIČ the sole trader's IČO
		if visible() {
			t.Fatal("the person objected by DIČ: their RPO records must leave name search on their own")
		}
		lst, _ := ingest.ListObjections(ctx, conn)
		for _, o := range lst {
			if o.ID == id && (len(o.Links) == 0 || o.Links[0].Basis != ingest.BasisDICList || o.DICHasICO != "99900004") {
				t.Fatalf("the link must be recorded with its basis: %+v", o)
			}
		}
		version("") // the IČO disappears from the DIČ list again: the link stays (I2)
		if visible() {
			t.Fatal("a link stays when the DIČ list changes again")
		}
		if _, err := ingest.RevokeObjection(ctx, conn, id); err != nil {
			t.Fatal(err)
		}
		if !visible() {
			t.Fatal("after the undo the sole trader is back")
		}
	})

	t.Run("an objection by DIČ whose row gains an IČO is flagged", func(t *testing.T) {
		id, _, err := ingest.AddObjection(ctx, conn, "2099900053", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, `UPDATE fs_tax_subject SET ico = '99900053' WHERE dic = '2099900053'`); err != nil {
			t.Fatal(err)
		}
		lst, err := ingest.ListObjections(ctx, conn)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range lst {
			if o.ID == id && o.DICHasICO != "99900053" {
				t.Fatalf("not flagged: %+v", o)
			}
		}
		if _, err := conn.Exec(ctx, `UPDATE fs_tax_subject SET ico = NULL WHERE dic = '2099900053'`); err != nil {
			t.Fatal(err)
		}
		if _, err := ingest.RevokeObjection(ctx, conn, id); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a restored objection by DIČ holds in a rebuilt database", func(t *testing.T) {
		if _, _, err := ingest.AddObjection(ctx, conn, "2099900052", ""); err != nil {
			t.Fatal(err)
		}
		backup, err := ingest.ListObjections(ctx, conn)
		if err != nil {
			t.Fatal(err)
		}
		fresh := testkit.Database(t)
		if _, err := ingest.RestoreObjections(ctx, testkit.Conn(t, fresh), backup); err != nil {
			t.Fatal(err)
		}
		populate(t, fresh)
		e := start(t, fresh, func(c *api.Config) { c.SearchTaxpayers = true })
		want(t, e, "Jana Fiktívna", "searched", "2099900051")
		// and the restore added the revoked one as revoked: 51 is findable, 52 is not
		lst, err := ingest.ListObjections(ctx, testkit.Conn(t, fresh))
		if err != nil || len(lst) != len(backup) {
			t.Fatalf("restored: %d of %d, %v", len(lst), len(backup), err)
		}
	})

	t.Run("never on an instance with anonymous access", func(t *testing.T) {
		anon := start(t, dsn, func(c *api.Config) { c.SearchTaxpayers, c.AllowAnonymous = true, true })
		for _, withKey := range []bool{true, false} {
			if _, _, m := anon.get(t, "/v1/search?q="+q("Jana Fiktívna"), withKey); len(taxpayers(m)) != 0 || m["taxpayers"] != nil {
				t.Fatalf("anonymous instance (key %v): %v", withKey, m)
			}
		}
	})

	t.Run("MCP gives the same", func(t *testing.T) {
		e := start(t, dsn, func(c *api.Config) { c.SearchTaxpayers, c.MCP = true, true })
		cs, err := connect(t, e, key)
		if err != nil {
			t.Fatal(err)
		}
		text := callTool(t, cs, "search_subjects", map[string]any{"q": "Jana Fiktívna"}).Content[0].(*mcp.TextContent).Text
		if !strings.Contains(text, "2099900051") || strings.Contains(text, "2099900052") {
			t.Fatalf("MCP: the objection must hold, the other namesake must be found: %s", text)
		}
	})
}
