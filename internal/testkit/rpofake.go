// Package testkit holds test helpers: the synthetic RPO and FS data of package synthetic, served
// on test servers, and a throwaway PostgreSQL database.
package testkit

import (
	"net/http/httptest"
	"testing"

	"github.com/accquix/register/internal/synthetic"
)

type (
	// Rec is a synthetic RPO record.
	Rec = synthetic.Rec
	// Item is one synthetic list item: element name → value.
	Item = synthetic.Item
)

var (
	File    = synthetic.File
	Company = synthetic.Company
	Trader  = synthetic.Trader
	FSZip   = synthetic.FSZip
	VATItem = synthetic.VATItem
	TaxItem = synthetic.TaxItem
)

// Bucket is a fake RPO bucket on a test server.
type Bucket struct {
	*synthetic.Bucket
	Server *httptest.Server
}

// NewBucket starts a fake bucket; it is closed when the test ends.
func NewBucket(t *testing.T) *Bucket {
	b := &Bucket{Bucket: synthetic.NewBucket()}
	b.Server = httptest.NewServer(b.Bucket)
	t.Cleanup(b.Server.Close)
	return b
}

// URL is the bucket base URL.
func (b *Bucket) URL() string { return b.Server.URL }

// FSServer is a fake report.financnasprava.sk on a test server.
type FSServer struct {
	*synthetic.FSServer
	Server *httptest.Server
}

// NewFSServer starts the fake; it is closed when the test ends.
func NewFSServer(t *testing.T) *FSServer {
	s := &FSServer{FSServer: synthetic.NewFSServer()}
	s.Server = httptest.NewServer(s.FSServer)
	t.Cleanup(s.Server.Close)
	return s
}

// URL is the base URL.
func (s *FSServer) URL() string { return s.Server.URL }
