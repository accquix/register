// Package lookup assembles a subject from the source tables (D20–D27). Each section carries a
// state: present (a row exists), absent (the source is fresh and has no row) or unknown (the source
// was never published or is stale). Booleans derived from a section are only true/false on fresh
// data; otherwise null. A section's version is a hash of its content, so a rebuild from the
// sources gives the same versions.
package lookup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accquix/register/internal/normalize"
)

// Freshness thresholds (D24), in days since the source's published as-of date.
var maxAge = map[string]int{"rpo": 3, "fs:ds_iz_ran": 14}

const defaultFSMaxAge = 2

// Sources the API needs before it serves (D22).
var AllSources = []string{"rpo", "fs:ds_dphs", "fs:ds_dsrdp", "fs:ds_iz_ran", "fs:ds_dphz", "fs:ds_dphv", "fs:ds_dpho"}

var riskSources = []string{"fs:ds_iz_ran", "fs:ds_dphz", "fs:ds_dphv", "fs:ds_dpho"}

// Attribution is returned with every answer.
var Attribution = []string{
	"Register právnických osôb, Ministerstvo vnútra SR, CC BY 4.0",
	"Zdroj údajov: Finančná správa SR",
}

// SourceInfo is one row of sync_source plus its freshness.
type SourceInfo struct {
	Source          string `json:"source"`
	State           string `json:"state" enum:"empty,bootstrap,daily,gap_wait,reconcile,blocked"`
	PublishedAsOf   string `json:"publishedAsOf,omitempty" doc:"Date the published data was stamped by the agency"`
	Stale           bool   `json:"stale"`
	MaxAgeDays      int    `json:"maxAgeDays"`
	Diagnostic      string `json:"diagnostic,omitempty"`
	Items           int    `json:"items,omitempty" doc:"Financial Administration lists: items of the published file kept, with an identifier"`
	SkippedItems    int    `json:"skippedItems,omitempty" doc:"Items of the published file left out because they carry no identifier at all. Above the list's cap (0.05 % of the DIČ list, at most 1000; 0.01 % of the others, at most 100; never less than 10) the list is blocked"`
	ItemsWithoutDIC int    `json:"itemsWithoutDic,omitempty" doc:"DIČ list: items left out because they have an IČO and no DIČ yet (new businesses). Normal, with no limit"`
}

func (s SourceInfo) published() bool { return s.PublishedAsOf != "" }
func (s SourceInfo) fresh() bool     { return s.published() && !s.Stale }

// Store reads the mirror.
type Store struct {
	Pool *pgxpool.Pool
	Now  func() time.Time
}

