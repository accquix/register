package rpo_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/accquix/register/internal/sources/rpo"
	"github.com/accquix/register/internal/testkit"
)

func decodeAll(t *testing.T, data []byte) ([]rpo.Row, *rpo.Decoder, error) {
	t.Helper()
	d, err := rpo.NewDecoder(bytes.NewReader(data))
	if err != nil {
		return nil, nil, err
	}
	var rows []rpo.Row
	for d.Next() {
		rows = append(rows, d.Row())
	}
	return rows, d, d.Err()
}

func TestDecodeNormalises(t *testing.T) {
	co := testkit.Company(1, "Zlatá Rybka, s.r.o.")
	co.OldNames = []string{"Strieborná Rybka, s.r.o."}
	tr := testkit.Trader(2, "Neuvedené", "Ján Fiktívny - Opravy")
	tr.Building, tr.RegNumber = "", 0
	gone := testkit.Company(3, "Zaniknutá Firma, a.s.")
	gone.LegalCode, gone.Terminated = "121", "2026-10-15"
	gone.OldNames = []string{"Pôvodná Firma, a.s."}

	rows, d, err := decodeAll(t, testkit.File("2026-11-07", []testkit.Rec{co, tr, gone}))
	if err != nil {
		t.Fatal(err)
	}
	if !d.ExportDate().Equal(time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)) || d.Count() != 3 {
		t.Fatalf("export date %v, count %d", d.ExportDate(), d.Count())
	}
	a, b, c := rows[0], rows[1], rows[2]

	if a.ICO != "99900001" || a.Name != "Zlatá Rybka, s.r.o." || a.NameCore != "zlata rybka" || a.LegalSuffix != "s r o" {
		t.Errorf("company identity: %+v", a)
	}
	if a.Kind != "legal_person" || a.BuildingNumber != "1/1" || a.PostalCode != "999 99" || a.RegistrationNumber != "Sro/1/F" ||
		a.RegistrationOffice != "Okresný súd Fiktívny" || a.RegisterCode != "1" || a.RegistrationValidFrom != "2020-01-01" {
		t.Errorf("company details: %+v", a)
	}
	if b.ICO != "" || b.Kind != "sole_trader" || b.BuildingNumber != "" || b.RegisterCode != "2" {
		t.Errorf("placeholder IČO must give no IČO: %+v", b)
	}
	// terminated: every attribute closed; the current name is the latest validTo, not the oldest
	if c.TerminatedOn != "2026-10-15" || c.Name != "Zaniknutá Firma, a.s." || c.Kind != "legal_person" {
		t.Errorf("terminated record: %+v", c)
	}
	if len(a.Hash) != 32 || bytes.Equal(a.Hash, b.Hash) {
		t.Error("row hashes must be set and differ")
	}
}

func TestHashIgnoresUnstoredFields(t *testing.T) {
	r := testkit.Company(5, "Firma Hash, s.r.o.")
	f1 := testkit.File("2026-11-07", []testkit.Rec{r})
	// a different export date and the same exposed content must hash the same
	f2 := testkit.File("2026-11-09", []testkit.Rec{r})
	r1, _, _ := decodeAll(t, f1)
	r2, _, _ := decodeAll(t, f2)
	if !bytes.Equal(r1[0].Hash, r2[0].Hash) {
		t.Error("hash depends on the export date")
	}
	r.Municipality = "Iné Mesto"
	r3, _, _ := decodeAll(t, testkit.File("2026-11-07", []testkit.Rec{r}))
	if bytes.Equal(r1[0].Hash, r3[0].Hash) {
		t.Error("hash ignores an exposed field")
	}
}

