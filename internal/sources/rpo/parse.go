package rpo

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Decoder streams the records of one RPO file.
type Decoder struct {
	dec        *json.Decoder
	gz         *gzip.Reader
	guard      *guard
	maxRecords int
	exportDate time.Time
	row        Row
	n          int
	err        error
	done       bool
}

// Limits bound what one file may cost before it is rejected. The source is trusted to be the
// state's, not to be well-formed: a damaged or hostile file must end as a rejected unit, never as
// a full disk or an out-of-memory kill. The defaults leave ample room over the real files
// (docs/sources.md: a snapshot part decompresses to about 0.5–1.2 GB, a daily file to tens of MB).
type Limits struct {
	Decompressed int64 // bytes of JSON after gunzip; 0 = DefaultLimits.Decompressed
	Record       int64 // bytes of one record; 0 = DefaultLimits.Record
	Records      int   // records in the file; 0 = no limit here (the unit sets it)
}

// DefaultLimits are used by NewDecoder.
var DefaultLimits = Limits{Decompressed: 8 << 30, Record: 64 << 20}

// ErrTooLarge marks a file or a record over a limit.
var ErrTooLarge = errors.New("over the size limit")

// guard counts the decompressed bytes, in total and since the current record began.
type guard struct {
	r                    io.Reader
	total, max           int64
	sinceRecord, maxItem int64
}

func (g *guard) Read(p []byte) (int, error) {
	n, err := g.r.Read(p)
	g.total += int64(n)
	g.sinceRecord += int64(n)
	// Over a limit, the bytes are withheld: a decoder that got its data would keep parsing what
	// it has buffered and might never ask again, so the error would be lost.
	switch {
	case g.total > g.max:
		return 0, fmt.Errorf("%w: more than %d bytes after decompression", ErrTooLarge, g.max)
	case g.sinceRecord > g.maxItem:
		return 0, fmt.Errorf("%w: one record of more than %d bytes", ErrTooLarge, g.maxItem)
	}
	return n, err
}

// NewDecoder reads the gzip stream r up to the first record, with DefaultLimits.
func NewDecoder(r io.Reader) (*Decoder, error) { return NewDecoderLimits(r, Limits{}) }

// NewDecoderLimits reads the gzip stream r up to the first record. It fails if the header is not
// {"exportDate": "YYYY-MM-DD", "results": [.
func NewDecoderLimits(r io.Reader, lim Limits) (*Decoder, error) {
	if lim.Decompressed <= 0 {
		lim.Decompressed = DefaultLimits.Decompressed
	}
	if lim.Record <= 0 {
		lim.Record = DefaultLimits.Record
	}
	gz, err := gzip.NewReader(bufio.NewReaderSize(r, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	g := &guard{r: gz, max: lim.Decompressed, maxItem: lim.Record}
	d := &Decoder{dec: json.NewDecoder(g), gz: gz, guard: g, maxRecords: lim.Records}
	if err := d.expectDelim('{'); err != nil {
		return nil, err
	}
	if err := d.expectKey("exportDate"); err != nil {
		return nil, err
	}
	var ds string
	if err := d.dec.Decode(&ds); err != nil {
		return nil, fmt.Errorf("exportDate: %w", err)
	}
	if d.exportDate, err = time.Parse("2006-01-02", ds); err != nil {
		return nil, fmt.Errorf("exportDate %q: %w", ds, err)
	}
	if err := d.expectKey("results"); err != nil {
		return nil, err
	}
	if err := d.expectDelim('['); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *Decoder) expectDelim(want json.Delim) error {
	t, err := d.dec.Token()
	if err != nil {
		return fmt.Errorf("header: %w", err)
	}
	if got, ok := t.(json.Delim); !ok || got != want {
		return fmt.Errorf("header: expected %q, got %v", want, t)
	}
	return nil
}

func (d *Decoder) expectKey(want string) error {
	t, err := d.dec.Token()
	if err != nil {
		return fmt.Errorf("header: %w", err)
	}
	if got, ok := t.(string); !ok || got != want {
		return fmt.Errorf("header: expected key %q, got %v", want, t)
	}
	return nil
}

// SetMaxRecords makes the decoder fail once the file holds more than n records, so that an
// oversized file is rejected while it is read, not after all of it was staged.
func (d *Decoder) SetMaxRecords(n int) { d.maxRecords = n }

// ExportDate is the date the ministry stamped on the file.
func (d *Decoder) ExportDate() time.Time { return d.exportDate }

// Next decodes the next record. It returns false at the end of the array or on an error (see Err).
func (d *Decoder) Next() bool {
	if d.done || d.err != nil {
		return false
	}
	if !d.dec.More() {
		d.done = true
		// consume "]" and "}" so that a truncated file is reported as an error
		if err := d.expectDelim(']'); err != nil {
			d.err = err
		} else if err := d.expectDelim('}'); err != nil {
			d.err = err
		}
		return false
	}
	if d.maxRecords > 0 && d.n >= d.maxRecords {
		d.err = fmt.Errorf("%w: more than %d records", ErrTooLarge, d.maxRecords)
		return false
	}
	d.guard.sinceRecord = 0
	var r record
	if err := d.dec.Decode(&r); err != nil {
		d.err = fmt.Errorf("record %d: %w", d.n+1, err)
		return false
	}
	if r.ID <= 0 {
		d.err = fmt.Errorf("record %d: missing id", d.n+1)
		return false
	}
	d.row = r.row()
	d.n++
	return true
}

// Row returns the current record.
func (d *Decoder) Row() Row { return d.row }

// Count is the number of records decoded so far.
func (d *Decoder) Count() int { return d.n }

// Err returns the first error, including a truncated or malformed file.
func (d *Decoder) Err() error {
	if d.err != nil && errors.Is(d.err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("truncated file: %w", d.err)
	}
	return d.err
}
