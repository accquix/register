// Package api serves the read-only HTTP API (architecture v1, D17–D27).
package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accquix/register/internal/demo"
	"github.com/accquix/register/internal/lookup"
	"github.com/accquix/register/internal/normalize"
	"github.com/accquix/register/internal/search"
)

// APIVersion is the contract version in the OpenAPI document.
const APIVersion = "1.0.0"

// Config holds the serve settings.
type Config struct {
	Version         string // binary version, reported by /v1/capabilities
	Keys            Keys
	AllowAnonymous  bool
	Fuzzy           bool
	RatePerSecond   float64
	RateBurst       int
	FuzzyEvery      time.Duration
	FuzzyConcurrent int
	FuzzyTimeout    time.Duration
	QueryTimeout    time.Duration
	Search          search.Config
	// MCP serves the read-only MCP adapter at /mcp (REGISTER_MCP, D36).
	MCP bool
	// MCPAllowedOrigins are browser origins allowed to call /mcp; empty refuses every
	// cross-origin browser request (REGISTER_MCP_ALLOWED_ORIGINS).
	MCPAllowedOrigins []string
	// MaxInFlight is how many data requests run at once (REGISTER_MAX_IN_FLIGHT, D39). 0 means
	// the size of the connection pool (10 when the pool is unknown).
	MaxInFlight int
	// AdmitWait is how long a request waits for a free slot before 503 overloaded
	// (REGISTER_ADMIT_WAIT, 250 ms).
	AdmitWait time.Duration
}

// Server holds the dependencies of the handlers.
type Server struct {
	cfg      Config
	pool     *pgxpool.Pool
	store    *lookup.Store
	searcher *search.Searcher
	keys     Keys
	limiter  *limiter
	fuzzyLim *limiter
	fuzzySem chan struct{}
	slots    chan struct{}
	log      *slog.Logger
}

// New builds the HTTP handler. pool may be nil only to render the OpenAPI document.
func New(pool *pgxpool.Pool, cfg Config, log *slog.Logger) (http.Handler, huma.API) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.FuzzyConcurrent < 1 {
		cfg.FuzzyConcurrent = 1
	}
	if cfg.FuzzyEvery <= 0 {
		cfg.FuzzyEvery = 5 * time.Second
	}
	if cfg.RatePerSecond <= 0 {
		cfg.RatePerSecond, cfg.RateBurst = 20, 40
	}
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = 10
		if pool != nil {
			cfg.MaxInFlight = int(pool.Config().MaxConns)
		}
	}
	if cfg.AdmitWait <= 0 {
		cfg.AdmitWait = DefaultAdmitWait
	}
	s := &Server{
		cfg: cfg, pool: pool, keys: cfg.Keys, log: log,
		store:    &lookup.Store{Pool: pool, Now: time.Now},
		searcher: search.New(pool, cfg.Search),
		limiter:  newLimiter(cfg.RatePerSecond, cfg.RateBurst),
		fuzzyLim: newLimiter(1/cfg.FuzzyEvery.Seconds(), 2),
		fuzzySem: make(chan struct{}, cfg.FuzzyConcurrent),
		slots:    make(chan struct{}, cfg.MaxInFlight),
	}
	router := chi.NewMux()
	router.Use(s.accessLog, s.authenticate, s.admission)
	hc := huma.DefaultConfig("Accquix Register API", APIVersion)
	hc.Info.Description = "Search and lookup of Slovak business register data (RPO and the Financial Administration lists). " +
		"Search proposes; only an exact identifier resolves. Data: RPO © Ministerstvo vnútra SR, CC BY 4.0; Zdroj údajov: Finančná správa SR."
	hc.DocsPath = ""     // no docs UI: it would load scripts from a CDN
	hc.CreateHooks = nil // no $schema links in bodies
	hc.Components.SecuritySchemes = map[string]*huma.SecurityScheme{"bearer": {Type: "http", Scheme: "bearer"}}
	hc.Security = []map[string][]string{{"bearer": {}}}
	hc.Servers = []*huma.Server{{URL: "http://localhost:8080", Description: "Your instance (docker compose default)"}}
	hc.Tags = []*huma.Tag{
		{Name: "Search", Description: "Search by name or identifier. Search proposes; resolve with a lookup."},
		{Name: "Lookup", Description: "Subjects by IČO, DIČ or IČ DPH, with sections, states and versions."},
		{Name: "Service", Description: "Data freshness and what this instance offers."},
	}
	api := humachi.New(router, hc)
	s.register(api)
	router.Get("/livez", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	router.Get("/readyz", s.readyz)
	if cfg.MCP {
		router.Handle(MCPPath, s.mcpHandler(router))
	}
	return router, api
}

