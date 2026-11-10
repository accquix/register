package search

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Fuzzy corrects unknown query words against the word dictionary and runs the word step with the
// corrections (D18). Words that are a known word prefix stay as typed. It never runs on its own:
// callers invoke it only for mode=fuzzy.
func (s *Searcher) Fuzzy(ctx context.Context, core string, limit int) (Result, error) {
	var res Result
	words := Words(core)
	if len(words) == 0 {
		return res, nil
	}
	conds := make([]cond, 0, len(words))
	corrected := false
	for i, w := range words {
		n, err := s.rangeSize(ctx, w)
		if err != nil {
			return res, err
		}
		if n > 0 {
			conds = append(conds, cond{prefix: w, size: n})
			continue
		}
		to, d, err := s.corrections(ctx, w, i == len(words)-1)
		if err != nil {
			return res, err
		}
		if len(to) == 0 {
			res.Steps = "fuzzy:no_correction"
			return res, nil
		}
		if err := s.pool.QueryRow(ctx, `SELECT coalesce(sum(freq), 0)::int FROM search_word WHERE word = ANY($1)`, to).Scan(&n); err != nil {
			return res, err
		}
		conds = append(conds, cond{exact: to, size: n})
		res.Fixes = append(res.Fixes, fmt.Sprintf("%s→%s", w, strings.Join(to, "|")))
		corrected = true
		_ = d
	}
	if !corrected {
		// nothing to correct: the default search already answers this query
		r, err := s.Default(ctx, core, limit)
		r.Steps = "fuzzy:no_typo;" + r.Steps
		return r, err
	}
	hits, capped, err := s.wordCandidates(ctx, conds)
	if err != nil {
		return res, err
	}
	res.Steps = fmt.Sprintf("fuzzy:words(%d", len(hits))
	if capped {
		res.Steps += ",capped"
	}
	res.Steps += ")"
	// typos per hit: corrected query words whose correction is not literally in the name count 1
	for i := range hits {
		padded := " " + hits[i].NameCore + " "
		for _, c := range conds {
			if c.exact == nil {
				continue
			}
			found := false
			for _, e := range c.exact {
				if strings.Contains(padded, " "+e+" ") {
					found = true
					break
				}
			}
			if !found {
				hits[i].Typos++
			}
		}
	}
	res.Hits = rank(core, words, hits, limit)
	return res, nil
}

// maxTypos allows 1 edit in words of up to 4 letters, 2 above.
func maxTypos(w string) int {
	if len([]rune(w)) <= 4 {
		return 1
	}
	return 2
}

// corrections returns the dictionary words at the smallest edit distance from w (within
// maxTypos), most frequent first, at most 8.
func (s *Searcher) corrections(ctx context.Context, w string, last bool) ([]string, int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL pg_trgm.similarity_threshold = 0.3"); err != nil {
		return nil, 0, err
	}
	rows, err := tx.Query(ctx, `SELECT word, freq FROM search_word WHERE word % $1
ORDER BY similarity(word, $1) DESC, freq DESC LIMIT 60`, w)
	if err != nil {
		return nil, 0, err
	}
	type cand struct {
		word string
		freq int
		d    int
	}
	cands, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (cand, error) {
		var c cand
		err := r.Scan(&c.word, &c.freq)
		return c, err
	})
	if err != nil {
		return nil, 0, err
	}
	wr := []rune(w)
	best := maxTypos(w) + 1
	for i := range cands {
		r := []rune(cands[i].word)
		cands[i].d = OSA(wr, r)
		if last && len(r) > len(wr) { // the word being typed may be a prefix: one extra point
			if d := OSA(wr, r[:len(wr)]) + 1; d < cands[i].d {
				cands[i].d = d
			}
		}
		if cands[i].d < best {
			best = cands[i].d
		}
	}
	if best > maxTypos(w) {
		return nil, 0, nil
	}
	sort.SliceStable(cands, func(a, b int) bool { return cands[a].freq > cands[b].freq })
	var out []string
	for _, c := range cands {
		if c.d == best && len(out) < 8 {
			out = append(out, c.word)
		}
	}
	return out, best, nil
}

// OSA is the optimal string alignment distance: Levenshtein plus adjacent transposition.
func OSA(a, b []rune) int {
	prev2 := make([]int, len(b)+1)
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				cur[j] = min(cur[j], prev2[j-2]+1)
			}
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return prev[len(b)]
}
