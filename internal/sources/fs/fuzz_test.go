package fs_test

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/accquix/register/internal/sources/fs"
)

var (
	reICDPH = regexp.MustCompile(`^SK[0-9]{10}$`)
	reDIC   = regexp.MustCompile(`^[0-9]{10}$`)
	reICO   = regexp.MustCompile(`^[0-9]{8}$`)
	reDay   = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
)

var fuzzLimits = fs.Limits{XML: 1 << 20, Element: 4 << 10, Fields: 32, Items: 500}

const list = `<?xml version="1.0" encoding="UTF-8"?><Z><DatumAktualizacieZoznamu>10112026</DatumAktualizacieZoznamu><L>
<ITEM><IC_DPH>SK2099900001</IC_DPH><ICO>99900001</ICO><NAZOV_DS>Platiteľ Fiktívny</NAZOV_DS><DRUH_REG_DPH>§4</DRUH_REG_DPH><DATUM_REG>01.08.2020</DATUM_REG><PLAT_DPH_OD>01.08.2020</PLAT_DPH_OD></ITEM>
<ITEM><DIC>2099900002</DIC><NAZOV_DS>Daňovník &amp; Syn</NAZOV_DS><OBEC>Fiktívne Mesto</OBEC><PSC>99999</PSC></ITEM>
<ITEM><IC_DPH>SK2099900003</IC_DPH><DAT_ZVEREJNENIA>25.02.2021</DAT_ZVEREJNENIA><DAT_ZACATIA>01.10.2020</DAT_ZACATIA><IDS>spoľahlivý</IDS></ITEM>
</L></Z>`

// FuzzDecoder feeds arbitrary XML to the decoder of every list: it must never panic, never return
// a row that breaks the row invariants, and never read past its limits.
func FuzzDecoder(f *testing.F) {
	for i := range fs.Lists {
		f.Add(uint8(i), []byte(list))
	}
	f.Add(uint8(0), []byte(`<Z><ITEM><IC_DPH>SK2099900001</IC_DPH></ITEM></Z>`))                                  // an item before the date
	f.Add(uint8(1), []byte(`<Z><DatumAktualizacieZoznamu>31022026</DatumAktualizacieZoznamu></Z>`))               // an impossible date
	f.Add(uint8(1), []byte(`<Z><DatumAktualizacieZoznamu>10112026</DatumAktualizacieZoznamu><ITEM><DIC>1</DIC>`)) // truncated
	f.Add(uint8(0), []byte(`<!DOCTYPE z [<!ENTITY a "aaaaaaaaaa"><!ENTITY b "&a;&a;&a;&a;&a;&a;&a;&a;">]><Z>&b;</Z>`))
	f.Add(uint8(2), []byte("<Z><DatumAktualizacieZoznamu>10112026</DatumAktualizacieZoznamu><ITEM><DIC>2099900002</DIC><X><Y><ITEM/></Y></X></ITEM></Z>"))
	f.Fuzz(func(t *testing.T, which uint8, raw []byte) {
		l := fs.Lists[int(which)%len(fs.Lists)]
		d := fs.NewDecoder(l.Slug, bytes.NewReader(raw), fuzzLimits)
		for d.Next() {
			r := d.Row()
			if r.Key == "" || strings.HasPrefix(r.Key, "|") {
				t.Fatalf("a row without a key: %q", r.Key)
			}
			if r.ICDPH != "" && !reICDPH.MatchString(r.ICDPH) {
				t.Fatalf("IČ DPH %q", r.ICDPH)
			}
			if r.DIC != "" && !reDIC.MatchString(r.DIC) {
				t.Fatalf("DIČ %q", r.DIC)
			}
			if r.ICO != "" && !reICO.MatchString(r.ICO) {
				t.Fatalf("IČO %q", r.ICO)
			}
			for _, day := range []string{r.D1, r.D2, r.D3} {
				if day != "" && !reDay.MatchString(day) {
					t.Fatalf("date %q", day)
				}
			}
			if len(r.Hash) != 32 {
				t.Fatalf("hash of %d bytes", len(r.Hash))
			}
			if d.AsOf().IsZero() {
				t.Fatal("a row before the list date")
			}
			if d.Count() > fuzzLimits.Items {
				t.Fatalf("%d items passed a limit of %d", d.Count(), fuzzLimits.Items)
			}
		}
	})
}

// The limits end an export as an error the list can be rejected with.
func TestDecoderLimits(t *testing.T) {
	item := func(extra string) string {
		return `<ITEM><DIC>2099900002</DIC><NAZOV_DS>Daňovník</NAZOV_DS>` + extra + `</ITEM>`
	}
	doc := func(items ...string) []byte {
		return []byte(`<Z><DatumAktualizacieZoznamu>10112026</DatumAktualizacieZoznamu>` + strings.Join(items, "") + `</Z>`)
	}
	read := func(raw []byte, lim fs.Limits) (int, error) {
		d := fs.NewDecoder("ds_dsrdp", bytes.NewReader(raw), lim)
		for d.Next() {
		}
		return d.Count(), d.Err()
	}
	many := make([]string, 30)
	for i := range many {
		many[i] = strings.Replace(item(""), "2099900002", "20999"+strings.Repeat("0", 3)+string(rune('0'+i/10))+string(rune('0'+i%10)), 1)
	}
	if n, err := read(doc(many...), fs.Limits{}); err != nil || n != 30 {
		t.Fatalf("default limits: %d %v", n, err)
	}
	for name, c := range map[string]struct {
		raw []byte
		lim fs.Limits
	}{
		"items":   {doc(many...), fs.Limits{Items: 10}},
		"element": {doc(item("<OBEC>" + strings.Repeat("x", 5000) + "</OBEC>")), fs.Limits{Element: 1000}},
		"fields":  {doc(item("<A>1</A><B>2</B><C>3</C><D>4</D>")), fs.Limits{Fields: 4}},
		"xml":     {doc(many...), fs.Limits{XML: 500}},
	} {
		if _, err := read(c.raw, c.lim); !errors.Is(err, fs.ErrTooLarge) {
			t.Errorf("%s limit: %v", name, err)
		}
	}
	// a ZIP with more entries than a real export can have
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i <= fs.MaxZipEntries; i++ {
		w, _ := zw.Create("f" + string(rune('a'+i)) + ".xml")
		_, _ = w.Write([]byte("<Z/>"))
	}
	_ = zw.Close()
	path := filepath.Join(t.TempDir(), "ds_dphs.zip")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Open("ds_dphs", path); !errors.Is(err, fs.ErrTooLarge) {
		t.Fatalf("zip entries limit: %v", err)
	}
}