// ---------- types ----------

// Card is one search result.
type Card struct {
	ICO          string `json:"ico,omitempty"`
	Name         string `json:"name" doc:"Display name, with diacritics and legal form"`
	MatchedName  string `json:"matchedName,omitempty" doc:"The official name that matched, when it is not the display name"`
	Municipality string `json:"municipality,omitempty"`
	// The same record and order as the lookup's identity section, so both show one address.
	Address *lookup.Address `json:"address,omitempty" doc:"The registered seat or place of business, as published in RPO"`
	Kind    string          `json:"kind" enum:"legal_person,sole_trader,public_body,other"`
	Active  bool            `json:"active"`
	Typos   int             `json:"typos,omitempty" doc:"fuzzy only: corrections needed"`
}

type SearchInput struct {
	Q     string `query:"q" required:"true" minLength:"1" maxLength:"200" doc:"Name, IČO, DIČ or IČ DPH"`
	Mode  string `query:"mode" enum:"default,fuzzy" default:"default" doc:"fuzzy corrects typos; explicit only, may be disabled (see /v1/capabilities)"`
	Limit int    `query:"limit" minimum:"1" maximum:"20" default:"10"`
}

type SearchBody struct {
	Mode        string   `json:"mode"`
	Results     []Card   `json:"results"`
	Corrections []string `json:"corrections,omitempty" doc:"fuzzy: corrected words"`
	DataAsOf    string   `json:"dataAsOf,omitempty" doc:"As-of date of the RPO data the search layer is built from"`
	Attribution []string `json:"attribution"`
}

type SearchOutput struct{ Body SearchBody }

type LookupInput struct {
	ID string `path:"id" maxLength:"20" doc:"IČO (8 digits), DIČ (10 digits) or IČ DPH (SK + 10 digits)"`
}

type LookupBody struct {
	Type        string           `json:"type" enum:"ico,dic,ic_dph"`
	Subjects    []lookup.Subject `json:"subjects" doc:"One subject; several for a VAT group (IČ DPH)"`
	Taxpayer    *lookup.Taxpayer `json:"taxpayer,omitempty" doc:"A natural person without an IČO, found by DIČ (authenticated callers only)"`
	Attribution []string         `json:"attribution"`
}

type LookupOutput struct{ Body LookupBody }

type BatchInput struct {
	Body struct {
		IDs    []string `json:"ids" minItems:"1" maxItems:"100"`
		Fields string   `json:"fields,omitempty" enum:"all,versions" default:"all" doc:"versions returns only each section's state and version"`
	}
}

type SectionVersion struct {
	State   string `json:"state"`
	Version string `json:"version,omitempty"`
}

type BatchItem struct {
	ID       string                      `json:"id"`
	Status   int                         `json:"status" doc:"HTTP status this id would get from /v1/lookup/{id}"`
	Code     string                      `json:"code,omitempty"`
	Result   *LookupBody                 `json:"result,omitempty"`
	Versions []map[string]SectionVersion `json:"versions,omitempty" doc:"Per subject: section → state and version"`
}

type BatchBody struct {
	Items       []BatchItem `json:"items"`
	Attribution []string    `json:"attribution"`
}

type BatchOutput struct{ Body BatchBody }

