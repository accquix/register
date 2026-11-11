// Package synthetic writes fictional register data in the real file layouts of the sources, and
// serves it like the sources do. Tests use it through testkit; the demo mode (D41) loads it into
// a database. Everything it produces is synthetic: fictional names, IČOs in the 999xxxxx range,
// DIČs from 20999xxxxx and RPO ids from 900 000 000, which do not exist in the registers.
package synthetic

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Rec is a synthetic RPO record.
type Rec struct {
	ID           int64
	ICO          string // "" for none; any string is written as is (e.g. a placeholder)
	Name         string
	OldNames     []string // closed earlier names
	LegalCode    string
	LegalForm    string
	Established  string
	Terminated   string // also closes every attribute (as RPO does)
	Street       string
	RegNumber    int64
	Building     string
	PSC          string
	Municipality string
	RegisterCode string
	RegisterName string
	Office       string
	Number       string
	RegFrom      string
}

func tv(value, from, to string) map[string]any {
	m := map[string]any{"value": value, "validFrom": from}
	if to != "" {
		m["validTo"] = to
	}
	return m
}

// JSON renders the record in the RPO layout (plus a few fields the register must ignore).
func (r Rec) JSON() map[string]any {
	from := r.Established
	if from == "" {
		from = "2020-01-01"
	}
	to := r.Terminated
	m := map[string]any{"id": r.ID, "establishment": from}
	if r.Terminated != "" {
		m["termination"] = r.Terminated
	}
	if r.ICO != "" {
		m["identifiers"] = []any{tv(r.ICO, from, to)}
	}
	names := []any{}
	prev := "2000-01-01"
	for _, n := range r.OldNames {
		names = append(names, tv(n, prev, from))
		prev = from
	}
	names = append(names, tv(r.Name, from, to))
	m["fullNames"] = names
	if r.LegalCode != "" {
		m["legalForms"] = []any{map[string]any{"value": map[string]any{"codelistCode": "CL000056", "code": r.LegalCode, "value": r.LegalForm}, "validFrom": from}}
	}
	addr := map[string]any{"validFrom": from, "street": r.Street, "regNumber": r.RegNumber, "buildingNumber": r.Building,
		"postalCodes": []string{r.PSC}, "municipality": map[string]any{"value": r.Municipality}, "country": map[string]any{"value": "Slovenská republika"}}
	if to != "" {
		addr["validTo"] = to
	}
	m["addresses"] = []any{addr}
	if r.RegisterCode != "" {
		m["sourceRegister"] = map[string]any{
			"value":               map[string]any{"codelistCode": "CL010112", "code": r.RegisterCode, "value": r.RegisterName},
			"registrationOffices": []any{tv(r.Office, r.RegFrom, "")},
			"registrationNumbers": []any{tv(r.Number, r.RegFrom, "")},
		}
	}
	// personal data the register must never store
	m["statutoryBodies"] = []any{map[string]any{"personName": map[string]any{"givenNames": []string{"Fiktívny"}, "familyNames": []string{"Štatutár"}},
		"address": map[string]any{"street": "Bydlisková", "municipality": map[string]any{"value": "Niekde"}}}}
	return m
}

// File renders a gzipped RPO file: {"exportDate": ..., "results": [ one record per line ]}.
func File(exportDate string, recs []Rec) []byte {
	var raw bytes.Buffer
	fmt.Fprintf(&raw, "{\"exportDate\":%q,\"results\":[\n", exportDate)
	for i, r := range recs {
		b, _ := json.Marshal(r.JSON())
		if i > 0 {
			raw.WriteString(",")
		}
		raw.Write(b)
		raw.WriteString("\n")
	}
	raw.WriteString("]}")
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(raw.Bytes())
	_ = w.Close()
	return gz.Bytes()
}

// Company returns a synthetic Commercial-Register s.r.o. with IČO 9990nnnn.
func Company(n int, name string) Rec {
	return Rec{ID: 900_000_000 + int64(n), ICO: fmt.Sprintf("9990%04d", n), Name: name, LegalCode: "112",
		LegalForm: "Spoločnosť s ručením obmedzeným", Established: "2020-01-01", Street: "Testovacia", RegNumber: int64(n), Building: "1",
		PSC: "99999", Municipality: "Fiktívne Mesto", RegisterCode: "1", RegisterName: "Obchodný register",
		Office: "Okresný súd Fiktívny", Number: "Sro/" + strconv.Itoa(n) + "/F", RegFrom: "2020-01-01"}
}