func gz(s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func TestDecodeErrors(t *testing.T) {
	cases := map[string][]byte{
		"not gzip":     []byte("{}"),
		"bad header":   gz(`{"results":[]}`),
		"bad date":     gz(`{"exportDate":"07.11.2026","results":[]}`),
		"truncated":    gz(`{"exportDate":"2026-11-07","results":[{"id":1},{"id":2`),
		"missing id":   gz(`{"exportDate":"2026-11-07","results":[{"fullNames":[]}]}`),
		"no close":     gz(`{"exportDate":"2026-11-07","results":[{"id":1}`),
		"wrong record": gz(`{"exportDate":"2026-11-07","results":[{"id":"x"}]}`),
	}
	for name, data := range cases {
		if _, _, err := decodeAll(t, data); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if rows, _, err := decodeAll(t, gz(`{"exportDate":"2026-11-07","results":[]}`)); err != nil || len(rows) != 0 {
		t.Errorf("empty file: %v %v", rows, err)
	}
}

func TestKindOf(t *testing.T) {
	for code, want := range map[string]string{"101": "sole_trader", "105": "sole_trader", "112": "legal_person", "121": "legal_person",
		"331": "public_body", "701": "legal_person", "": "other", "x": "other"} {
		if got := rpo.KindOf(code); got != want {
			t.Errorf("KindOf(%q) = %q, want %q", code, got, want)
		}
	}
}

func TestInventoryAndPaging(t *testing.T) {
	b := testkit.NewBucket(t)
	b.PutDaily("2026-11-08", nil)
	b.PutDaily("2026-11-09", nil)
	b.PutDaily("2026-11-10", nil)
	b.PutSnapshot("2026-11-07", []testkit.Rec{testkit.Company(1, "A"), testkit.Company(2, "B"), testkit.Company(3, "C")}, 2)
	b.Put("batch-init/unrelated.txt", []byte("x"))

	bk := rpo.NewBucket(b.URL())
	inv, err := bk.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Dailies) != 3 || inv.OldestDaily() != "2026-11-08" || inv.NewestDaily() != "2026-11-10" {
		t.Fatalf("dailies: %v", inv.Dailies)
	}
	s := inv.Snapshots["2026-11-07"]
	if s == nil || s.Manifest == nil || len(s.Parts) != 2 {
		t.Fatalf("snapshot: %+v", s)
	}
	names, err := bk.ReadManifest(context.Background(), s.Manifest.Key)
	if err != nil || strings.Join(names, ",") != "init_2026-11-07_001.json.gz,init_2026-11-07_002.json.gz" {
		t.Fatalf("manifest: %v %v", names, err)
	}
}

func TestDownloadResumesAndRetries(t *testing.T) {
	b := testkit.NewBucket(t)
	data := testkit.File("2026-11-08", []testkit.Rec{testkit.Company(1, "Resume Test")})
	b.Put("batch-daily/actual_2026-11-08.json.gz", data)
	obj := rpo.Object{Key: "batch-daily/actual_2026-11-08.json.gz", Size: int64(len(data))}
	dir := t.TempDir()

	// a partial file from an earlier attempt is resumed with a Range request
	if err := os.WriteFile(filepath.Join(dir, "actual_2026-11-08.json.gz"), data[:10], 0o600); err != nil {
		t.Fatal(err)
	}
	bk := rpo.NewBucket(b.URL())
	bk.RetryDelay = time.Millisecond
	p, err := bk.Download(context.Background(), obj, dir)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if !bytes.Equal(got, data) {
		t.Fatal("resumed file differs")
	}

	// a dropped connection is retried and completes
	os.Remove(p)
	b.FailNext[obj.Key] = 5
	if _, err := bk.Download(context.Background(), obj, dir); err != nil {
		t.Fatalf("retry: %v", err)
	}
	got, _ = os.ReadFile(p)
	if !bytes.Equal(got, data) {
		t.Fatal("retried file differs")
	}

	// a size mismatch is an error after the retries
	bk.Retries = 1
	if _, err := bk.Download(context.Background(), rpo.Object{Key: obj.Key, Size: obj.Size + 1}, t.TempDir()); err == nil {
		t.Fatal("want a size mismatch error")
	}
}

func TestDownloadAll(t *testing.T) {
	b := testkit.NewBucket(t)
	var objs []rpo.Object
	for i := 1; i <= 5; i++ {
		data := testkit.File("2026-11-07", []testkit.Rec{testkit.Company(i, "Parallel")})
		key := "batch-init/p" + string(rune('0'+i)) + ".json.gz"
		b.Put(key, data)
		objs = append(objs, rpo.Object{Key: key, Size: int64(len(data))})
	}
	paths, err := rpo.NewBucket(b.URL()).DownloadAll(context.Background(), objs, t.TempDir(), 3)
	if err != nil || len(paths) != 5 {
		t.Fatalf("%v %v", paths, err)
	}
	for i, p := range paths {
		if filepath.Base(p) != filepath.Base(objs[i].Key) {
			t.Errorf("path %d = %s", i, p)
		}
	}
	objs = append(objs, rpo.Object{Key: "batch-init/missing.json.gz", Size: 1})
	bk := rpo.NewBucket(b.URL())
	bk.Retries, bk.RetryDelay = 1, time.Millisecond
	if _, err := bk.DownloadAll(context.Background(), objs, t.TempDir(), 3); err == nil {
		t.Fatal("want an error for a missing object")
	}
}
