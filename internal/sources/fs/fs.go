// Package fs reads the Financial Administration (Finančná správa SR) list exports: daily ZIP files
// at report.financnasprava.sk/{slug}.zip, each holding one XML document (plus an XSD). The XML is
// streamed item by item. Downloads use a conditional GET with the stored ETag.
package fs

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/accquix/register/internal/normalize"
)

// DefaultBaseURL serves the list exports. No API key is needed.
const DefaultBaseURL = "https://report.financnasprava.sk"

// Target is the table a list is merged into.
type Target int

const (
	TaxSubjects Target = iota // ds_dsrdp → fs_tax_subject
	VATPayers                 // ds_dphs  → fs_vat_payer
	ListEntries               // ds_dphz, ds_dphv, ds_dpho, ds_iz_ran → fs_list_entry
)

// List describes one export.
type List struct {
	Slug    string
	Target  Target
	MaxDrop float64 // a new version may have at most this share fewer rows than the active ones (D23)
}

// Lists are the six exports the register mirrors. The IBAN lists (ds_dph_iban, ds_dph_oud) are
// CC BY-NC-ND and are never ingested.
var Lists = []List{
	{"ds_dphs", VATPayers, 0.03},
	{"ds_dsrdp", TaxSubjects, 0.03},
	{"ds_iz_ran", ListEntries, 0.03},
	{"ds_dphz", ListEntries, 0.20},
	{"ds_dphv", ListEntries, 0.20},
	{"ds_dpho", ListEntries, 0.20},
}

// ListBySlug finds a list definition.
func ListBySlug(slug string) (List, bool) {
	for _, l := range Lists {
		if l.Slug == slug {
			return l, true
		}
	}
	return List{}, false
}

// Row is one normalised list item, shaped like the fs_stage table.
type Row struct {
	Key          string
	ICDPH        string
	DIC          string
	ICO          string // "" unless a valid 8-digit IČO
	Name         string
	NameCore     string
	Municipality string
	PostalCode   string
	Street       string
	Country      string
	Paragraph    string // ds_dphs: §4, §4b, §5, §7, §7a
	D1, D2, D3   string // ds_dphs: registered_on, type_changed_on, payer_from (YYYY-MM-DD or "")
	Attrs        string // JSON object of list-specific attributes (other lists)
	Hash         []byte
}

// identity and address fields; everything else of a list item goes into Attrs.
var baseFields = map[string]bool{"IC_DPH": true, "DIC": true, "ICO": true, "NAZOV": true, "NAZOV_DS": true, "NAZOV_SUBJEKTU": true,
	"OBEC": true, "PSC": true, "ADRESA": true, "ULICA_CISLO": true, "STAT": true, "NAZOV_STATU": true}

func skDate(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	t, err := time.Parse("02.01.2006", s)
	if err != nil {
		return "", fmt.Errorf("date %q", s)
	}
	return t.Format("2006-01-02"), nil
}

func first(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := m[k]; v != "" {
			return v
		}
	}
	return ""
}

