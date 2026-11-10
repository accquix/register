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
	exportDate time.Time
	row        Row
	n          int
	err        error
	done       bool
}

// NewDecoder reads the gzip stream r up to the first record. It fails if the header is not
// {"exportDate": "YYYY-MM-DD", "results": [.
func NewDecoder(r io.Reader) (*Decoder, error) {
	gz, err := gzip.NewReader(bufio.NewReaderSize(r, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	d := &Decoder{dec: json.NewDecoder(gz), gz: gz}
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