// Sources returns every expected source, published or not.
func (s *Store) Sources(ctx context.Context) (map[string]SourceInfo, error) {
	rows, err := s.Pool.Query(ctx, `SELECT source, state, published_as_of, coalesce(diagnostic, ''),
  coalesce((cursor->>'skipped')::int, 0), coalesce((cursor->>'noDicYet')::int, 0), coalesce((cursor->>'items')::int, 0) FROM sync_source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]SourceInfo{}
	for _, src := range AllSources {
		out[src] = SourceInfo{Source: src, State: "empty"}
	}
	today := s.Now().UTC().Truncate(24 * time.Hour)
	for rows.Next() {
		var si SourceInfo
		var pub *time.Time
		if err := rows.Scan(&si.Source, &si.State, &pub, &si.Diagnostic, &si.SkippedItems, &si.ItemsWithoutDIC, &si.Items); err != nil {
			return nil, err
		}
		si.MaxAgeDays = defaultFSMaxAge
		if d, ok := maxAge[si.Source]; ok {
			si.MaxAgeDays = d
		}
		if pub != nil {
			si.PublishedAsOf = pub.Format("2006-01-02")
			age := today.Sub(pub.UTC().Truncate(24 * time.Hour))
			// A date after tomorrow cannot be right: never treat it as fresh (the sync rejects such
			// files; this is the second line, for data that got in some other way).
			si.Stale = age > time.Duration(si.MaxAgeDays)*24*time.Hour || age < -24*time.Hour
		}
		out[si.Source] = si
	}
	for k, v := range out {
		if v.MaxAgeDays == 0 {
			v.MaxAgeDays = defaultFSMaxAge
			if d, ok := maxAge[k]; ok {
				v.MaxAgeDays = d
			}
			out[k] = v
		}
	}
	return out, rows.Err()
}

// ObjectionsConfirmed reports whether the operator confirmed the objections of this installation
// (restored from a backup, or declared a new installation). Until then the API does not serve:
// an empty database cannot tell a first installation from one that lost its objections.
// Setup reports whether the objections are confirmed, and whether the database is a demo (D41).
// A search rebuild that is owed after an upgrade (migration 00006) counts as not confirmed: the
// search layer may still show what the new rules hide.
func (s *Store) Setup(ctx context.Context) (confirmed, demo bool, err error) {
	err = s.Pool.QueryRow(ctx, `SELECT coalesce(objections_confirmed_at IS NOT NULL AND NOT search_rebuild_pending, false), coalesce(demo_since IS NOT NULL, false)
FROM (SELECT 1) one LEFT JOIN register_setup ON only_row`).Scan(&confirmed, &demo)
	return confirmed, demo, err
}

func (s *Store) ObjectionsConfirmed(ctx context.Context) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx, `SELECT coalesce((SELECT objections_confirmed_at IS NOT NULL AND NOT search_rebuild_pending FROM register_setup WHERE only_row), false)`).Scan(&ok)
	return ok, err
}

// Bootstrapping lists the sources not yet published; the API serves only when it is empty.
func Bootstrapping(srcs map[string]SourceInfo) []string {
	var missing []string
	for _, s := range AllSources {
		if !srcs[s].published() {
			missing = append(missing, s)
		}
	}
	return missing
}

// SourceRef tells where a section comes from and how fresh it is.
type SourceRef struct {
	Dataset   string     `json:"dataset"`
	AsOf      string     `json:"asOf,omitempty" doc:"As-of date of the dataset (the agency's date)"`
	ChangedAt *time.Time `json:"changedAt,omitempty" doc:"When the register last saw this content change"`
	Version   string     `json:"version,omitempty" doc:"Content hash of the section; compare to detect changes"`
	Stale     bool       `json:"stale"`
}

// Registration is one RPO record of the subject.
type Registration struct {
	Register     string  `json:"register,omitempty" doc:"Source register, e.g. Obchodný register"`
	RegisterCode string  `json:"registerCode,omitempty"`
	Office       string  `json:"office,omitempty" doc:"Registration court or office"`
	Number       string  `json:"number,omitempty" doc:"Registration number (oddiel/vložka or trade licence number)"`
	ValidFrom    *string `json:"validFrom,omitempty"`
	Name         string  `json:"name,omitempty" doc:"The subject's name in this record"`
	Active       bool    `json:"active"`
	TerminatedOn *string `json:"terminatedOn,omitempty"`
}

// Address is the registered seat or place of business.
type Address struct {
	Street         string `json:"street,omitempty"`
	BuildingNumber string `json:"buildingNumber,omitempty"`
	PostalCode     string `json:"postalCode,omitempty"`
	Municipality   string `json:"municipality,omitempty"`
	Country        string `json:"country,omitempty"`
}

// Identity is the RPO section.
type Identity struct {
	State            string         `json:"state" enum:"present,absent,unknown"`
	Source           SourceRef      `json:"source"`
	Name             string         `json:"name,omitempty"`
	Kind             string         `json:"kind,omitempty" enum:"legal_person,sole_trader,public_body,other"`
	LegalFormCode    string         `json:"legalFormCode,omitempty"`
	LegalForm        string         `json:"legalForm,omitempty"`
	EstablishedOn    *string        `json:"establishedOn,omitempty"`
	TerminatedOn     *string        `json:"terminatedOn,omitempty"`
	Active           *bool          `json:"active,omitempty"`
	Address          *Address       `json:"address,omitempty"`
	Registrations    []Registration `json:"registrations,omitempty"`
	ICOChecksumValid *bool          `json:"icoChecksumValid,omitempty" doc:"The IČO check digit; a warning only, real IČOs exist that fail it"`
}

// Tax is the ds_dsrdp section.
type Tax struct {
	State  string    `json:"state" enum:"present,absent,unknown"`
	Source SourceRef `json:"source"`
	DIC    string    `json:"dic,omitempty"`
}

// VAT is the ds_dphs section (D25).
type VAT struct {
	State         string    `json:"state" enum:"present,absent,unknown"`
	Source        SourceRef `json:"source"`
	ICDPH         string    `json:"icDph,omitempty"`
	Paragraph     string    `json:"paragraph,omitempty" enum:"§4,§4b,§5,§7,§7a" doc:"§7 and §7a are registered but are not VAT payers"`
	Payer         *bool     `json:"payer" doc:"true/false only from fresh data; null when unknown"`
	RegisteredOn  *string   `json:"registeredOn,omitempty"`
	PayerFrom     *string   `json:"payerFrom,omitempty" doc:"Always null for §7 and §7a"`
	TypeChangedOn *string   `json:"typeChangedOn,omitempty"`
	Name          string    `json:"name,omitempty" doc:"Name as registered for VAT"`
}

// ListEntry is one entry of a Financial Administration list.
type ListEntry struct {
	Dataset string            `json:"dataset"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// Risk holds the reliability index and the risk lists.
type Risk struct {
	State            string      `json:"state" enum:"present,absent,unknown"`
	Source           SourceRef   `json:"source"`
	ReliabilityIndex string      `json:"reliabilityIndex,omitempty" doc:"ds_iz_ran: vysoko spoľahlivý, spoľahlivý, menej spoľahlivý"`
	Entries          []ListEntry `json:"entries,omitempty" doc:"ds_dphz (grounds to cancel VAT registration), ds_dphv (removed from that list), ds_dpho (special scheme)"`
}

// Subject is one assembled subject.
type Subject struct {
	ICO      string   `json:"ico,omitempty"`
	Identity Identity `json:"identity"`
	Tax      Tax      `json:"tax"`
	VAT      VAT      `json:"vat"`
	Risk     Risk     `json:"risk"`
}

// Taxpayer is a natural person without an IČO (D26): DIČ, name and municipality only.
type Taxpayer struct {
	DIC          string    `json:"dic"`
	Name         string    `json:"name,omitempty"`
	Municipality string    `json:"municipality,omitempty"`
	Source       SourceRef `json:"source"`
}

func version(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:12])
}

