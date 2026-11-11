package rpo_test

import (
	"bytes"
	"compress/gzip"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/accquix/register/internal/sources/rpo"
	"github.com/accquix/register/internal/testkit"
)

func gunzip(t testing.TB, b []byte) []byte {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := out.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

var (
	reICO  = regexp.MustCompile(`^[0-9]{8}$`)
	reDate = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	kinds  = map[string]bool{"legal_person": true, "sole_trader": true, "public_body": true, "other": true}
)

// small limits, so that the fuzzer can reach them
var fuzzLimits = rpo.Limits{Decompressed: 1 << 20, Record: 64 << 10, Records: 200}

func checkRows(t *testing.T, d *rpo.Decoder) {
	for d.Next() {
		r := d.Row()
		if r.ID <= 0 {
			t.Fatalf("a record without a positive id was accepted: %d", r.ID)
		}
		if r.ICO != "" && !reICO.MatchString(r.ICO) {
			t.Fatalf("IČO %q is not 8 digits", r.ICO)
		}
		for _, date := range []string{r.EstablishedOn, r.TerminatedOn, r.RegistrationValidFrom} {
			if date == "" {
				continue
			}
			if _, err := time.Parse("2006-01-02", date); err != nil || !reDate.MatchString(date) {
				t.Fatalf("date %q is not a calendar date", date)
			}
		}
		if !kinds[r.Kind] {
			t.Fatalf("kind %q", r.Kind)
		}
		if len(r.Hash) != 32 {
			t.Fatalf("hash of %d bytes", len(r.Hash))
		}
		if strings.ContainsRune(r.NameCore, 0) || r.NameCore != strings.TrimSpace(r.NameCore) {
			t.Fatalf("name core %q", r.NameCore)
		}
		if d.Count() > fuzzLimits.Records {
			t.Fatalf("%d records passed a limit of %d", d.Count(), fuzzLimits.Records)
		}
	}
}

// FuzzDecoder feeds arbitrary JSON (gzipped by the test) to the RPO decoder: it must never panic,
// never accept a record that breaks the row invariants, and never read past its limits.
func FuzzDecoder(f *testing.F) {
	f.Add(gunzip(f, testkit.File("2026-01-03", []testkit.Rec{testkit.Company(1, "Alfa Stavby, s.r.o."), testkit.Trader(4, "99900004", "Ján Fiktívny")})))
	f.Add(gunzip(f, testkit.File("2026-01-03", nil)))
	f.Add([]byte(`{"exportDate":"2026-01-03","results":[{"id":1,"identifiers":[{"value":"Neuvedené"}],"fullNames":[{"value":"\u0000 A, s.r.o."}]}]}`))
	f.Add([]byte(`{"exportDate":"2026-02-31","results":[]}`))
	f.Add([]byte(`{"exportDate":"2026-01-03","results":[{"id":-1}]}`))
	f.Add([]byte(`{"exportDate":"2026-01-03","results":[{"id":1e99}]}`))
	f.Add([]byte(`{"results":[],"exportDate":"2026-01-03"}`))
	f.Add([]byte(`{"exportDate":"2026-01-03","results":[{"id":1,"addresses":[{"regNumber":"x","postalCodes":[null]}]}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		d, err := rpo.NewDecoderLimits(bytes.NewReader(gz(string(raw))), fuzzLimits)
		if err != nil {
			return
		}
		checkRows(t, d)
	})
}

// FuzzDecoderGzip feeds arbitrary bytes as the compressed stream itself.
func FuzzDecoderGzip(f *testing.F) {
	f.Add(testkit.File("2026-01-03", []testkit.Rec{testkit.Company(1, "Alfa Stavby, s.r.o.")}))
	f.Add([]byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0})
	f.Add([]byte("not gzip"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		d, err := rpo.NewDecoderLimits(bytes.NewReader(raw), fuzzLimits)
		if err != nil {
			return
		}
		checkRows(t, d)
	})
}

// The limits end a file as an error the unit can reject, at the point where the limit is passed.
func TestDecoderLimits(t *testing.T) {
	var recs []testkit.Rec
	for i := 1; i <= 50; i++ {
		recs = append(recs, testkit.Company(i, "Firma s veľmi dlhým názvom číslo "+strings.Repeat("x", 200)))
	}
	file := testkit.File("2026-01-03", recs)
	read := func(lim rpo.Limits) (int, error) {
		d, err := rpo.NewDecoderLimits(bytes.NewReader(file), lim)
		if err != nil {
			return 0, err
		}
		for d.Next() {
		}
		return d.Count(), d.Err()
	}
	if n, err := read(rpo.Limits{}); err != nil || n != 50 {
		t.Fatalf("default limits: %d %v", n, err)
	}
	if n, err := read(rpo.Limits{Records: 10}); !errors.Is(err, rpo.ErrTooLarge) || n != 10 {
		t.Fatalf("record count limit: read %d, %v", n, err)
	}
	if n, err := read(rpo.Limits{Decompressed: 4 << 10}); !errors.Is(err, rpo.ErrTooLarge) || n >= 50 {
		t.Fatalf("decompressed size limit: read %d, %v", n, err)
	}
	if _, err := read(rpo.Limits{Record: 100}); !errors.Is(err, rpo.ErrTooLarge) {
		t.Fatalf("record size limit: %v", err)
	}
	// a gzip bomb: a few KB that decompress to many MB of padding inside one record
	bomb := gz(`{"exportDate":"2026-01-03","results":[{"id":1,"x":"` + strings.Repeat("A", 8<<20) + `"}]}`)
	if len(bomb) > 64<<10 {
		t.Fatalf("the bomb is %d bytes", len(bomb))
	}
	d, err := rpo.NewDecoderLimits(bytes.NewReader(bomb), rpo.Limits{Decompressed: 1 << 20, Record: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	for d.Next() {
	}
	if !errors.Is(d.Err(), rpo.ErrTooLarge) {
		t.Fatalf("gzip bomb: %v", d.Err())
	}
}

// An object the bucket lists as larger than the limit is refused without a request.
func TestDownloadRefusesAnOversizedObject(t *testing.T) {
	b := testkit.NewBucket(t)
	bk := rpo.NewBucket(b.URL())
	for key, size := range map[string]int64{
		"batch-daily/actual_2026-01-04.json.gz":  rpo.MaxDailyBytes + 1,
		"batch-init/init_2026-01-03_001.json.gz": rpo.MaxPartBytes + 1,
	} {
		if _, err := bk.Download(t.Context(), rpo.Object{Key: key, Size: size}, t.TempDir()); !errors.Is(err, rpo.ErrTooLarge) {
			t.Fatalf("%s: %v", key, err)
		}
		if b.Gets[key] != 0 {
			t.Fatalf("%s was requested", key)
		}
	}
}
