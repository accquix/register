// Package normalize holds the pure normalisation rules shared by ingest and search.
// Nothing here touches the network or the database.
package normalize

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Fold returns the search form of s: Unicode NFD, combining marks removed, lower case, and every
// run of characters that are neither letters nor digits collapsed to one space.
// "Slovenská sporiteľňa, a.s." becomes "slovenska sporitelna a s".
func Fold(s string) string {
	// Bytes that are not UTF-8 count as separators. Left in, they stop the decomposition of the
	// letter after them, and the same name would fold differently on a second pass.
	d := norm.NFD.String(strings.ToValidUTF8(s, " "))
	var b strings.Builder
	b.Grow(len(d))
	space := true // suppress leading spaces
	for _, r := range d {
		switch {
		case unicode.Is(unicode.Mn, r):
			continue
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			space = false
		default:
			if !space {
				b.WriteByte(' ')
				space = true
			}
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// legalSuffixes are folded token sequences that mark a legal form or a status at the end of a
// name. Longer sequences come first so that "spol s r o" wins over "s r o".
var legalSuffixes = [][]string{
	{"spol", "s", "r", "o"},
	{"spol", "sro"},
	{"s", "r", "o"},
	{"sro"},
	{"a", "s"},
	{"k", "s"},
	{"v", "o", "s"},
	{"s", "p"},
	{"j", "s", "a"},
	{"n", "o"},
	{"o", "z"},
	{"v", "likvidacii"},
	{"v", "konkurze"},
	{"v", "restrukturalizacii"},
	{"organizacna", "zlozka"},
}

// SplitName folds name and splits off trailing legal-form and status suffixes.
// It returns the core used for search and the removed suffix (folded, space separated).
// If the whole name is a suffix, the core is the whole folded name and the suffix is empty.
func SplitName(name string) (core, suffix string) {
	tokens := strings.Fields(Fold(name))
	end := len(tokens)
	for end > 0 {
		matched := false
		for _, sfx := range legalSuffixes {
			n := len(sfx)
			if n >= end { // never strip the whole name
				continue
			}
			if equalTokens(tokens[end-n:end], sfx) {
				end -= n
				matched = true
				break
			}
		}
		if !matched {
			break
		}
	}
	return strings.Join(tokens[:end], " "), strings.Join(tokens[end:], " ")
}

func equalTokens(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Words returns the distinct words of a folded name that are at least minLen characters long,
// in their first-seen order. These are the search tokens.
func Words(core string, minLen int) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range strings.Fields(core) {
		if len([]rune(w)) >= minLen && !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}