func day(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format("2006-01-02")
	return &s
}

func ref(src SourceInfo, dataset string) SourceRef {
	return SourceRef{Dataset: dataset, AsOf: src.PublishedAsOf, Stale: src.Stale}
}

func absentOrUnknown(src SourceInfo) string {
	if src.fresh() {
		return "absent"
	}
	return "unknown"
}

func latest(ts ...*time.Time) *time.Time {
	var out *time.Time
	for _, t := range ts {
		if t != nil && (out == nil || t.After(*out)) {
			out = t
		}
	}
	return out
}

// Subject assembles the subject with this IČO. found is false when no source has a row.
func (s *Store) Subject(ctx context.Context, srcs map[string]SourceInfo, ico string) (Subject, bool, error) {
	sub := Subject{ICO: ico}
	found := false
	var err error
	if sub.Identity, err = s.identity(ctx, srcs["rpo"], ico); err != nil {
		return sub, false, err
	}
	found = found || sub.Identity.State == "present"
	if sub.Tax, err = s.tax(ctx, srcs["fs:ds_dsrdp"], `ico = $1`, ico); err != nil {
		return sub, false, err
	}
	found = found || sub.Tax.State == "present"
	vats, err := s.vat(ctx, srcs["fs:ds_dphs"], `ico = $1`, ico, true)
	if err != nil {
		return sub, false, err
	}
	sub.VAT = vats[0]
	found = found || sub.VAT.State == "present"
	if sub.Risk, err = s.risk(ctx, srcs, ico, sub.Tax.DIC); err != nil {
		return sub, false, err
	}
	found = found || sub.Risk.State == "present"
	return sub, found, nil
}

func (s *Store) identity(ctx context.Context, src SourceInfo, ico string) (Identity, error) {
	rows, err := s.Pool.Query(ctx, `SELECT name, kind, legal_form_code, legal_form, established_on, terminated_on, street, building_number,
  postal_code, municipality, country, register_code, register_name, registration_office, registration_number, registration_valid_from, changed_at
FROM rpo_record WHERE ico = $1 AND removed_at IS NULL
ORDER BY (coalesce(register_code, '') IN ('1', '2')) DESC, src_date DESC, changed_at DESC, id DESC`, ico)
	if err != nil {
		return Identity{}, err
	}
	type rec struct {
		name, kind, lfCode, lf, street, bnum, psc, muni, country, regCode, regName, office, number *string
		est, term, regFrom, changed                                                                *time.Time
	}
	recs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (rec, error) {
		var x rec
		err := r.Scan(&x.name, &x.kind, &x.lfCode, &x.lf, &x.est, &x.term, &x.street, &x.bnum, &x.psc, &x.muni, &x.country,
			&x.regCode, &x.regName, &x.office, &x.number, &x.regFrom, &x.changed)
		return x, err
	})
	if err != nil {
		return Identity{}, err
	}
	id := Identity{Source: ref(src, "rpo")}
	if len(recs) == 0 {
		id.State = absentOrUnknown(src)
		return id, nil
	}
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	// the display record: an active one first, in the same order as the search layer
	sort.SliceStable(recs, func(a, b int) bool { return (recs[a].term == nil) && (recs[b].term != nil) })
	p := recs[0]
	id.State = "present"
	id.Name, id.Kind, id.LegalFormCode, id.LegalForm = str(p.name), str(p.kind), str(p.lfCode), str(p.lf)
	id.EstablishedOn, id.TerminatedOn = day(p.est), day(p.term)
	active := false
	var changed *time.Time
	for _, r := range recs {
		active = active || r.term == nil
		changed = latest(changed, r.changed)
		id.Registrations = append(id.Registrations, Registration{Register: str(r.regName), RegisterCode: str(r.regCode),
			Office: str(r.office), Number: str(r.number), ValidFrom: day(r.regFrom), Name: str(r.name), Active: r.term == nil, TerminatedOn: day(r.term)})
	}
	id.Active = &active
	id.Address = &Address{Street: str(p.street), BuildingNumber: str(p.bnum), PostalCode: str(p.psc), Municipality: str(p.muni), Country: str(p.country)}
	ok := normalize.ICOChecksumValid(ico)
	id.ICOChecksumValid = &ok
	content := id
	content.Source = SourceRef{}
	id.Source.Version = version(content)
	id.Source.ChangedAt = changed
	return id, nil
}