type SourcesBody struct {
	Sources       []lookup.SourceInfo `json:"sources"`
	Bootstrapping bool                `json:"bootstrapping" doc:"true until every source is first published"`
	SetupPending  bool                `json:"setupPending" doc:"true until the operator confirms the objections of this installation; the data endpoints answer 503 reason=setup"`
	Demo          bool                `json:"demo" doc:"true on a demo database: fictional subjects, not register data (accquix-register demo)"`
	Licences      map[string]string   `json:"licences"`
	Attribution   []string            `json:"attribution"`
}

type SourcesOutput struct{ Body SourcesBody }

type CapabilitiesBody struct {
	APIVersion     string `json:"apiVersion"`
	Version        string `json:"version"`
	Anonymous      bool   `json:"anonymous" doc:"Anonymous access is enabled"`
	TaxpayerLookup bool   `json:"taxpayerLookup" doc:"Lookup of natural persons without an IČO by DIČ"`
	Search         struct {
		Modes    []string `json:"modes"`
		LimitMax int      `json:"limitMax"`
	} `json:"search"`
	Fuzzy struct {
		Enabled     bool   `json:"enabled"`
		PerKeyEvery string `json:"perKeyEvery,omitempty"`
		Concurrency int    `json:"concurrencyPerInstance,omitempty"`
		Timeout     string `json:"timeout,omitempty"`
		Note        string `json:"note"`
	} `json:"fuzzy"`
	Rate struct {
		PerSecond float64 `json:"perSecond"`
		Burst     int     `json:"burst"`
	} `json:"rate" doc:"Per caller, per instance"`
	MCP struct {
		Enabled bool   `json:"enabled"`
		Path    string `json:"path,omitempty"`
	} `json:"mcp" doc:"The read-only MCP adapter for AI agents"`
	MaxInFlight int  `json:"maxInFlight" doc:"Data requests this instance runs at once; beyond that, after a short wait, 503 with reason overloaded"`
	Demo        bool `json:"demo" doc:"true on a demo database: every answer holds fictional subjects, not register data"`
}

type CapabilitiesOutput struct{ Body CapabilitiesBody }

// ---------- handlers ----------

func (s *Server) register(api huma.API) {
	errs := []int{400, 401, 429, 503}
	huma.Register(api, huma.Operation{OperationID: "search", Tags: []string{"Search"}, Method: http.MethodGet, Path: "/v1/search", Summary: "Search by name or identifier",
		Description: "One box: an identifier gives an exact match; anything else is a name search (diacritics and word order ignored, " +
			"start of any word matched). mode=fuzzy corrects typos and is rate-limited. Search proposes; resolve with /v1/lookup/{id}.",
		Errors: errs}, s.search)
	huma.Register(api, huma.Operation{OperationID: "lookup", Tags: []string{"Lookup"}, Method: http.MethodGet, Path: "/v1/lookup/{id}", Summary: "Look up by IČO, DIČ or IČ DPH",
		Description: "Sections identity (RPO), tax, vat and risk (Financial Administration lists), each with state present/absent/unknown, " +
			"source as-of date and a content version. 404 only when every consulted dataset is fresh.",
		Errors: []int{400, 401, 403, 404, 429, 503}}, s.lookup)
	huma.Register(api, huma.Operation{OperationID: "lookupBatch", Tags: []string{"Lookup"}, Method: http.MethodPost, Path: "/v1/lookup:batch", Summary: "Look up up to 100 identifiers",
		Description: "fields=versions returns only the section versions, for daily change checks.", Errors: errs}, s.batch)
	huma.Register(api, huma.Operation{OperationID: "sources", Tags: []string{"Service"}, Method: http.MethodGet, Path: "/v1/sources", Summary: "Data freshness per source",
		Errors: []int{401, 503}}, s.sources)
	huma.Register(api, huma.Operation{OperationID: "capabilities", Tags: []string{"Service"}, Method: http.MethodGet, Path: "/v1/capabilities", Summary: "What this instance offers",
		Errors: []int{401}}, s.capabilities)
}