// Trader returns a synthetic Trade-Register sole trader.
func Trader(n int, ico, name string) Rec {
	return Rec{ID: 900_000_000 + int64(n), ICO: ico, Name: name, LegalCode: "101",
		LegalForm: "Podnikateľ-fyzická osoba-nezapísaný v obchodnom registri", Established: "2021-05-01", Street: "Živnostenská",
		RegNumber: int64(n), PSC: "98888", Municipality: "Fiktívna Obec", RegisterCode: "2", RegisterName: "Živnostenský register",
		Office: "Okresný úrad Fiktívny", Number: "999-" + strconv.Itoa(n), RegFrom: "2021-05-01"}
}

// Bucket imitates the S3-compatible RPO bucket: ListObjectsV2 (two keys per page, to exercise paging)
// and GET with Range.
type Bucket struct {
	mu      sync.Mutex
	objects map[string][]byte
	Gets    map[string]int // GET count per key
	// FailNext makes the next GET of a key fail after writing that many bytes (simulates a drop).
	FailNext map[string]int
	// Modified overrides the listed LastModified of a key (default: long ago, 2020-01-01).
	Modified map[string]time.Time
}

// NewBucket returns an empty bucket; serve it with net/http (it is an http.Handler).
func NewBucket() *Bucket {
	return &Bucket{objects: map[string][]byte{}, Gets: map[string]int{}, FailNext: map[string]int{}, Modified: map[string]time.Time{}}
}

// Put stores an object.
func (b *Bucket) Put(key string, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[key] = data
}

// Delete removes an object.
func (b *Bucket) Delete(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.objects, key)
}

// PutDaily stores batch-daily/actual_<date>.json.gz.
func (b *Bucket) PutDaily(date string, recs []Rec) {
	b.Put("batch-daily/actual_"+date+".json.gz", File(date, recs))
}

// PutSnapshot stores a snapshot split into parts of partSize records, and its manifest.
func (b *Bucket) PutSnapshot(date string, recs []Rec, partSize int) {
	var names []string
	for i, part := 0, 1; i < len(recs); i, part = i+partSize, part+1 {
		end := min(i+partSize, len(recs))
		name := fmt.Sprintf("init_%s_%03d.json.gz", date, part)
		b.Put("batch-init/"+name, File(date, recs[i:end]))
		names = append(names, name)
	}
	b.Put("batch-init/init_"+date+"_list.txt", []byte(strings.Join(names, "\n")+"\n"))
}

type listEntry struct {
	Key          string `xml:"Key"`
	Size         int    `xml:"Size"`
	LastModified string `xml:"LastModified"`
}

type listXML struct {
	XMLName               xml.Name    `xml:"ListBucketResult"`
	Contents              []listEntry `xml:"Contents"`
	IsTruncated           bool        `xml:"IsTruncated"`
	NextContinuationToken string      `xml:"NextContinuationToken,omitempty"`
}

// ServeHTTP answers ListObjectsV2 and GET with Range, like the RPO bucket.
func (b *Bucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.URL.Path == "/" && r.URL.Query().Get("list-type") == "2" {
		prefix := r.URL.Query().Get("prefix")
		var keys []string
		for k := range b.objects {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		start, _ := strconv.Atoi(r.URL.Query().Get("continuation-token"))
		end := min(start+2, len(keys))
		var out listXML
		for _, k := range keys[start:end] {
			mod, ok := b.Modified[k]
			if !ok {
				mod = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
			}
			out.Contents = append(out.Contents, listEntry{k, len(b.objects[k]), mod.UTC().Format("2006-01-02T15:04:05.000Z")})
		}
		if end < len(keys) {
			out.IsTruncated, out.NextContinuationToken = true, strconv.Itoa(end)
		}
		w.Header().Set("Content-Type", "application/xml")
		_ = xml.NewEncoder(w).Encode(out)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/")
	data, ok := b.objects[key]
	if !ok {
		http.NotFound(w, r)
		return
	}
	b.Gets[key]++
	if n, fail := b.FailNext[key]; fail {
		delete(b.FailNext, key)
		// write a truncated body and drop the connection
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data[:n])
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				_ = c.Close()
			}
		}
		return
	}
	http.ServeContent(w, r, key, time.Time{}, bytes.NewReader(data))
}