func (s *Store) tax(ctx context.Context, src SourceInfo, where string, arg any) (Tax, error) {
	t := Tax{Source: ref(src, "ds_dsrdp")}
	var changed *time.Time
	err := s.Pool.QueryRow(ctx, `SELECT dic, changed_at FROM fs_tax_subject WHERE `+where+` AND absent_since IS NULL ORDER BY dic LIMIT 1`, arg).
		Scan(&t.DIC, &changed)
	if errors.Is(err, pgx.ErrNoRows) {
		t.State = absentOrUnknown(src)
		return t, nil
	}
	if err != nil {
		return t, err
	}
	t.State = "present"
	t.Source.Version = version(map[string]string{"dic": t.DIC})
	t.Source.ChangedAt = changed
	return t, nil
}

var payers = map[string]bool{"§4": true, "§4b": true, "§5": true, "§7": false, "§7a": false}

// vat returns one VAT section per matching row (several for a VAT group looked up by IČ DPH);
// with none, a single absent/unknown section. withName adds the VAT name.
func (s *Store) vat(ctx context.Context, src SourceInfo, where string, arg any, withName bool) ([]VAT, error) {
	rows, err := s.Pool.Query(ctx, `SELECT ic_dph, coalesce(ico, ''), coalesce(name, ''), paragraph, registered_on, payer_from, type_changed_on, changed_at
FROM fs_vat_payer WHERE `+where+` AND absent_since IS NULL ORDER BY ic_dph, vkey`, arg)
	if err != nil {
		return nil, err
	}
	type row struct {
		v       VAT
		ico     string
		changed *time.Time
	}
	rs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		var reg, pf, tc *time.Time
		err := r.Scan(&x.v.ICDPH, &x.ico, &x.v.Name, &x.v.Paragraph, &reg, &pf, &tc, &x.changed)
		x.v.RegisteredOn, x.v.PayerFrom, x.v.TypeChangedOn = day(reg), day(pf), day(tc)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	if len(rs) == 0 {
		v := VAT{State: absentOrUnknown(src), Source: ref(src, "ds_dphs")}
		if src.fresh() {
			f := false
			v.Payer = &f // fresh list, no row: not registered for VAT as of the list date
		}
		return []VAT{v}, nil
	}
	out := make([]VAT, 0, len(rs))
	for _, r := range rs {
		v := r.v
		v.State, v.Source = "present", ref(src, "ds_dphs")
		if !withName {
			v.Name = ""
		}
		if src.fresh() {
			p := payers[v.Paragraph]
			v.Payer = &p
		}
		content := v
		content.Source, content.Payer = SourceRef{}, nil
		v.Source.Version = version(content)
		v.Source.ChangedAt = r.changed
		out = append(out, v)
	}
	return out, nil
}