func isDigits(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// mapRow turns one XML item into a Row. An item without its key is an error (D23: 0 parse errors).
func mapRow(slug string, m map[string]string) (Row, error) {
	r := Row{
		ICDPH:        strings.ToUpper(strings.TrimSpace(m["IC_DPH"])),
		DIC:          strings.TrimSpace(m["DIC"]),
		Name:         first(m, "NAZOV_DS", "NAZOV", "NAZOV_SUBJEKTU"),
		Municipality: m["OBEC"],
		PostalCode:   normalize.PostalCode(m["PSC"]),
		Street:       first(m, "ULICA_CISLO", "ADRESA"),
		Country:      first(m, "STAT", "NAZOV_STATU"),
	}
	if ico, ok := normalize.ICO(m["ICO"]); ok {
		r.ICO = ico
	}
	r.NameCore, _ = normalize.SplitName(r.Name)
	if r.ICDPH != "" && (!strings.HasPrefix(r.ICDPH, "SK") || !isDigits(r.ICDPH[2:], 10)) {
		return r, fmt.Errorf("IC_DPH %q", r.ICDPH)
	}
	if r.DIC != "" && !isDigits(r.DIC, 10) {
		return r, fmt.Errorf("DIC %q", r.DIC)
	}
	var err error
	switch slug {
	case "ds_dsrdp":
		r.Key = r.DIC
	case "ds_dphs":
		if r.ICDPH == "" {
			return r, errors.New("missing IC_DPH")
		}
		r.Paragraph = strings.TrimSpace(m["DRUH_REG_DPH"])
		switch r.Paragraph {
		case "§4", "§4b", "§5", "§7", "§7a":
		default:
			return r, fmt.Errorf("DRUH_REG_DPH %q", r.Paragraph)
		}
		// VAT groups share an IČ DPH: key by IČ DPH plus the member's IČO, else its name.
		if r.ICO != "" {
			r.Key = r.ICDPH + "|" + r.ICO
		} else {
			r.Key = r.ICDPH + "|n:" + r.NameCore
		}
		if r.D1, err = skDate(m["DATUM_REG"]); err != nil {
			return r, err
		}
		if r.D2, err = skDate(m["DATUM_ZMENY_DRUHU_REG"]); err != nil {
			return r, err
		}
		if r.D3, err = skDate(m["PLAT_DPH_OD"]); err != nil {
			return r, err
		}
	case "ds_iz_ran":
		r.Key = r.DIC
	case "ds_dphz":
		r.Key = r.ICDPH + "|" + m["DAT_ZVEREJNENIA"]
	case "ds_dphv":
		r.Key = r.ICDPH + "|" + m["DAT_ZVEREJNENIA"] + "|" + m["DAT_VYMAZU"]
	case "ds_dpho":
		// one subject can have several periods with the same start date (real data, 2026-11):
		// the end date is part of the key
		r.Key = r.ICDPH + "|" + m["DAT_ZACATIA"] + "|" + m["DAT_SKONCENIA"]
	default:
		return r, fmt.Errorf("unknown list %s", slug)
	}
	if strings.Trim(r.Key, "|") == "" || strings.HasPrefix(r.Key, "|") {
		return r, errors.New("missing key")
	}
	if slug != "ds_dsrdp" && slug != "ds_dphs" {
		attrs := map[string]string{}
		for k, v := range m {
			if !baseFields[k] && strings.TrimSpace(v) != "" {
				attrs[k] = strings.TrimSpace(v)
			}
		}
		b, _ := json.Marshal(attrs) // map keys are sorted: deterministic
		r.Attrs = string(b)
	}
	h := sha256.New()
	for _, f := range []string{r.ICDPH, r.DIC, r.ICO, r.Name, r.Municipality, r.PostalCode, r.Street, r.Country, r.Paragraph, r.D1, r.D2, r.D3, r.Attrs} {
		h.Write([]byte(f))
		h.Write([]byte{0x1f})
	}
	r.Hash = h.Sum(nil)
	return r, nil
}

// Decoder streams the items of one export.
type Decoder struct {
	slug   string
	dec    *xml.Decoder
	asOf   time.Time
	row    Row
	n      int
	err    error
	closer io.Closer
}

// Open reads the XML entry of the ZIP at path.
func Open(slug, path string) (*Decoder, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("zip: %w", err)
	}
	var entry *zip.File
	for _, f := range zr.File {
		if strings.HasSuffix(strings.ToLower(f.Name), ".xml") {
			if entry != nil {
				_ = zr.Close()
				return nil, errors.New("zip: more than one XML entry")
			}
			entry = f
		}
	}
	if entry == nil {
		_ = zr.Close()
		return nil, errors.New("zip: no XML entry")
	}
	rc, err := entry.Open()
	if err != nil {
		_ = zr.Close()
		return nil, err
	}
	return &Decoder{slug: slug, dec: xml.NewDecoder(rc), closer: multiCloser{rc, zr}}, nil
}

type multiCloser []io.Closer

func (m multiCloser) Close() error {
	var errs []error
	for _, c := range m {
		errs = append(errs, c.Close())
	}
	return errors.Join(errs...)
}

// Close releases the file.
func (d *Decoder) Close() error { return d.closer.Close() }

