package normalize

import (
	"strings"
	"testing"
	"unicode"
)

// FuzzFold: the search form of any text is letters, digits and single spaces, and folding it
// again changes nothing (ingest and search must agree on it).
func FuzzFold(f *testing.F) {
	for _, s := range []string{"Slovenská sporiteľňa, a.s.", "  Ján   Fiktívny - Kaderníctvo ", "İstanbul ǅ ß ﬁ", "á̂b", "", "\x00\xff", "ΑΣ ς"} {
		f.Add(s)
	}
	f.Add("\xf4ĥ") // found by this fuzz test: a byte that is not UTF-8 stopped the decomposition after it
	f.Fuzz(func(t *testing.T, s string) {
		out := Fold(s)
		if again := Fold(out); again != out {
			t.Fatalf("Fold is not idempotent: %q -> %q -> %q", s, out, again)
		}
		if out != strings.TrimSpace(out) || strings.Contains(out, "  ") {
			t.Fatalf("spaces in %q", out)
		}
		for _, r := range out {
			if r != ' ' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
				t.Fatalf("%q holds %q", out, r)
			}
		}
	})
}

// FuzzSplitName: the core and the suffix together are the folded name, and the core is empty
// only when the name is.
func FuzzSplitName(f *testing.F) {
	for _, s := range []string{"Alfa Stavby, s.r.o.", "s.r.o.", "A s", "spol. s r.o. v likvidácii", "a.s. a.s. a.s.", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		core, suffix := SplitName(s)
		folded := Fold(s)
		if got := strings.TrimSpace(core + " " + suffix); got != folded {
			t.Fatalf("%q: core %q + suffix %q is not %q", s, core, suffix, folded)
		}
		if (core == "") != (folded == "") {
			t.Fatalf("%q: core %q for %q", s, core, folded)
		}
		for _, w := range Words(core, 2) {
			if len([]rune(w)) < 2 || strings.ContainsRune(w, ' ') {
				t.Fatalf("word %q", w)
			}
		}
	})
}

// FuzzClassify: an identifier is recognised only in its exact shape (D20), and anything else is
// text, unchanged.
func FuzzClassify(f *testing.F) {
	for _, s := range []string{"99900001", "2099900001", "sk 2099 900 001", "SK2099900001", " 9990 0001 ", "９９９０0001", "SK209990000١", "tatra", ""} {
		f.Add(s)
	}
	digits := func(s string) bool {
		for _, r := range s {
			if r < '0' || r > '9' {
				return false
			}
		}
		return s != ""
	}
	f.Fuzz(func(t *testing.T, s string) {
		kind, id := Classify(s)
		switch kind {
		case KindICO:
			if len(id) != 8 || !digits(id) {
				t.Fatalf("%q -> IČO %q", s, id)
			}
		case KindDIC:
			if len(id) != 10 || !digits(id) {
				t.Fatalf("%q -> DIČ %q", s, id)
			}
		case KindICDPH:
			if len(id) != 12 || id[:2] != "SK" || !digits(id[2:]) {
				t.Fatalf("%q -> IČ DPH %q", s, id)
			}
		default:
			if id != s {
				t.Fatalf("text must come back unchanged: %q -> %q", s, id)
			}
		}
		if ico, ok := ICO(s); ok != (kind == KindICO) || (ok && ico != id) {
			t.Fatalf("ICO(%q) = %q %v, Classify says %v %q", s, ico, ok, kind, id)
		}
		_ = ICOChecksumValid(s)
		if p := PostalCode(s); p != strings.TrimSpace(p) {
			t.Fatalf("postal code %q", p)
		}
	})
}