func (s *Store) risk(ctx context.Context, srcs map[string]SourceInfo, ico, dic string) (Risk, error) {
	r := Risk{Source: SourceRef{Dataset: "ds_iz_ran,ds_dphz,ds_dphv,ds_dpho"}}
	allFresh, oldest := true, ""
	for _, name := range riskSources {
		si := srcs[name]
		allFresh = allFresh && si.fresh()
		r.Source.Stale = r.Source.Stale || si.Stale
		if si.PublishedAsOf != "" && (oldest == "" || si.PublishedAsOf < oldest) {
			oldest = si.PublishedAsOf
		}
	}
	r.Source.AsOf = oldest
	rows, err := s.Pool.Query(ctx, `SELECT dataset, attrs, changed_at FROM fs_list_entry
WHERE (ico = $1 OR ($2 <> '' AND dic = $2)) AND absent_since IS NULL ORDER BY dataset, subject_key`, ico, dic)
	if err != nil {
		return r, err
	}
	var changed *time.Time
	for rows.Next() {
		var e ListEntry
		var raw []byte
		var ch *time.Time
		if err := rows.Scan(&e.Dataset, &raw, &ch); err != nil {
			rows.Close()
			return r, err
		}
		_ = json.Unmarshal(raw, &e.Attrs)
		changed = latest(changed, ch)
		if e.Dataset == "ds_iz_ran" {
			r.ReliabilityIndex = e.Attrs["IDS"]
			continue
		}
		r.Entries = append(r.Entries, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return r, err
	}
	if r.ReliabilityIndex == "" && len(r.Entries) == 0 {
		if allFresh {
			r.State = "absent"
		} else {
			r.State = "unknown"
		}
		return r, nil
	}
	r.State = "present"
	content := r
	content.Source = SourceRef{}
	r.Source.Version = version(content)
	r.Source.ChangedAt = changed
	return r, nil
}

// ByDIC resolves a DIČ: to a subject when the taxpayer has an IČO, else to a Taxpayer (D26).
func (s *Store) ByDIC(ctx context.Context, srcs map[string]SourceInfo, dic string) (*Subject, *Taxpayer, error) {
	var ico, name, muni string
	var changed *time.Time
	err := s.Pool.QueryRow(ctx, `SELECT coalesce(ico, ''), coalesce(name, ''), coalesce(municipality, ''), changed_at
FROM fs_tax_subject WHERE dic = $1 AND absent_since IS NULL`, dic).Scan(&ico, &name, &muni, &changed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if ico != "" {
		sub, _, err := s.Subject(ctx, srcs, ico)
		return &sub, nil, err
	}
	src := srcs["fs:ds_dsrdp"]
	tp := &Taxpayer{DIC: dic, Name: name, Municipality: muni, Source: ref(src, "ds_dsrdp")}
	tp.Source.Version = version(map[string]string{"dic": dic, "name": name, "municipality": muni})
	tp.Source.ChangedAt = changed
	return nil, tp, nil
}

// ByICDPH returns the subjects registered under an IČ DPH (several for a VAT group). Members
// without an IČO get only their VAT section; withNames controls whether their name is included.
func (s *Store) ByICDPH(ctx context.Context, srcs map[string]SourceInfo, icdph string, withNames bool) ([]Subject, error) {
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT coalesce(ico, '') FROM fs_vat_payer WHERE ic_dph = $1 AND absent_since IS NULL`, icdph)
	if err != nil {
		return nil, err
	}
	icos, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	// members with an IČO first, in IČO order; members without one last
	sort.Slice(icos, func(a, b int) bool {
		if (icos[a] == "") != (icos[b] == "") {
			return icos[b] == ""
		}
		return icos[a] < icos[b]
	})
	var out []Subject
	for _, ico := range icos {
		if ico != "" {
			sub, _, err := s.Subject(ctx, srcs, ico)
			if err != nil {
				return nil, err
			}
			out = append(out, sub)
			continue
		}
		vats, err := s.vat(ctx, srcs["fs:ds_dphs"], `ic_dph = $1 AND ico IS NULL`, icdph, withNames)
		if err != nil {
			return nil, err
		}
		for _, v := range vats {
			out = append(out, Subject{VAT: v,
				Identity: Identity{State: "unknown", Source: ref(srcs["rpo"], "rpo")},
				Tax:      Tax{State: "unknown", Source: ref(srcs["fs:ds_dsrdp"], "ds_dsrdp")},
				Risk:     Risk{State: "unknown", Source: SourceRef{Dataset: "ds_iz_ran,ds_dphz,ds_dphv,ds_dpho"}}})
		}
	}
	return out, nil
}

// Fresh reports whether every named source is published and not stale.
func Fresh(srcs map[string]SourceInfo, names ...string) bool {
	for _, n := range names {
		if !srcs[n].fresh() {
			return false
		}
	}
	return true
}

// StaleNames lists the named sources that are not fresh.
func StaleNames(srcs map[string]SourceInfo, names ...string) string {
	var out []string
	for _, n := range names {
		if !srcs[n].fresh() {
			out = append(out, n)
		}
	}
	return strings.Join(out, ", ")
}
