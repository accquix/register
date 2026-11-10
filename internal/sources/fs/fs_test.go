package fs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/accquix/register/internal/sources/fs"
	"github.com/accquix/register/internal/testkit"
)

func write(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "list.zip")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func readAll(t *testing.T, slug string, data []byte) ([]fs.Row, *fs.Decoder, error) {
	t.Helper()
	d, err := fs.Open(slug, write(t, data))
	if err != nil {
		return nil, nil, err
	}
	t.Cleanup(func() { d.Close() })
	var rows []fs.Row
	for d.Next() {
		rows = append(rows, d.Row())
	}
	return rows, d, d.Err()
}

func TestVATList(t *testing.T) {
	items := []testkit.Item{
		testkit.VATItem(1, "99900001", "§4", "01.08.2020", "01.08.2020"),
		testkit.VATItem(2, "99900002", "§7a", "06.09.2023", ""), // registered, not a payer
		testkit.VATItem(3, "123456789012", "§4b", "01.01.2024", "01.01.2024"),
	}
	items[2]["IC_DPH"] = items[0]["IC_DPH"] // a VAT group member shares the IČ DPH; its IČO is not valid
	rows, d, err := readAll(t, "ds_dphs", testkit.FSZip("ds_dphs", "2026-11-10", items))
	if err != nil {
		t.Fatal(err)
	}
	if d.AsOf().Format("2006-01-02") != "2026-11-10" || len(rows) != 3 {
		t.Fatalf("as of %v, %d rows", d.AsOf(), len(rows))
	}
	a, b, c := rows[0], rows[1], rows[2]
	if a.Key != "SK2099900001|99900001" || a.Paragraph != "§4" || a.D1 != "2020-08-01" || a.D3 != "2020-08-01" || a.PostalCode != "999 99" {
		t.Errorf("payer: %+v", a)
	}
	if b.Paragraph != "§7a" || b.D1 != "2023-09-06" || b.D3 != "" {
		t.Errorf("§7a must have a registration date and no payer date: %+v", b)
	}
	if c.ICO != "" || !strings.HasPrefix(c.Key, "SK2099900001|n:") || c.Key == a.Key {
		t.Errorf("group member without a valid IČO is keyed by name: %+v", c)
	}
}

func TestOtherLists(t *testing.T) {
	rows, _, err := readAll(t, "ds_dpho", testkit.FSZip("ds_dpho", "2026-11-10", []testkit.Item{
		{"IC_DPH": "SK2099900005", "NAZOV": "Osobitná Fiktívna", "ICO": "99900005", "DAT_ZACATIA": "01.10.2020", "ADRESA": "1"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Key != "SK2099900005|01.10.2020|" || rows[0].Attrs != `{"DAT_ZACATIA":"01.10.2020"}` || rows[0].Street != "1" {
		t.Errorf("ds_dpho: %+v", rows[0])
	}
	rows, _, err = readAll(t, "ds_iz_ran", testkit.FSZip("ds_iz_ran", "2026-10-31", []testkit.Item{
		{"DIC": "2099900006", "ICO": "99900006", "IDS": "spoľahlivý", "NAZOV_SUBJEKTU": "Spoľahlivá Fiktívna", "STAT": "SK"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Key != "2099900006" || rows[0].Attrs != `{"IDS":"spoľahlivý"}` || rows[0].Name != "Spoľahlivá Fiktívna" {
		t.Errorf("ds_iz_ran: %+v", rows[0])
	}
}

func TestParseErrorsReject(t *testing.T) {
	cases := map[string]struct {
		slug string
		item testkit.Item
	}{
		"missing DIC":    {"ds_dsrdp", testkit.Item{"NAZOV_DS": "x"}},
		"bad IC_DPH":     {"ds_dphs", testkit.Item{"IC_DPH": "SK12", "DRUH_REG_DPH": "§4"}},
		"bad paragraph":  {"ds_dphs", testkit.Item{"IC_DPH": "SK2099900001", "DRUH_REG_DPH": "§99"}},
		"bad date":       {"ds_dphs", testkit.Item{"IC_DPH": "SK2099900001", "DRUH_REG_DPH": "§4", "DATUM_REG": "2020-08-01"}},
		"missing IC_DPH": {"ds_dphz", testkit.Item{"DAT_ZVEREJNENIA": "25.02.2021"}},
		"bad DIC":        {"ds_iz_ran", testkit.Item{"DIC": "12"}},
	}
	for name, c := range cases {
		if _, _, err := readAll(t, c.slug, testkit.FSZip(c.slug, "2026-11-10", []testkit.Item{c.item})); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := fs.Open("ds_dphs", write(t, []byte("not a zip"))); err == nil {
		t.Error("not a zip: want an error")
	}
}

func TestConditionalFetch(t *testing.T) {
	srv := testkit.NewFSServer(t)
	srv.Put("ds_dphs", testkit.FSZip("ds_dphs", "2026-11-10", nil))
	c := fs.NewClient(srv.URL())
	dir := t.TempDir()
	f, err := c.Fetch(context.Background(), "ds_dphs", "", dir)
	if err != nil || f.NotModified || f.ETag == "" || len(f.SHA256) != 64 {
		t.Fatalf("first fetch: %+v %v", f, err)
	}
	g, err := c.Fetch(context.Background(), "ds_dphs", f.ETag, dir)
	if err != nil || !g.NotModified {
		t.Fatalf("second fetch must be 304: %+v %v", g, err)
	}
	if _, err := c.Fetch(context.Background(), "ds_missing", "", dir); err == nil {
		t.Fatal("want an error for a missing list")
	}
}
