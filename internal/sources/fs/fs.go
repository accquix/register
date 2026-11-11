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
	MinRows int     // a version with fewer distinct rows is rejected, also on the first load; 0 = no floor
}

// Lists are the six exports the register mirrors. The IBAN lists (ds_dph_iban, ds_dph_oud) are
// CC BY-NC-ND and are never ingested.
//
// MinRows is about a quarter to a third of the size measured on 2026-11-10 (docs/sources.md): far
// below any real version, far above a truncated or empty one.
var Lists = []List{
	{"ds_dphs", VATPayers, 0.03, 100_000},
	{"ds_dsrdp", TaxSubjects, 0.03, 500_000},
	{"ds_iz_ran", ListEntries, 0.03, 200_000},
	{"ds_dphz", ListEntries, 0.20, 1_000},
	{"ds_dphv", ListEntries, 0.20, 10_000},
	{"ds_dpho", ListEntries, 0.20, 300},
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

// MaxUnidentified is how many items of one file may be left out before the file is rejected
// (OQ-37, D38). An item is left out, and counted, when it carries no identifier at all (it cannot
// be found by any), or when an item of the DIČ list has a valid IČO but no DIČ yet (a new
// business: the register knows it from RPO, and there is no DIČ to hold). More of them means
// the export itself is broken.
const MaxUnidentified = 100

// errNoIdentifier marks an item to leave out (see MaxUnidentified): fields absent, not
// malformed. A malformed identifier still rejects the file.
var errNoIdentifier = errors.New("no identifier")

// mapRow turns one XML item into a Row. An item without its key is an error (D23: 0 parse
// errors), except an item with no identifier at all (errNoIdentifier, counted by the decoder).
func mapRow(slug string, m map[string]string) (Row, error) {
	if strings.TrimSpace(m["DIC"]) == "" && strings.TrimSpace(m["ICO"]) == "" && strings.TrimSpace(m["IC_DPH"]) == "" {
		return Row{}, errNoIdentifier
	}
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
		if r.DIC == "" && r.ICO != "" { // a valid IČO, no DIČ yet
			return r, errNoIdentifier
		}
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
	lim  Limits
	slug string
	dec  *xml.Decoder
	asOf time.Time
	row  Row
	n    int // items returned
	pos  int // items read, for error messages
	err  error
	// skipped counts items without any identifier (at most MaxUnidentified)
	skipped int
	closer  io.Closer
}

// Open reads the XML entry of the ZIP at path.
func Open(slug, path string) (*Decoder, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("zip: %w", err)
	}
	if len(zr.File) > MaxZipEntries {
		_ = zr.Close()
		return nil, fmt.Errorf("zip: %w: %d entries, the limit is %d", ErrTooLarge, len(zr.File), MaxZipEntries)
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
	if entry.UncompressedSize64 > uint64(DefaultLimits.XML) {
		_ = zr.Close()
		return nil, fmt.Errorf("zip: %w: the XML is %d bytes, the limit is %d", ErrTooLarge, entry.UncompressedSize64, DefaultLimits.XML)
	}
	rc, err := entry.Open()
	if err != nil {
		_ = zr.Close()
		return nil, err
	}
	d := NewDecoder(slug, rc, Limits{})
	d.closer = multiCloser{rc, zr}
	return d, nil
}

// Limits bound what one export may cost before it is rejected: a damaged or hostile file must end
// as a rejected list, never as a full disk or an out-of-memory kill. The defaults leave ample room
// over the real lists (docs/sources.md: the largest ZIP is 57 MB and its XML 399 MB with 1.4 M
// items; an item has about ten short fields).
type Limits struct {
	XML     int64 // bytes of XML after decompression
	Element int   // bytes of one element's text
	Fields  int   // elements in one ITEM
	Items   int   // items in the list
}

// DefaultLimits are used by Open.
var DefaultLimits = Limits{XML: 8 << 30, Element: 1 << 20, Fields: 256, Items: 20_000_000}

// Limits of the ZIP itself.
const (
	MaxZipBytes   = 1 << 30 // the download
	MaxZipEntries = 16      // a real export has two: the XML and its XSD
)

// ErrTooLarge marks an export over a limit.
var ErrTooLarge = errors.New("over the size limit")

type limitReader struct {
	r        io.Reader
	n, limit int64
}

func (l *limitReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if l.n += int64(n); l.n > l.limit {
		// withhold the bytes: a decoder that got its data might never ask again
		return 0, fmt.Errorf("%w: more than %d bytes of XML", ErrTooLarge, l.limit)
	}
	return n, err
}

// NewDecoder reads the XML of one export from r. Zero fields of lim take DefaultLimits.
func NewDecoder(slug string, r io.Reader, lim Limits) *Decoder {
	if lim.XML <= 0 {
		lim.XML = DefaultLimits.XML
	}
	if lim.Element <= 0 {
		lim.Element = DefaultLimits.Element
	}
	if lim.Fields <= 0 {
		lim.Fields = DefaultLimits.Fields
	}
	if lim.Items <= 0 {
		lim.Items = DefaultLimits.Items
	}
	return &Decoder{slug: slug, dec: xml.NewDecoder(&limitReader{r: r, limit: lim.XML}), lim: lim, closer: multiCloser{}}
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
			if b.Len()+len(v) > d.lim.Element {
				return "", fmt.Errorf("%w: an element of more than %d bytes", ErrTooLarge, d.lim.Element)
			}
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
					d.err = fmt.Errorf("item %d: %w", d.pos+1, err)
					return false
				}
				if f, ok := t.(xml.StartElement); ok {
					v, err := d.text()
					if err != nil {
						d.err = fmt.Errorf("item %d: %w", d.pos+1, err)
						return false
					}
					if m[f.Name.Local] = strings.TrimSpace(v); len(m) > d.lim.Fields {
						d.err = fmt.Errorf("item %d: %w: more than %d elements", d.pos+1, ErrTooLarge, d.lim.Fields)
						return false
					}
					continue
				}
				if _, ok := t.(xml.EndElement); ok {
					break
				}
			}
			if d.pos >= d.lim.Items {
				d.err = fmt.Errorf("%w: more than %d items", ErrTooLarge, d.lim.Items)
				return false
			}
			r, err := mapRow(d.slug, m)
			d.pos++
			if errors.Is(err, errNoIdentifier) {
				if d.skipped++; d.skipped > MaxUnidentified {
					d.err = fmt.Errorf("more than %d items without any identifier (DIC, ICO, IC_DPH), or with an IČO but no DIČ in the DIČ list; the latest is item %d", MaxUnidentified, d.pos)
					return false
				}
				continue
			}
			if err != nil {
				d.err = fmt.Errorf("item %d: %w", d.pos, err)
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

// Count is the number of items read, without the skipped ones.
func (d *Decoder) Count() int { return d.n }

// Skipped is the number of items left out because they carry no identifier at all.
func (d *Decoder) Skipped() int { return d.skipped }

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
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, MaxZipBytes+1))
	closeErr := f.Close()
	if n > MaxZipBytes || resp.ContentLength > MaxZipBytes {
		_ = os.Remove(path)
		return Fetched{}, fmt.Errorf("%s: %w: the download is more than %d bytes", slug, ErrTooLarge, int64(MaxZipBytes))
	}
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
