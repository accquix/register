package search_test

// The search relevance test set (OQ-16, D50). Every rule of the ranking and every promise of the
// search documentation is one case below, run on a small synthetic register through the real sync
// code. The names are invented; IČOs are in the fictional 9990xxxx range. Checks on the real data
// are run locally by the operator and never committed (CLAUDE.md: code only, never data).

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accquix/register/internal/ingest"
	"github.com/accquix/register/internal/normalize"
	"github.com/accquix/register/internal/search"
	"github.com/accquix/register/internal/sources/rpo"
	"github.com/accquix/register/internal/syncer"
	"github.com/accquix/register/internal/testkit"
)

// corpus: the display name of each subject, by its number (IČO 9990nnnn).
var corpus = map[int]string{
	1:  "Rybka, s.r.o.",
	2:  "Rybka Plus, s.r.o.",
	3:  "Rybkáreň Dunajská, s.r.o.",
	4:  "Zlatá Rybka, s.r.o.",
	5:  "Lipa Servis, s.r.o.",
	6:  "Lipa Servis Východ, s.r.o.",
	7:  "Modrá Lipa, a.s.",
	8:  "Sova Trade, s.r.o.",
	9:  "Sovák Group, s.r.o.",
	10: "Stavebná firma Kotva, s.r.o.",
	11: "ŽELEZIARNE TICHÁ DOLINA, a.s.",
	12: "Kaderníctvo Lucia, s.r.o.", // two different subjects with the same name
	13: "Kaderníctvo Lucia, s.r.o.",
	14: "Zaniknutá Rybka, s.r.o.", // closed: not in name search
	15: "Utajená Rybka, s.r.o.",   // objected: never in name search
	16: "AB Systémy, s.r.o.",
	17: "Pekáreň U Lipy, s.r.o.",
}

// load syncs the corpus into a fresh database and returns a Searcher on it.
func load(t *testing.T) *search.Searcher {
	t.Helper()
	ctx := context.Background()
	dsn := testkit.Database(t)
	conn := testkit.Conn(t, dsn)
	if err := ingest.ConfirmNoObjections(ctx, conn); err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	var recs []testkit.Rec
	for n := 1; n <= len(corpus); n++ {
		r := testkit.Company(n, corpus[n])
		if strings.HasSuffix(corpus[n], ", a.s.") {
			r.LegalCode, r.LegalForm = "121", "Akciová spoločnosť"
		}
		if n == 14 {
			r.Terminated = time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02")
		}
		recs = append(recs, r)
	}
	b := testkit.NewBucket(t)
	b.PutSnapshot(day, recs, 100)
	if err := syncer.NewRPO(conn, syncer.RPOConfig{Bucket: rpo.NewBucket(b.URL()), WorkDir: t.TempDir()}).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ingest.AddObjection(ctx, conn, ico(15), "relevance test"); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return search.New(pool, search.Config{})
}

func ico(n int) string { return fmt.Sprintf("9990%04d", n) }

// rel is one relevance case. want lists subjects in the order they must appear (others may come
// between them); absent lists subjects that must not appear at all.
type rel struct {
	name   string
	query  string
	fuzzy  bool
	first  int   // the subject that must rank first (0: no check)
	want   []int // must appear, in this relative order
	absent []int
}

func TestSearchRelevance(t *testing.T) {
	s := load(t)
	cases := []rel{
		{name: "the exact name ranks first", query: "rybka", first: 1, want: []int{1, 2}},
		{name: "the legal form in the query is ignored", query: "Rybka, s.r.o.", first: 1},
		{name: "diacritics and case are ignored", query: "RYBKAREN dunajska", first: 3},
		{name: "a whole word beats a word prefix", query: "sova", first: 8, want: []int{8, 9}},
		{name: "a shorter name first when the rest is equal", query: "lipa serv", first: 5, want: []int{5, 6}},
		{name: "word order is ignored", query: "lipa modra", first: 7},
		{name: "the start of any word matches", query: "kotva", first: 10},
		{name: "a name in capitals is found", query: "zeleziarne ticha", first: 11},
		{name: "two subjects with the same name both appear", query: "kadernictvo lucia", want: []int{12}, first: 0},
		{name: "a closed subject is not proposed", query: "zaniknuta rybka", absent: []int{14}},
		{name: "an objected subject is never proposed", query: "utajena rybka", absent: []int{15}},
		{name: "a two-letter query works", query: "ab", first: 16},
		{name: "names that start with the query come before names that only contain it", query: "lip", want: []int{5, 7, 17}},
		{name: "fuzzy corrects one dropped letter", query: "zlta rybka", fuzzy: true, first: 4},
		{name: "fuzzy corrects two swapped letters", query: "kotav", fuzzy: true, first: 10},
		{name: "fuzzy corrects a swap in a short word", query: "rybka plsu", fuzzy: true, first: 2},
		{name: "fuzzy corrects a wrong letter in a short word", query: "sava trade", fuzzy: true, first: 8},
	}
	ctx := context.Background()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			core, _ := normalize.SplitName(c.query)
			var res search.Result
			var err error
			if c.fuzzy {
				res, err = s.Fuzzy(ctx, core, 10)
			} else {
				res, err = s.Default(ctx, core, 10)
			}
			if err != nil {
				t.Fatal(err)
			}
			pos := map[string]int{}
			var got []string
			for i, h := range res.Hits {
				pos[h.SKey] = i
				got = append(got, h.Name)
			}
			if c.first != 0 && (len(res.Hits) == 0 || res.Hits[0].SKey != ico(c.first)) {
				t.Fatalf("%q: want %q first, got %q", c.query, corpus[c.first], got)
			}
			last := -1
			for _, n := range c.want {
				p, ok := pos[ico(n)]
				if !ok {
					t.Fatalf("%q: %q missing from %q", c.query, corpus[n], got)
				}
				if p < last {
					t.Fatalf("%q: %q out of order in %q", c.query, corpus[n], got)
				}
				last = p
			}
			for _, n := range c.absent {
				if _, ok := pos[ico(n)]; ok {
					t.Fatalf("%q: %q must not be proposed, got %q", c.query, corpus[n], got)
				}
			}
		})
	}
	// the same name of two subjects: both are proposed, each once
	res, err := s.Default(ctx, "kadernictvo lucia", 10)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, h := range res.Hits {
		seen[h.SKey]++
	}
	if seen[ico(12)] != 1 || seen[ico(13)] != 1 {
		t.Fatalf("both subjects named %q, once each: %v", corpus[12], seen)
	}
}

func TestEditVariantsAreOneEditAway(t *testing.T) {
	vs := search.EditVariants([]rune("plsu"))
	has := map[string]bool{}
	for _, v := range vs {
		has[v] = true
		if d := search.OSA([]rune("plsu"), []rune(v)); d != 1 {
			t.Fatalf("%q is %d edits from plsu", v, d)
		}
	}
	for _, want := range []string{"plus", "psu", "plsa", "plsua"} {
		if !has[want] {
			t.Fatalf("%q missing", want)
		}
	}
	if has["plsu"] || len(vs) > 400 {
		t.Fatalf("%d variants, original included: %v", len(vs), has["plsu"])
	}
}
