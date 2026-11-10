package normalize

import "strings"

// Kind is the class of a search query or lookup identifier.
type Kind int

const (
	KindText  Kind = iota // a name or anything else
	KindICO               // 8 digits
	KindDIC               // 10 digits
	KindICDPH             // "SK" + 10 digits
)

func (k Kind) String() string {
	switch k {
	case KindICO:
		return "ico"
	case KindDIC:
		return "dic"
	case KindICDPH:
		return "ic_dph"
	default:
		return "text"
	}
}

// compact removes spaces and upper-cases, so "sk 2020 411 536" becomes "SK2020411536".
func compact(s string) string {
	return strings.ToUpper(strings.Join(strings.Fields(s), ""))
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Classify decides whether q is an identifier and returns its canonical form.
// Identifiers are always matched exactly (D20); anything else is KindText with q unchanged.
func Classify(q string) (Kind, string) {
	c := compact(q)
	switch {
	case len(c) == 8 && allDigits(c):
		return KindICO, c
	case len(c) == 10 && allDigits(c):
		return KindDIC, c
	case len(c) == 12 && strings.HasPrefix(c, "SK") && allDigits(c[2:]):
		return KindICDPH, c
	default:
		return KindText, q
	}
}

// ICO returns the canonical IČO and true when raw is exactly 8 digits (spaces ignored).
// Anything else, such as the text placeholder some RPO records carry, means "no IČO".
func ICO(raw string) (string, bool) {
	c := compact(raw)
	if len(c) == 8 && allDigits(c) {
		return c, true
	}
	return "", false
}

// ICOChecksumValid reports whether the 8th digit matches the mod-11 check digit
// (weights 8..2 over the first 7 digits). It is a warning only (D20): real RPO IČOs exist that
// fail it, so a failing IČO is never rejected.
func ICOChecksumValid(ico string) bool {
	if len(ico) != 8 || !allDigits(ico) {
		return false
	}
	sum := 0
	for i := 0; i < 7; i++ {
		sum += int(ico[i]-'0') * (8 - i)
	}
	check := (11 - sum%11) % 10
	return int(ico[7]-'0') == check
}

// PostalCode normalises a Slovak PSČ to "NNN NN". Input that is not 5 digits (spaces ignored)
// is returned trimmed and otherwise unchanged.
func PostalCode(s string) string {
	c := strings.Join(strings.Fields(s), "")
	if len(c) == 5 && allDigits(c) {
		return c[:3] + " " + c[3:]
	}
	return strings.TrimSpace(s)
}
