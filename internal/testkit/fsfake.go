package testkit

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Item is one synthetic list item: element name → value.
type Item map[string]string

// FSZip renders a list export in the real layout: a ZIP with <slug>.xml and an .xsd; the XML has
// DatumAktualizacieZoznamu (ddmmyyyy) and ITEM elements. date is "YYYY-MM-DD".
func FSZip(slug, date string, items []Item) []byte {
	var x strings.Builder
	x.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<ZoznamTest>\n")
	if slug == "ds_iz_ran" {
		x.WriteString("  <Licencia>CC0</Licencia>\n")
	}
	fmt.Fprintf(&x, "  <DatumAktualizacieZoznamu>%s%s%s</DatumAktualizacieZoznamu>\n  <%s>\n", date[8:10], date[5:7], date[0:4], strings.ToUpper(slug))
	for _, it := range items {
		x.WriteString("    <ITEM>\n")
		keys := make([]string, 0, len(it))
		for k := range it {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			var esc bytes.Buffer
			_ = xmlEscape(&esc, it[k])
			fmt.Fprintf(&x, "      <%s>%s</%s>\n", k, esc.String(), k)
		}
		x.WriteString("    </ITEM>\n")
	}
	fmt.Fprintf(&x, "  </%s>\n</ZoznamTest>\n", strings.ToUpper(slug))
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create(slug + ".xml")
	w.Write([]byte(x.String()))
	w, _ = zw.Create(slug + ".xsd")
	w.Write([]byte("<xs:schema/>"))
	zw.Close()
	return buf.Bytes()
}

func xmlEscape(b *bytes.Buffer, s string) error {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	_, err := b.WriteString(r.Replace(s))
	return err
}

// VATItem is a ds_dphs item. payerFrom "" leaves PLAT_DPH_OD empty (as for §7/§7a).
func VATItem(n int, ico, paragraph, registered, payerFrom string) Item {
	it := Item{"IC_DPH": fmt.Sprintf("SK20999%05d", n), "NAZOV_DS": fmt.Sprintf("Platiteľ Fiktívny %d", n), "OBEC": "Fiktívne Mesto",
		"PSC": "99999", "ULICA_CISLO": "Testovacia 1", "STAT": "Slovensko", "DRUH_REG_DPH": paragraph, "DATUM_REG": registered}
	if ico != "" {
		it["ICO"] = ico
	}
	if payerFrom != "" {
		it["PLAT_DPH_OD"] = payerFrom
	}
	return it
}

// TaxItem is a ds_dsrdp item (DIČ 20999nnnnn).
func TaxItem(n int, ico string) Item {
	it := Item{"DIC": fmt.Sprintf("20999%05d", n), "NAZOV_DS": fmt.Sprintf("Daňovník Fiktívny %d", n), "OBEC": "Fiktívne Mesto",
		"PSC": "99999", "ULICA_CISLO": "Testovacia 2", "NAZOV_STATU": "Slovensko"}
	if ico != "" {
		it["ICO"] = ico
	}
	return it
}

// FSServer is a fake report.financnasprava.sk: GET /<slug>.zip with an ETag and 304 for a match.
type FSServer struct {
	mu     sync.Mutex
	files  map[string][]byte
	Server *httptest.Server
	Gets   map[string]int // full (200) responses per slug
}

// NewFSServer starts the fake; it is closed when the test ends.
func NewFSServer(t *testing.T) *FSServer {
	s := &FSServer{files: map[string][]byte{}, Gets: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Server.Close)
	return s
}

// URL is the base URL.
func (s *FSServer) URL() string { return s.Server.URL }

// Put publishes a list version.
func (s *FSServer) Put(slug string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[slug] = data
}

func etagOf(b []byte) string {
	h := sha256.Sum256(b)
	return `"` + hex.EncodeToString(h[:8]) + `"`
}

func (s *FSServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	slug := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".zip")
	data, ok := s.files[slug]
	if !ok {
		http.NotFound(w, r)
		return
	}
	et := etagOf(data)
	w.Header().Set("ETag", et)
	if r.Header.Get("If-None-Match") == et {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	s.Gets[slug]++
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	w.Write(data)
}
