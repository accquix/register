package demo_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accquix/register/internal/api"
	"github.com/accquix/register/internal/demo"
	"github.com/accquix/register/internal/ingest"
	"github.com/accquix/register/internal/normalize"
	"github.com/accquix/register/internal/sources/rpo"
	"github.com/accquix/register/internal/syncer"
	"github.com/accquix/register/internal/testkit"
)

func TestDatasetIsFictional(t *testing.T) {
	set := demo.Dataset(time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC))
	if len(set.Records) < 150 || len(set.Lists) != 6 {
		t.Fatalf("%d records, %d lists", len(set.Records), len(set.Lists))
	}
	seen := map[string]bool{}
	for _, r := range set.Records {
		if !strings.HasPrefix(r.ICO, "999") || seen[r.ICO] {
			t.Fatalf("IČO %q outside the fictional range or repeated", r.ICO)
		}
		seen[r.ICO] = true
		if ico, ok := normalize.ICO(r.ICO); !ok || ico != r.ICO {
			t.Fatalf("IČO %q is not a valid 8-digit IČO", r.ICO)
		}
		if r.ID < 900_000_000 {
			t.Fatalf("RPO id %d outside the fictional range", r.ID)
		}
	}
}

func TestDemo(t *testing.T) {
	ctx := context.Background()
	dsn := testkit.Database(t)
	conn := testkit.Conn(t, dsn)

	sum, err := demo.Load(ctx, conn, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.AlreadyLoaded || sum.Subjects < 150 {
		t.Fatalf("summary %+v", sum)
	}
	if again, err := demo.Load(ctx, conn, t.TempDir(), nil); err != nil || !again.AlreadyLoaded {
		t.Fatalf("a second load must be a no-op: %+v %v", again, err)
	}
	if ok, _ := demo.IsDemo(ctx, conn); !ok {
		t.Fatal("the database is not marked as a demo")
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	h, _ := api.New(pool, api.Config{Version: "test", AllowAnonymous: true, Fuzzy: true, RatePerSecond: 1000, RateBurst: 1000,
		FuzzyEvery: time.Millisecond, FuzzyConcurrent: 1, FuzzyTimeout: 2 * time.Second, QueryTimeout: 3 * time.Second}, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	get := func(path string, out any) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var raw json.RawMessage
		_ = json.NewDecoder(resp.Body).Decode(&raw)
		if out != nil {
			_ = json.Unmarshal(raw, out)
		}
		return resp.StatusCode, string(raw)
	}

	t.Run("ready, and says it is a demo", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		buf := make([]byte, 200)
		n, _ := resp.Body.Read(buf)
		b.Write(buf[:n])
		_ = resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(b.String(), "demo") {
			t.Fatalf("readyz %d %q", resp.StatusCode, b.String())
		}
		var caps struct{ Demo bool }
		if code, _ := get("/v1/capabilities", &caps); code != 200 || !caps.Demo {
			t.Fatalf("capabilities: %d demo=%v", code, caps.Demo)
		}
	})

	t.Run("search, lookup and sources answer with the demo attribution", func(t *testing.T) {
		var s struct {
			Results []struct {
				ICO, Name string
				Address   *struct{ Municipality string }
			}
			Attribution []string
		}
		if code, raw := get("/v1/search?q=zlata", &s); code != 200 || len(s.Results) == 0 {
			t.Fatalf("search: %d %s", code, raw)
		}
		if len(s.Attribution) != 1 || !strings.HasPrefix(s.Attribution[0], "DEMO DATA") {
			t.Fatalf("search attribution %v", s.Attribution)
		}
		if s.Results[0].Address == nil || s.Results[0].Address.Municipality == "" {
			t.Fatalf("a demo card has no address: %+v", s.Results[0])
		}
		var l struct {
			Subjects []struct {
				VAT struct{ State string }
			}
			Attribution []string
		}
		if code, raw := get("/v1/lookup/"+s.Results[0].ICO, &l); code != 200 || len(l.Subjects) != 1 || !strings.HasPrefix(l.Attribution[0], "DEMO DATA") {
			t.Fatalf("lookup: %d %s", code, raw)
		}
		var src struct {
			Demo        bool
			Sources     []struct{ State string }
			Attribution []string
		}
		if code, raw := get("/v1/sources", &src); code != 200 || !src.Demo || len(src.Sources) != 7 || !strings.HasPrefix(src.Attribution[0], "DEMO DATA") {
			t.Fatalf("sources: %d %s", code, raw)
		}
	})

	t.Run("a VAT group and fuzzy search are there to try", func(t *testing.T) {
		var l struct{ Subjects []any }
		if code, raw := get("/v1/lookup/SK2099900033", &l); code != 200 || len(l.Subjects) != 2 {
			t.Fatalf("VAT group: %d %s", code, raw)
		}
		var f struct{ Results []any }
		if code, raw := get("/v1/search?q=zlta%20rybka&mode=fuzzy", &f); code != 200 || len(f.Results) == 0 {
			t.Fatalf("fuzzy: %d %s", code, raw)
		}
	})

}

func TestDemoRefusesRealData(t *testing.T) {
	ctx := context.Background()
	dsn := testkit.Database(t)
	conn := testkit.Conn(t, dsn)
	day := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	b := testkit.NewBucket(t)
	b.PutSnapshot(day, []testkit.Rec{testkit.Company(1, "Skutočne Načítaná, s.r.o.")}, 100)
	if err := syncer.NewRPO(conn, syncer.RPOConfig{Bucket: rpo.NewBucket(b.URL()), WorkDir: t.TempDir()}).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := demo.Load(ctx, conn, t.TempDir(), nil); !errors.Is(err, demo.ErrRealData) {
		t.Fatalf("want ErrRealData, got %v", err)
	}
	if ok, _ := demo.IsDemo(ctx, conn); ok {
		t.Fatal("a database with real data must not be marked as a demo")
	}
	if ok, _ := ingest.SetupConfirmed(ctx, conn); ok {
		t.Fatal("demo must not confirm a real installation")
	}
}