// ready is what a data endpoint needs to answer: the sources, and the attribution to show.
type ready struct {
	srcs        map[string]lookup.SourceInfo
	attribution []string
}

func (s *Server) sourcesReady(ctx context.Context) (ready, error) {
	srcs, err := s.store.Sources(ctx)
	if err != nil {
		return ready{}, unavailable("database", "the database is unavailable")
	}
	if missing := lookup.Bootstrapping(srcs); len(missing) > 0 {
		return ready{}, unavailable("bootstrapping", "the mirror is still being built")
	}
	ok, isDemo, err := s.store.Setup(ctx)
	if err != nil {
		return ready{}, unavailable("database", "the database is unavailable")
	} else if !ok {
		return ready{}, unavailable("setup", setupDetail)
	}
	return ready{srcs: srcs, attribution: attribution(isDemo)}, nil
}

// attribution is the sources' attribution, or the demo notice on a demo database (D41).
func attribution(isDemo bool) []string {
	if isDemo {
		return demo.Attribution
	}
	return lookup.Attribution
}

const setupDetail = "the instance is not set up: the operator has not confirmed its objections (suppress --restore or suppress --init-empty), or the search layer is being rebuilt after an upgrade (done by the next sync pass)"

func dbError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return unavailable("timeout", "the query took too long")
	}
	var p *Problem
	if errors.As(err, &p) {
		return err
	}
	return unavailable("database", "the database is unavailable")
}

func (s *Server) search(ctx context.Context, in *SearchInput) (*SearchOutput, error) {
	rd, err := s.sourcesReady(ctx)
	if err != nil {
		return nil, err
	}
	out := &SearchOutput{Body: SearchBody{Mode: in.Mode, Results: []Card{}, DataAsOf: rd.srcs["rpo"].PublishedAsOf, Attribution: rd.attribution}}
	if kind, id := normalize.Classify(in.Q); kind != normalize.KindText {
		ctx, cancel := context.WithTimeout(ctx, s.cfg.QueryTimeout)
		defer cancel()
		cards, err := s.identifierCards(ctx, kind, id)
		if err != nil {
			return nil, dbError(err)
		}
		out.Body.Results = cards
		return out, nil
	}
	core, _ := normalize.SplitName(in.Q)
	var res search.Result
	if in.Mode == "fuzzy" {
		if !s.cfg.Fuzzy {
			return nil, problem(http.StatusBadRequest, "fuzzy_disabled", "fuzzy search is disabled on this instance")
		}
		if ok, wait := s.fuzzyLim.allow(principal(ctx).Bucket); !ok {
			return nil, withRetryAfter(problem(http.StatusTooManyRequests, "rate_limited", "fuzzy search rate exceeded"), wait)
		}
		select {
		case s.fuzzySem <- struct{}{}:
			defer func() { <-s.fuzzySem }()
		default:
			return nil, withRetryAfter(problem(http.StatusTooManyRequests, "rate_limited", "fuzzy search is busy"), time.Second)
		}
		ctx, cancel := context.WithTimeout(ctx, s.cfg.FuzzyTimeout)
		defer cancel()
		res, err = s.searcher.Fuzzy(ctx, core, in.Limit)
	} else {
		ctx, cancel := context.WithTimeout(ctx, s.cfg.QueryTimeout)
		defer cancel()
		res, err = s.searcher.Default(ctx, core, in.Limit)
	}
	if err != nil {
		return nil, dbError(err)
	}
	cards, err := s.cards(ctx, res.Hits)
	if err != nil {
		return nil, dbError(err)
	}
	out.Body.Results, out.Body.Corrections = cards, res.Fixes
	return out, nil
}

