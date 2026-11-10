// Package search implements the name search (D17, D18) over the search layer that sync maintains:
// search_name (one row per distinct official name of an active subject), search_token (its words)
// and search_word (the word dictionary). Identifiers are matched exactly elsewhere, never here.
package search

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accquix/register/internal/normalize"
)

// Config tunes the adaptive word step (D17).
type Config struct {
	WordStepMax int // run the word step when the rarest query word occurs at most this often (1000)
	WordCap     int // candidates read by the word step (1000)
}

// Searcher runs searches on a pool.
type Searcher struct {
	pool *pgxpool.Pool
	cfg  Config
}

// New returns a Searcher.
func New(pool *pgxpool.Pool, cfg Config) *Searcher {
	if cfg.WordStepMax <= 0 {
		cfg.WordStepMax = 1000
	}
	if cfg.WordCap <= 0 {
		cfg.WordCap = 1000
	}
	return &Searcher{pool: pool, cfg: cfg}
}

// Hit is one matched name of a subject.
type Hit struct {
	SKey     string
	NameCore string
	Name     string // display form of the matched name
	Typos    int    // fuzzy only: corrections needed
}

// Result is a ranked list of distinct subjects.
type Result struct {
	Hits  []Hit
	Steps string   // which steps ran (for diagnostics, never logged with the query)
	Fixes []string // fuzzy: "word→correction"
}

// Words returns the query words used by the word step: 2+ characters, at most the 4 longest.
func Words(core string) []string {
	ws := normalize.Words(core, 2)
	if len(ws) > 4 {
		sort.SliceStable(ws, func(a, b int) bool { return len([]rune(ws[a])) > len([]rune(ws[b])) })
		ws = ws[:4]
	}
	return ws
}

// Default is the adaptive search: name prefix, then word prefixes when useful.
func (s *Searcher) Default(ctx context.Context, core string, limit int) (Result, error) {
	var res Result
	if core == "" {
		return res, nil
	}
	pre, err := s.namePrefix(ctx, core)
	if err != nil {
		return res, err
	}
	res.Steps = fmt.Sprintf("name_prefix(%d)", len(pre))
	all := pre
	if len([]rune(core)) > 2 {
		words := Words(core)
		if len(words) > 0 {
			conds := make([]cond, 0, len(words))
			minSize := -1
			for _, w := range words {
				n, err := s.rangeSize(ctx, w)
				if err != nil {
					return res, err
				}
				conds = append(conds, cond{prefix: w, size: n})
				if minSize < 0 || n < minSize {
					minSize = n
				}
			}
			if len(pre) < 10 || minSize <= s.cfg.WordStepMax {
				cands, capped, err := s.wordCandidates(ctx, conds)
				if err != nil {
					return res, err
				}
				all = append(all, cands...)
				res.Steps += fmt.Sprintf("+words(%d", len(cands))
				if capped {
					res.Steps += ",capped"
				}
				res.Steps += ")"
			} else {
				res.Steps += "+words_skipped"
			}
		}
	}
	res.Hits = rank(core, Words(core), all, limit)
	return res, nil
}

func (s *Searcher) namePrefix(ctx context.Context, core string) ([]Hit, error) {
	rows, err := s.pool.Query(ctx, `SELECT skey, name_core, name FROM search_name WHERE name_core LIKE $1
ORDER BY name_core USING ~<~ LIMIT 50`, core+"%")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Hit, error) {
		var h Hit
		err := r.Scan(&h.SKey, &h.NameCore, &h.Name)
		return h, err
	})
}

func (s *Searcher) rangeSize(ctx context.Context, w string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT coalesce(sum(freq), 0)::int FROM search_word WHERE word LIKE $1`, w+"%").Scan(&n)
	return n, err
}

// cond is one query word: a prefix, or a set of exact (corrected) words.
type cond struct {
	prefix string
	exact  []string
	size   int
}

// wordCandidates returns the names of subjects whose words match every condition. The rarest
// condition drives; the others are checked per subject (index on search_token(skey, token)).
func (s *Searcher) wordCandidates(ctx context.Context, conds []cond) ([]Hit, bool, error) {
	sort.SliceStable(conds, func(a, b int) bool { return conds[a].size < conds[b].size })
	var args []any
	match := func(alias string, c cond) string {
		if c.exact != nil {
			args = append(args, c.exact)
			return fmt.Sprintf("%s.token = ANY($%d)", alias, len(args))
		}
		args = append(args, c.prefix+"%")
		return fmt.Sprintf("%s.token LIKE $%d", alias, len(args))
	}
	where := match("t", conds[0])
	for i, c := range conds[1:] {
		a := fmt.Sprintf("o%d", i)
		where += fmt.Sprintf(" AND EXISTS (SELECT 1 FROM search_token %s WHERE %s.skey = t.skey AND %s)", a, a, match(a, c))
	}
	args = append(args, s.cfg.WordCap)
	sql := `SELECT n.skey, n.name_core, n.name FROM (SELECT DISTINCT t.skey FROM search_token t WHERE ` + where +
		fmt.Sprintf(` LIMIT $%d) c JOIN search_name n USING (skey)`, len(args))
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, false, err
	}
	hits, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Hit, error) {
		var h Hit
		err := r.Scan(&h.SKey, &h.NameCore, &h.Name)
		return h, err
	})
	if err != nil {
		return nil, false, err
	}
	keys := map[string]bool{}
	for _, h := range hits {
		keys[h.SKey] = true
	}
	return hits, len(keys) >= s.cfg.WordCap, nil
}

func wholeWords(name string, words []string) int {
	padded := " " + name + " "
	n := 0
	for _, w := range words {
		if strings.Contains(padded, " "+w+" ") {
			n++
		}
	}
	return n
}

// rank orders by: exact name, fewer typos, more query words as whole words, name starts with the
// query, shorter name, then name; and keeps the best name per subject. No popularity signal.
func rank(core string, words []string, hits []Hit, limit int) []Hit {
	sort.SliceStable(hits, func(a, b int) bool {
		x, y := hits[a], hits[b]
		if (x.NameCore == core) != (y.NameCore == core) {
			return x.NameCore == core
		}
		if x.Typos != y.Typos {
			return x.Typos < y.Typos
		}
		if wx, wy := wholeWords(x.NameCore, words), wholeWords(y.NameCore, words); wx != wy {
			return wx > wy
		}
		if px, py := strings.HasPrefix(x.NameCore, core), strings.HasPrefix(y.NameCore, core); px != py {
			return px
		}
		if len(x.NameCore) != len(y.NameCore) {
			return len(x.NameCore) < len(y.NameCore)
		}
		if x.NameCore != y.NameCore {
			return x.NameCore < y.NameCore
		}
		return x.SKey < y.SKey
	})
	out := []Hit{}
	seen := map[string]bool{}
	for _, h := range hits {
		if seen[h.SKey] {
			continue
		}
		seen[h.SKey] = true
		out = append(out, h)
		if len(out) == limit {
			break
		}
	}
	return out
}