// AsOf is the list date (DatumAktualizacieZoznamu); zero until the first item has been read.
func (d *Decoder) AsOf() time.Time { return d.asOf }

func (d *Decoder) text() (string, error) {
	var b strings.Builder
	for {
		t, err := d.dec.Token()
		if err != nil {
			return "", err
		}
		switch v := t.(type) {
		case xml.CharData:
			b.Write(v)
		case xml.StartElement:
			if err := d.dec.Skip(); err != nil {
				return "", err
			}
		case xml.EndElement:
			return b.String(), nil
		}
	}
}

// Next reads the next item. It returns false at the end or on an error (see Err).
func (d *Decoder) Next() bool {
	if d.err != nil {
		return false
	}
	for {
		t, err := d.dec.Token()
		if errors.Is(err, io.EOF) {
			if d.asOf.IsZero() {
				d.err = errors.New("no DatumAktualizacieZoznamu in the list")
			}
			return false
		}
		if err != nil {
			d.err = fmt.Errorf("xml: %w", err)
			return false
		}
		se, ok := t.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "DatumAktualizacieZoznamu":
			s, err := d.text()
			if err != nil {
				d.err = err
				return false
			}
			if d.asOf, err = time.Parse("02012006", strings.TrimSpace(s)); err != nil {
				d.err = fmt.Errorf("list date %q", s)
				return false
			}
		case "ITEM":
			if d.asOf.IsZero() {
				d.err = errors.New("ITEM before DatumAktualizacieZoznamu")
				return false
			}
			m := map[string]string{}
			for {
				t, err := d.dec.Token()
				if err != nil {
					d.err = fmt.Errorf("item %d: %w", d.n+1, err)
					return false
				}
				if f, ok := t.(xml.StartElement); ok {
					v, err := d.text()
					if err != nil {
						d.err = fmt.Errorf("item %d: %w", d.n+1, err)
						return false
					}
					m[f.Name.Local] = strings.TrimSpace(v)
					continue
				}
				if _, ok := t.(xml.EndElement); ok {
					break
				}
			}
			r, err := mapRow(d.slug, m)
			if err != nil {
				d.err = fmt.Errorf("item %d: %w", d.n+1, err)
				return false
			}
			d.row = r
			d.n++
			return true
		}
	}
}

// Row returns the current item.
func (d *Decoder) Row() Row { return d.row }

// Count is the number of items read.
func (d *Decoder) Count() int { return d.n }

// Err returns the first error.
func (d *Decoder) Err() error { return d.err }

// Client downloads the exports.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient returns a client for baseURL.
func NewClient(baseURL string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: &http.Client{Timeout: 15 * time.Minute}}
}

// Fetched is the result of a conditional download.
type Fetched struct {
	NotModified bool
	Path        string // local ZIP when modified
	ETag        string
	SHA256      string
}

// Fetch downloads slug.zip into dir unless the server answers 304 for etag.
func (c *Client) Fetch(ctx context.Context, slug, etag, dir string) (Fetched, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/"+slug+".zip", nil)
	if err != nil {
		return Fetched{}, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Fetched{}, fmt.Errorf("%s: %w", slug, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return Fetched{NotModified: true, ETag: etag}, nil
	case http.StatusOK:
	default:
		return Fetched{}, fmt.Errorf("%s: HTTP %d", slug, resp.StatusCode)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Fetched{}, err
	}
	path := filepath.Join(dir, slug+".zip")
	f, err := os.Create(path)
	if err != nil {
		return Fetched{}, err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		return Fetched{}, fmt.Errorf("%s: %w", slug, copyErr)
	}
	if closeErr != nil {
		return Fetched{}, closeErr
	}
	if resp.ContentLength > 0 && n != resp.ContentLength {
		return Fetched{}, fmt.Errorf("%s: got %d bytes, want %d", slug, n, resp.ContentLength)
	}
	return Fetched{Path: path, ETag: resp.Header.Get("ETag"), SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// Slugs returns the list slugs in a stable order.
func Slugs() []string {
	out := make([]string, len(Lists))
	for i, l := range Lists {
		out[i] = l.Slug
	}
	sort.Strings(out)
	return out
}
