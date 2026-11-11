package normalize

import (
	"testing"
)

// All names and identifiers below are fictional. The IČOs pass the check digit but were verified
// not to exist in the registers at the time of writing.

func TestFold(t *testing.T) {
	cases := map[string]string{
		"Zlatá Rybka, s.r.o.":          "zlata rybka s r o",
		"  ŠIDIELKO   Ľúbica  ":        "sidielko lubica",
		"Pekáreň \"U Kvietka\" – a.s.": "pekaren u kvietka a s",
		"ÄÔŔŤ-ÝŽ":                      "aort yz",
		"Firma 311":                    "firma 311",
		"":                             "",
		"...":                          "",
	}
	for in, want := range cases {
		if got := Fold(in); got != want {
			t.Errorf("Fold(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFoldIdempotent(t *testing.T) {
	for _, s := range []string{"Zlatá Rybka, s.r.o.", "ĽUBICA – spol. s r. o.", "a  b\tc"} {
		once := Fold(s)
		if twice := Fold(once); twice != once {
			t.Errorf("Fold not idempotent for %q: %q then %q", s, once, twice)
		}
	}
}

func TestSplitName(t *testing.T) {
	cases := []struct{ in, core, suffix string }{
		{"Zlatá Rybka, s.r.o.", "zlata rybka", "s r o"},
		{"Zlatá Rybka s. r. o.", "zlata rybka", "s r o"},
		{"Zlatá Rybka sro", "zlata rybka", "sro"},
		{"Modrý Kameň, spol. s r.o.", "modry kamen", "spol s r o"},
		{"Horná Pekáreň, a.s.", "horna pekaren", "a s"},
		{"Lesný Podnik, š.p.", "lesny podnik", "s p"},
		{"Kvetinka, s.r.o. v likvidácii", "kvetinka", "s r o v likvidacii"},
		{"Ján Fiktívny", "jan fiktivny", ""},
		{"s.r.o.", "s r o", ""},                              // the whole name is a suffix: keep it
		{"Agro Družstvo Dolina", "agro druzstvo dolina", ""}, // a legal-form word inside the name stays
		{"Fiktív, organizačná zložka", "fiktiv", "organizacna zlozka"},
	}
	for _, c := range cases {
		core, suffix := SplitName(c.in)
		if core != c.core || suffix != c.suffix {
			t.Errorf("SplitName(%q) = (%q, %q), want (%q, %q)", c.in, core, suffix, c.core, c.suffix)
		}
	}
}

func TestWords(t *testing.T) {
	got := Words("zlata rybka a zlata ryba", 2)
	want := []string{"zlata", "rybka", "ryba"}
	if len(got) != len(want) {
		t.Fatalf("Words = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Words = %v, want %v", got, want)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		in   string
		kind Kind
		out  string
	}{
		{"12345679", KindICO, "12345679"},
		{"123 456 79", KindICO, "12345679"},
		{"2099999999", KindDIC, "2099999999"},
		{"sk2099999999", KindICDPH, "SK2099999999"},
		{"SK 2099 999 999", KindICDPH, "SK2099999999"},
		{"1234567", KindText, "1234567"},           // 7 digits: not an identifier
		{"CZ2099999999", KindText, "CZ2099999999"}, // only SK VAT numbers
		{"zlata rybka", KindText, "zlata rybka"},
	}
	for _, c := range cases {
		k, out := Classify(c.in)
		if k != c.kind || out != c.out {
			t.Errorf("Classify(%q) = (%v, %q), want (%v, %q)", c.in, k, out, c.kind, c.out)
		}
	}
}

func TestICO(t *testing.T) {
	if ico, ok := ICO(" 1234 5679 "); !ok || ico != "12345679" {
		t.Errorf("ICO with spaces = (%q, %v)", ico, ok)
	}
	for _, bad := range []string{"", "1234567", "123456789", "Neuvedené", "1234567a"} {
		if _, ok := ICO(bad); ok {
			t.Errorf("ICO(%q) accepted", bad)
		}
	}
}

func TestICOChecksum(t *testing.T) {
	for _, ok := range []string{"12345679", "98765434", "11111119", "22222227"} {
		if !ICOChecksumValid(ok) {
			t.Errorf("%s should pass the check digit", ok)
		}
	}
	for _, bad := range []string{"12345678", "98765431", "22222224", "1234567", "abcdefgh"} {
		if ICOChecksumValid(bad) {
			t.Errorf("%s should fail the check digit", bad)
		}
	}
}

func TestPostalCode(t *testing.T) {
	cases := map[string]string{"83237": "832 37", "811 06": "811 06", " 81106 ": "811 06", "8110": "8110", "SK-811": "SK-811"}
	for in, want := range cases {
		if got := PostalCode(in); got != want {
			t.Errorf("PostalCode(%q) = %q, want %q", in, got, want)
		}
	}
}

type tv struct{ v, from, to string }

func TestCurrent(t *testing.T) {
	from := func(x tv) string { return x.from }
	to := func(x tv) string { return x.to }
	cases := []struct {
		name  string
		items []tv
		want  string
	}{
		{"open wins over a later closed one", []tv{{"old", "2001-01-01", ""}, {"closed", "2020-01-01", "2021-01-01"}}, "old"},
		{"latest open", []tv{{"a", "2001-01-01", ""}, {"b", "2010-01-01", ""}}, "b"},
		// a terminated subject: every entry closed; the latest validTo wins, not the latest validFrom
		{"all closed: latest validTo", []tv{{"renamed-later", "2015-01-01", "2016-01-01"}, {"last-name", "2010-01-01", "2026-10-15"}}, "last-name"},
		{"all closed, same validTo: latest validFrom", []tv{{"a", "2010-01-01", "2026-10-15"}, {"b", "2012-01-01", "2026-10-15"}}, "b"},
	}
	for _, c := range cases {
		got, ok := Current(c.items, from, to)
		if !ok || got.v != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got.v, c.want)
		}
	}
	if _, ok := Current([]tv{}, from, to); ok {
		t.Error("Current on an empty list should report !ok")
	}
}
