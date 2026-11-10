// Package rpo reads the Register of Legal Entities (RPO) bulk files published by the Ministry of
// the Interior: monthly snapshots (batch-init) and daily change files (batch-daily). Each file is a
// gzipped JSON document {"exportDate": "...", "results": [ record, ... ]}. Records are streamed one
// at a time; only the fields the register exposes are kept (personal-data minimisation).
package rpo

import (
	"crypto/sha256"
	"fmt"
	"strconv"
	"time"

	"github.com/accquix/register/internal/normalize"
)

// timed is RPO's TimedValue: a value with a validity period.
type timed struct {
	Value     string `json:"value"`
	ValidFrom string `json:"validFrom"`
	ValidTo   string `json:"validTo"`
}

type codeValue struct {
	Code  string `json:"code"`
	Value string `json:"value"`
}

type address struct {
	ValidFrom      string    `json:"validFrom"`
	ValidTo        string    `json:"validTo"`
	Street         string    `json:"street"`
	RegNumber      int64     `json:"regNumber"`
	BuildingNumber string    `json:"buildingNumber"`
	PostalCodes    []string  `json:"postalCodes"`
	Municipality   codeValue `json:"municipality"`
	Country        codeValue `json:"country"`
}

type legalForm struct {
	Value     codeValue `json:"value"`
	ValidFrom string    `json:"validFrom"`
	ValidTo   string    `json:"validTo"`
}

// record is the subset of an RPO record we read. Unknown fields (statutory bodies, stakeholders,
// activities, …) are skipped by the decoder and never stored.
type record struct {
	ID             int64       `json:"id"`
	Identifiers    []timed     `json:"identifiers"`
	FullNames      []timed     `json:"fullNames"`
	Addresses      []address   `json:"addresses"`
	LegalForms     []legalForm `json:"legalForms"`
	Establishment  string      `json:"establishment"`
	Termination    string      `json:"termination"`
	SourceRegister *struct {
		Value               codeValue `json:"value"`
		RegistrationOffices []timed   `json:"registrationOffices"`
		RegistrationNumbers []timed   `json:"registrationNumbers"`
	} `json:"sourceRegister"`
}

// Row is one normalised RPO record, ready for COPY.
type Row struct {
	ID                    int64
	ICO                   string // empty when the record has no valid 8-digit IČO
	Kind                  string
	Name                  string
	NameCore              string
	LegalSuffix           string
	LegalFormCode         string
	LegalForm             string
	EstablishedOn         string // YYYY-MM-DD or empty
	TerminatedOn          string
	Street                string
	BuildingNumber        string
	PostalCode            string
	Municipality          string
	Country               string
	RegisterCode          string
	RegisterName          string
	RegistrationOffice    string
	RegistrationNumber    string
	RegistrationValidFrom string
	Hash                  []byte // SHA-256 over the exposed fields above
}

func tFrom(t timed) string { return t.ValidFrom }
func tTo(t timed) string   { return t.ValidTo }

// KindOf maps an RPO legal-form code (codelist CL000056) to the register's kind. It is a coarse
// grouping: 101–110 are natural-person entrepreneurs, 3xx are public bodies.
func KindOf(code string) string {
	n, err := strconv.Atoi(code)
	switch {
	case code == "" || err != nil:
		return "other"
	case n >= 101 && n <= 110:
		return "sole_trader"
	case n >= 300 && n < 400:
		return "public_body"
	default:
		return "legal_person"
	}
}

func isoDate(s string) string {
	if len(s) < 10 {
		return ""
	}
	if _, err := time.Parse("2006-01-02", s[:10]); err != nil {
		return ""
	}
	return s[:10]
}

func (r *record) row() Row {
	out := Row{ID: r.ID}
	if id, ok := normalize.Current(r.Identifiers, tFrom, tTo); ok {
		if ico, valid := normalize.ICO(id.Value); valid {
			out.ICO = ico
		}
	}
	if nm, ok := normalize.Current(r.FullNames, tFrom, tTo); ok {
		out.Name = nm.Value
		out.NameCore, out.LegalSuffix = normalize.SplitName(nm.Value)
	}
	if lf, ok := normalize.Current(r.LegalForms, func(x legalForm) string { return x.ValidFrom }, func(x legalForm) string { return x.ValidTo }); ok {
		out.LegalFormCode, out.LegalForm = lf.Value.Code, lf.Value.Value
	}
	out.Kind = KindOf(out.LegalFormCode)
	out.EstablishedOn = isoDate(r.Establishment)
	out.TerminatedOn = isoDate(r.Termination)
	if a, ok := normalize.Current(r.Addresses, func(x address) string { return x.ValidFrom }, func(x address) string { return x.ValidTo }); ok {
		out.Street = a.Street
		switch {
		case a.RegNumber > 0 && a.BuildingNumber != "":
			out.BuildingNumber = fmt.Sprintf("%d/%s", a.RegNumber, a.BuildingNumber)
		case a.RegNumber > 0:
			out.BuildingNumber = strconv.FormatInt(a.RegNumber, 10)
		default:
			out.BuildingNumber = a.BuildingNumber
		}
		if len(a.PostalCodes) > 0 {
			out.PostalCode = normalize.PostalCode(a.PostalCodes[0])
		}
		out.Municipality = a.Municipality.Value
		out.Country = a.Country.Value
	}
	if sr := r.SourceRegister; sr != nil {
		out.RegisterCode, out.RegisterName = sr.Value.Code, sr.Value.Value
		if o, ok := normalize.Current(sr.RegistrationOffices, tFrom, tTo); ok {
			out.RegistrationOffice = o.Value
		}
		if n, ok := normalize.Current(sr.RegistrationNumbers, tFrom, tTo); ok {
			out.RegistrationNumber = n.Value
			out.RegistrationValidFrom = isoDate(n.ValidFrom)
		}
	}
	out.Hash = out.hash()
	return out
}

func (r *Row) hash() []byte {
	h := sha256.New()
	for _, f := range []string{
		r.ICO, r.Kind, r.Name, r.LegalFormCode, r.LegalForm, r.EstablishedOn, r.TerminatedOn,
		r.Street, r.BuildingNumber, r.PostalCode, r.Municipality, r.Country,
		r.RegisterCode, r.RegisterName, r.RegistrationOffice, r.RegistrationNumber, r.RegistrationValidFrom,
	} {
		h.Write([]byte(f))
		h.Write([]byte{0x1f})
	}
	return h.Sum(nil)
}