// cards turns hits into result cards with the subject's display name.
func (s *Server) cards(ctx context.Context, hits []search.Hit) ([]Card, error) {
	out := []Card{}
	if len(hits) == 0 {
		return out, nil
	}
	keys := make([]string, len(hits))
	for i, h := range hits {
		keys[i] = h.SKey
	}
	rows, err := s.pool.Query(ctx, `SELECT n.skey, coalesce(n.ico, ''), n.name, coalesce(n.municipality, ''), n.kind FROM search_name n
WHERE n.skey = ANY($1) AND n.is_primary AND `+search.NotSuppressed, keys)
	if err != nil {
		return nil, err
	}
	type prim struct{ ico, name, muni, kind string }
	var outKeys []string
	byKey := map[string]prim{}
	for rows.Next() {
		var k string
		var p prim
		if err := rows.Scan(&k, &p.ico, &p.name, &p.muni, &p.kind); err != nil {
			rows.Close()
			return nil, err
		}
		byKey[k] = p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, h := range hits {
		p, ok := byKey[h.SKey]
		if !ok {
			continue
		}
		c := Card{ICO: p.ico, Name: p.name, Municipality: p.muni, Kind: p.kind, Active: true, Typos: h.Typos}
		if h.Name != p.name {
			c.MatchedName = h.Name
		}
		out = append(out, c)
		outKeys = append(outKeys, h.SKey)
	}
	return out, s.addAddresses(ctx, out, outKeys)
}

// addAddresses sets each card's address from its subject's display record, chosen like the
// lookup's identity section: an active record first, then the Commercial or Trade Register,
// then the newest. keys are the cards' subject keys: an IČO, or r<RPO id>.
func (s *Server) addAddresses(ctx context.Context, cards []Card, keys []string) error {
	if len(cards) == 0 {
		return nil
	}
	var icos []string
	var ids []int64
	for _, k := range keys {
		if id, err := strconv.ParseInt(strings.TrimPrefix(k, "r"), 10, 64); strings.HasPrefix(k, "r") && err == nil {
			ids = append(ids, id)
		} else {
			icos = append(icos, k)
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (k) k, coalesce(street, ''), coalesce(building_number, ''),
  coalesce(postal_code, ''), coalesce(municipality, ''), coalesce(country, '')
FROM (SELECT ico AS k, * FROM rpo_record WHERE ico = ANY($1) AND removed_at IS NULL
      UNION ALL
      SELECT 'r' || id, * FROM rpo_record WHERE id = ANY($2) AND removed_at IS NULL) x
ORDER BY k, terminated_on IS NULL DESC, (coalesce(register_code, '') IN ('1', '2')) DESC, src_date DESC, changed_at DESC, id DESC`, icos, ids)
	if err != nil {
		return err
	}
	byKey := map[string]*lookup.Address{}
	for rows.Next() {
		var k string
		var a lookup.Address
		if err := rows.Scan(&k, &a.Street, &a.BuildingNumber, &a.PostalCode, &a.Municipality, &a.Country); err != nil {
			rows.Close()
			return err
		}
		if a != (lookup.Address{}) {
			byKey[k] = &a
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range cards {
		cards[i].Address = byKey[keys[i]]
	}
	return nil
}

// identifierCards answers an identifier typed into the search box: exact, never fuzzy (D20).
func (s *Server) identifierCards(ctx context.Context, kind normalize.Kind, id string) ([]Card, error) {
	var icos []string
	switch kind {
	case normalize.KindICO:
		icos = []string{id}
	case normalize.KindDIC:
		rows, err := s.pool.Query(ctx, `SELECT ico FROM fs_tax_subject WHERE dic = $1 AND ico IS NOT NULL AND absent_since IS NULL`, id)
		if err != nil {
			return nil, err
		}
		if icos, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return nil, err
		}
	case normalize.KindICDPH:
		rows, err := s.pool.Query(ctx, `SELECT DISTINCT ico FROM fs_vat_payer WHERE ic_dph = $1 AND ico IS NOT NULL AND absent_since IS NULL ORDER BY ico`, id)
		if err != nil {
			return nil, err
		}
		if icos, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return nil, err
		}
	}
	out := []Card{}
	var keys []string
	for _, ico := range icos {
		var c Card
		var term *time.Time
		err := s.pool.QueryRow(ctx, `SELECT name, coalesce(municipality, ''), kind, terminated_on FROM rpo_record
WHERE ico = $1 AND removed_at IS NULL AND name IS NOT NULL
ORDER BY terminated_on IS NULL DESC, (coalesce(register_code, '') IN ('1', '2')) DESC, src_date DESC, id DESC LIMIT 1`, ico).
			Scan(&c.Name, &c.Municipality, &c.Kind, &term)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		c.ICO, c.Active = ico, term == nil
		out = append(out, c)
		keys = append(keys, ico)
	}
	return out, s.addAddresses(ctx, out, keys)
}

// lookupOne resolves one identifier. It returns a Problem for 4xx/5xx outcomes.
func (s *Server) lookupOne(ctx context.Context, rd ready, raw string) (*LookupBody, error) {
	srcs := rd.srcs
	kind, id := normalize.Classify(raw)
	body := &LookupBody{Type: kind.String(), Subjects: []lookup.Subject{}, Attribution: rd.attribution}
	switch kind {
	case normalize.KindICO:
		sub, found, err := s.store.Subject(ctx, srcs, id)
		if err != nil {
			return nil, dbError(err)
		}
		if !found {
			return nil, s.notFound(srcs, lookup.AllSources...)
		}
		body.Subjects = append(body.Subjects, sub)
	case normalize.KindDIC:
		sub, tp, err := s.store.ByDIC(ctx, srcs, id)
		if err != nil {
			return nil, dbError(err)
		}
		switch {
		case sub != nil:
			body.Subjects = append(body.Subjects, *sub)
		case tp != nil:
			if s.cfg.AllowAnonymous {
				return nil, problem(http.StatusForbidden, "forbidden", "lookup of natural persons without an IČO is unavailable on an instance with anonymous access")
			}
			body.Taxpayer = tp
		default:
			return nil, s.notFound(srcs, "fs:ds_dsrdp")
		}
	case normalize.KindICDPH:
		subs, err := s.store.ByICDPH(ctx, srcs, id, !s.cfg.AllowAnonymous)
		if err != nil {
			return nil, dbError(err)
		}
		if len(subs) == 0 {
			return nil, s.notFound(srcs, "fs:ds_dphs")
		}
		body.Subjects = subs
	default:
		return nil, problem(http.StatusBadRequest, "invalid_identifier", "want an IČO (8 digits), a DIČ (10 digits) or an IČ DPH (SK + 10 digits)")
	}
	return body, nil
}

// notFound is 404 only when every consulted dataset is fresh; otherwise 503 (never fabricate certainty).
func (s *Server) notFound(srcs map[string]lookup.SourceInfo, consulted ...string) error {
	if lookup.Fresh(srcs, consulted...) {
		return problem(http.StatusNotFound, "not_found", "no subject with this identifier")
	}
	return unavailable("source_stale", "not found, but these sources are not fresh: "+lookup.StaleNames(srcs, consulted...))
}

func (s *Server) lookup(ctx context.Context, in *LookupInput) (*LookupOutput, error) {
	rd, err := s.sourcesReady(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.QueryTimeout)
	defer cancel()
	body, err := s.lookupOne(ctx, rd, in.ID)
	if err != nil {
		return nil, err
	}
	return &LookupOutput{Body: *body}, nil
}

func versions(sub lookup.Subject) map[string]SectionVersion {
	return map[string]SectionVersion{
		"identity": {sub.Identity.State, sub.Identity.Source.Version},
		"tax":      {sub.Tax.State, sub.Tax.Source.Version},
		"vat":      {sub.VAT.State, sub.VAT.Source.Version},
		"risk":     {sub.Risk.State, sub.Risk.Source.Version},
	}
}

func (s *Server) batch(ctx context.Context, in *BatchInput) (*BatchOutput, error) {
	rd, err := s.sourcesReady(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*s.cfg.QueryTimeout)
	defer cancel()
	out := &BatchOutput{Body: BatchBody{Items: make([]BatchItem, 0, len(in.Body.IDs)), Attribution: rd.attribution}}
	for _, id := range in.Body.IDs {
		item := BatchItem{ID: id, Status: http.StatusOK}
		body, err := s.lookupOne(ctx, rd, id)
		var p *Problem
		switch {
		case errors.As(err, &p):
			item.Status, item.Code = p.Status, p.Code
		case err != nil:
			return nil, dbError(err)
		case in.Body.Fields == "versions":
			for _, sub := range body.Subjects {
				item.Versions = append(item.Versions, versions(sub))
			}
		default:
			item.Result = body
		}
		out.Body.Items = append(out.Body.Items, item)
	}
	return out, nil
}

func (s *Server) sources(ctx context.Context, _ *struct{}) (*SourcesOutput, error) {
	srcs, err := s.store.Sources(ctx)
	if err != nil {
		return nil, unavailable("database", "the database is unavailable")
	}
	confirmed, isDemo, err := s.store.Setup(ctx)
	if err != nil {
		return nil, unavailable("database", "the database is unavailable")
	}
	body := SourcesBody{Bootstrapping: len(lookup.Bootstrapping(srcs)) > 0, SetupPending: !confirmed, Demo: isDemo, Attribution: attribution(isDemo),
		Licences: map[string]string{"rpo": "CC BY 4.0 (Ministerstvo vnútra SR)", "fs": "Zdroj údajov: Finančná správa SR"}}
	if isDemo {
		body.Licences = map[string]string{"demo": "fictional data generated by Accquix Register; no licence of a register applies"}
	}
	for _, name := range lookup.AllSources {
		body.Sources = append(body.Sources, srcs[name])
	}
	sort.SliceStable(body.Sources, func(a, b int) bool { return body.Sources[a].Source < body.Sources[b].Source })
	return &SourcesOutput{Body: body}, nil
}

func (s *Server) capabilities(ctx context.Context, _ *struct{}) (*CapabilitiesOutput, error) {
	var b CapabilitiesBody
	if s.pool != nil {
		if _, isDemo, err := s.store.Setup(ctx); err == nil {
			b.Demo = isDemo
		}
	}
	b.APIVersion, b.Version, b.Anonymous, b.TaxpayerLookup = APIVersion, s.cfg.Version, s.cfg.AllowAnonymous, !s.cfg.AllowAnonymous
	b.Search.Modes, b.Search.LimitMax = []string{"default"}, 20
	b.Fuzzy.Enabled = s.cfg.Fuzzy
	b.Fuzzy.Note = "Fuzzy search is optional, potentially CPU-intensive, and never substitutes exact identifier matching."
	if s.cfg.Fuzzy {
		b.Search.Modes = append(b.Search.Modes, "fuzzy")
		b.Fuzzy.PerKeyEvery, b.Fuzzy.Concurrency, b.Fuzzy.Timeout = s.cfg.FuzzyEvery.String(), s.cfg.FuzzyConcurrent, s.cfg.FuzzyTimeout.String()
	}
	b.Rate.PerSecond, b.Rate.Burst = s.cfg.RatePerSecond, s.cfg.RateBurst
	b.MaxInFlight = s.cfg.MaxInFlight
	if s.cfg.MCP {
		b.MCP.Enabled, b.MCP.Path = true, MCPPath
	}
	return &CapabilitiesOutput{Body: b}, nil
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	srcs, err := s.store.Sources(ctx)
	if err != nil {
		writeProblem(w, unavailable("database", "the database is unavailable"), 0)
		return
	}
	if missing := lookup.Bootstrapping(srcs); len(missing) > 0 {
		writeProblem(w, unavailable("bootstrapping", fmt.Sprintf("not yet published: %v", missing)), 0)
		return
	}
	ok, isDemo, err := s.store.Setup(ctx)
	if err != nil {
		writeProblem(w, unavailable("database", "the database is unavailable"), 0)
		return
	} else if !ok {
		writeProblem(w, unavailable("setup", setupDetail), 0)
		return
	}
	if isDemo {
		_, _ = w.Write([]byte("ready (demo data: fictional subjects)\n"))
		return
	}
	_, _ = w.Write([]byte("ready\n"))
}
