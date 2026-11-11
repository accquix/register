// Command accquix-register is the single binary of Accquix Register.
//
//	accquix-register migrate [up|down-to N|status]
//	accquix-register serve
//	accquix-register sync [--source NAME]
//	accquix-register openapi
//	accquix-register version
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/accquix/register/internal/api"
	"github.com/accquix/register/internal/config"
	"github.com/accquix/register/internal/ingest"
	"github.com/accquix/register/internal/migrate"
	"github.com/accquix/register/internal/normalize"
	"github.com/accquix/register/internal/search"
	"github.com/accquix/register/internal/sources/fs"
	"github.com/accquix/register/internal/sources/rpo"
	"github.com/accquix/register/internal/syncer"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

const usage = `usage: accquix-register <mode> [args]

modes:
  migrate [up|down-to N|status]   apply or inspect the embedded schema migrations (default: up)
  serve                           read-only HTTP API
  sync [--source rpo|fs|all] [--every D]  one pass over the sources (default: all); with
                                  --every (e.g. 1h) it repeats until stopped
  suppress --subject ICO [--reason TEXT]
                                  record an objection: keep a subject out of name search
  suppress --undo --id N          revoke one objection (or --undo --subject ICO when it has one)
  suppress --list                 print every objection with its id
  suppress --export --output FILE [--overwrite]
                                  write the backup of the objections
  suppress --restore FILE         put a backup back ("-" reads standard input)
  suppress --init-empty           confirm a new installation with no objections to restore
  sync --unblock --source S       after a validation block (S: rpo or fs:<list>)
  mcp --url URL                   MCP over stdio for a local AI client, forwarding to the REST API of
                                  the instance at URL (key: REGISTER_MCP_KEY or REGISTER_MCP_KEY_FILE)
  mcp-tools                       print the MCP tool list (JSON)
  openapi                         print the OpenAPI 3.1 document (YAML)
  version                         print the version

configuration: environment variables only; see docs/architecture.md`

// errUsage marks a command-line mistake (exit code 2).
var errUsage = errors.New("usage")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, config.OSEnv)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, env config.Env) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	mode, rest := args[0], args[1:]
	switch mode {
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, version)
		return 0
	case "help", "--help", "-h":
		fmt.Fprintln(stdout, usage)
		return 0
	}

	cfg, err := config.Load(env)
	if err != nil {
		fmt.Fprintln(stderr, "configuration:", err)
		return 1
	}
	log := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	switch mode {
	case "migrate":
		err = runMigrate(ctx, cfg, rest, stdout, log)
	case "sync":
		err = runSync(ctx, cfg, rest, log)
	case "serve":
		err = runServe(ctx, cfg, log)
	case "suppress":
		err = runSuppress(ctx, cfg, rest, os.Stdin, stdout)
	case "mcp":
		err = runMCP(ctx, cfg, rest, os.Stdin, stdout, log)
	case "mcp-tools":
		var b []byte
		if b, err = api.MCPToolsJSON(); err == nil {
			_, err = stdout.Write(b)
		}
	case "openapi":
		err = runOpenAPI(stdout)
	default:
		err = fmt.Errorf("%w: unknown mode %q", errUsage, mode)
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		fmt.Fprintln(stderr, err)
		fmt.Fprintln(stderr, usage)
		return 2
	default:
		log.Error("failed", "mode", mode, "error", err)
		return 1
	}
}

func runMigrate(ctx context.Context, cfg config.Config, args []string, stdout io.Writer, log *slog.Logger) error {
	if cfg.MigrateDatabaseURL == "" {
		return errors.New("REGISTER_MIGRATE_DATABASE_URL or REGISTER_DATABASE_URL is required")
	}
	sub := "up"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "up":
		n, err := migrate.Up(ctx, cfg.MigrateDatabaseURL)
		if err != nil {
			return err
		}
		log.Info("migrations applied", "count", n)
		roles := migrate.Roles{API: cfg.RoleAPI, Sync: cfg.RoleSync, Operator: cfg.RoleOperator}
		if err := migrate.Grant(ctx, cfg.MigrateDatabaseURL, roles); err != nil {
			return fmt.Errorf("grants: %w", err)
		}
		if roles.Any() {
			log.Info("privileges applied", "api", roles.API != "", "sync", roles.Sync != "", "operator", roles.Operator != "")
		}
		return nil
	case "down-to":
		if len(args) != 2 {
			return fmt.Errorf("%w: migrate down-to needs a version", errUsage)
		}
		v, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || v < 0 {
			return fmt.Errorf("%w: bad version %q", errUsage, args[1])
		}
		n, err := migrate.DownTo(ctx, cfg.MigrateDatabaseURL, v)
		if err != nil {
			return err
		}
		log.Info("migrations rolled back", "count", n, "to", v)
		return nil
	case "status":
		st, err := migrate.Statuses(ctx, cfg.MigrateDatabaseURL)
		if err != nil {
			return err
		}
		for _, s := range st {
			state := "pending"
			if s.Applied {
				state = "applied"
			}
			fmt.Fprintf(stdout, "%05d  %-8s %s\n", s.Version, state, s.Source)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown migrate command %q", errUsage, sub)
	}
}

func runSync(ctx context.Context, cfg config.Config, args []string, log *slog.Logger) error {
	fset := flag.NewFlagSet("sync", flag.ContinueOnError)
	fset.SetOutput(io.Discard)
	source := fset.String("source", "all", "rpo, fs or all; with --unblock: rpo or fs:<list>")
	unblock := fset.Bool("unblock", false, "restore the state before a validation block, then stop")
	every := fset.Duration("every", 0, "repeat the pass at this interval until stopped (e.g. 1h)")
	if err := fset.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	src := *source
	if *unblock {
		_, isList := fs.ListBySlug(strings.TrimPrefix(src, "fs:"))
		if src != "rpo" && (!strings.HasPrefix(src, "fs:") || !isList) {
			return fmt.Errorf("%w: --unblock needs --source rpo or fs:<list> (lists: %s)", errUsage, strings.Join(fs.Slugs(), ", "))
		}
	} else if src != "rpo" && src != "fs" && src != "all" {
		return fmt.Errorf("%w: unknown source %q (rpo, fs or all)", errUsage, src)
	}
	if cfg.SyncDatabaseURL == "" {
		return errors.New("REGISTER_SYNC_DATABASE_URL or REGISTER_DATABASE_URL is required")
	}
	conn, err := pgx.Connect(ctx, cfg.SyncDatabaseURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))
	if *unblock {
		if err := syncer.Unblock(ctx, conn, src); err != nil {
			return err
		}
		log.Info("unblocked", "source", src)
		return nil
	}
	if *every > 0 {
		if *every < time.Minute {
			return fmt.Errorf("%w: --every must be at least 1m", errUsage)
		}
		for {
			if err := syncPass(ctx, conn, cfg, src, log); err != nil {
				// keep going: a blocked source or a network error must not stop the schedule
				log.Error("sync pass failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(*every):
			}
		}
	}
	return syncPass(ctx, conn, cfg, src, log)
}

func syncPass(ctx context.Context, conn *pgx.Conn, cfg config.Config, src string, log *slog.Logger) error {
	if o := cfg.Overrides(); len(o) > 0 {
		log.Warn("validation settings differ from the defaults (operator override)", "settings", strings.Join(o, " "))
	}
	var errs []error
	if src == "rpo" || src == "all" {
		errs = append(errs, syncer.NewRPO(conn, syncer.RPOConfig{
			Bucket:           rpo.NewBucket(cfg.RPOBucketURL),
			WorkDir:          cfg.WorkDir,
			Concurrency:      cfg.DownloadConcurrency,
			PartRecords:      cfg.RPOPartRecords,
			DailyMaxRecords:  cfg.RPODailyMaxRecords,
			SnapshotMinRatio: cfg.RPOSnapshotMinRatio,
			AbsenceMaxRatio:  cfg.AbsenceMaxRatio,
			DateSkewDays:     cfg.RPODateSkewDays, BootstrapMinRecords: cfg.RPOBootstrapMinRecords,
			SnapshotSettle: cfg.RPOSnapshotSettle,
			Log:            log,
		}).Run(ctx))
	}
	if src == "fs" || src == "all" {
		errs = append(errs, syncer.NewFS(conn, syncer.FSConfig{
			Client: fs.NewClient(cfg.FSBaseURL), WorkDir: cfg.WorkDir, MaxDrop: cfg.FSMaxDrop, SkipMinRows: cfg.FSSkipMinRows, Log: log,
		}).Run(ctx))
	}
	for i, e := range errs {
		if errors.Is(e, syncer.ErrLocked) {
			log.Info("another sync is running; nothing to do")
			errs[i] = nil
		}
	}
	return errors.Join(errs...)
}

func apiConfig(cfg config.Config, keys api.Keys) api.Config {
	return api.Config{
		Version: version, Keys: keys, AllowAnonymous: cfg.AllowAnonymous, Fuzzy: cfg.SearchFuzzy,
		RatePerSecond: cfg.RatePerSecond, RateBurst: cfg.RateBurst, FuzzyEvery: cfg.FuzzyEvery,
		MCP: cfg.MCP, MCPAllowedOrigins: cfg.MCPAllowedOrigins,
		FuzzyConcurrent: cfg.FuzzyConcurrent, FuzzyTimeout: cfg.FuzzyTimeout, QueryTimeout: cfg.QueryTimeout,
		Search: search.Config{WordStepMax: cfg.WordStepMax, WordCap: cfg.WordCap},
	}
}

func runServe(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	keys, err := api.ParseKeys(cfg.APIKeys)
	if err != nil {
		return err
	}
	if len(keys) == 0 && !cfg.AllowAnonymous {
		return errors.New("no API keys: set REGISTER_API_KEYS (name:sha256hex,...) or explicitly REGISTER_ALLOW_ANONYMOUS=true")
	}
	if cfg.DatabaseURL == "" {
		return errors.New("REGISTER_DATABASE_URL is required")
	}
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()
	handler, _ := api.New(pool, apiConfig(cfg, keys), log)
	srv := &http.Server{Addr: cfg.Listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("serving", "listen", cfg.Listen, "keys", len(keys), "anonymous", cfg.AllowAnonymous, "fuzzy", cfg.SearchFuzzy, "mcp", cfg.MCP)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// runMCP serves the MCP tools over stdio for a local client (Claude Desktop and similar). Every
// tool call goes to the REST API of the instance at --url; this process never sees the database.
// stdout carries the protocol only; logs go to stderr.
func runMCP(ctx context.Context, cfg config.Config, args []string, stdin io.Reader, stdout io.Writer, log *slog.Logger) error {
	fset := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fset.SetOutput(io.Discard)
	base := fset.String("url", "", "base URL of the instance, e.g. https://register.example.internal")
	if err := fset.Parse(args); err != nil || fset.NArg() > 0 {
		return fmt.Errorf("%w: mcp --url URL", errUsage)
	}
	u, err := url.Parse(*base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("%w: mcp: --url must be an http(s) URL", errUsage)
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		log.Warn("mcp: the API key is sent over plain http to a non-local host; use https")
	}
	call := api.HTTPCaller(*base, cfg.MCPKey, &http.Client{Timeout: 30 * time.Second})
	status, _, body, err := call(ctx, "/v1/capabilities", nil)
	if err != nil {
		return fmt.Errorf("mcp: cannot reach the instance: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("mcp: the instance answered %d to /v1/capabilities (is REGISTER_MCP_KEY set?)", status)
	}
	var caps api.CapabilitiesBody
	if err := json.Unmarshal(body, &caps); err != nil {
		return fmt.Errorf("mcp: unexpected /v1/capabilities answer: %w", err)
	}
	srv := api.NewMCPServer(call, api.MCPOptions{Version: caps.Version, Fuzzy: caps.Fuzzy.Enabled, Log: log})
	log.Info("mcp: serving over stdio", "instance", u.Host, "version", caps.Version)
	return srv.Run(ctx, &mcp.IOTransport{Reader: io.NopCloser(stdin), Writer: nopWriteCloser{stdout}})
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func runOpenAPI(stdout io.Writer) error {
	_, a := api.New(nil, api.Config{Version: "openapi", Fuzzy: true}, nil)
	b, err := a.OpenAPI().YAML()
	if err != nil {
		return err
	}
	_, err = stdout.Write(b)
	return err
}

func runSuppress(ctx context.Context, cfg config.Config, args []string, stdin io.Reader, stdout io.Writer) error {
	fset := flag.NewFlagSet("suppress", flag.ContinueOnError)
	fset.SetOutput(io.Discard)
	subject := fset.String("subject", "", "the subject's IČO (or r<RPO id> for one record)")
	reason := fset.String("reason", "", "operator note, e.g. a ticket number; never personal data")
	undo := fset.Bool("undo", false, "revoke an objection: with --id, or with --subject when it has exactly one")
	id := fset.Int64("id", 0, "with --undo: the objection to revoke (see --list)")
	list := fset.Bool("list", false, "print every objection")
	export := fset.Bool("export", false, "write the backup of the objections to --output")
	output := fset.String("output", "", "with --export: the file to write (never overwritten without --overwrite)")
	overwrite := fset.Bool("overwrite", false, "with --export: replace an existing file")
	restore := fset.String("restore", "", "restore objections from a backup (- for standard input)")
	initEmpty := fset.Bool("init-empty", false, "confirm a new installation with no objections to restore")
	if err := fset.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	modes := 0
	for _, on := range []bool{*subject != "" && !*undo, *undo, *list, *export, *restore != "", *initEmpty} {
		if on {
			modes++
		}
	}
	switch {
	case modes != 1 || fset.NArg() > 0:
		return fmt.Errorf("%w: suppress: --subject needs an IČO (8 digits) or r<RPO id>; or use one of --undo, --list, --export, --restore, --init-empty", errUsage)
	case *undo && (*id > 0) == (*subject != ""):
		return fmt.Errorf("%w: suppress --undo needs --id N or --subject", errUsage)
	case *export && *output == "":
		return fmt.Errorf("%w: suppress --export needs --output FILE", errUsage)
	}
	key := strings.TrimSpace(*subject)
	if key != "" {
		if ico, ok := normalize.ICO(key); ok {
			key = ico
		} else if !ingest.ValidObjectionKey(key) {
			return fmt.Errorf("%w: --subject needs an IČO (8 digits) or r<RPO id>", errUsage)
		}
	}
	var backup []ingest.Objection
	if *restore != "" {
		in := stdin
		if *restore != "-" {
			f, err := os.Open(*restore)
			if err != nil {
				return err
			}
			defer f.Close()
			in = f
		}
		var err error
		if backup, err = readObjections(in); err != nil {
			return err
		}
	}
	if cfg.OperatorDatabaseURL == "" {
		return errors.New("REGISTER_OPERATOR_DATABASE_URL, REGISTER_SYNC_DATABASE_URL or REGISTER_DATABASE_URL is required")
	}
	conn, err := pgx.Connect(ctx, cfg.OperatorDatabaseURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))
	switch {
	case *list:
		rows, err := ingest.ListObjections(ctx, conn)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, "id\tsubject\trecorded\trevoked\tanchored records\treason")
		for _, o := range rows {
			revoked := "-"
			if o.RevokedAt != nil {
				revoked = o.RevokedAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(stdout, "%d\t%s\t%s\t%s\t%d\t%s\n", o.ID, o.Key, o.CreatedAt.UTC().Format(time.RFC3339), revoked, len(o.Anchors), o.Reason)
		}
		return nil
	case *export:
		rows, err := ingest.ListObjections(ctx, conn)
		if err != nil {
			return err
		}
		if err := exportObjections(*output, *overwrite, rows); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "exported %d objections to %s; keep the file encrypted and away from the database\n", len(rows), *output)
		return nil
	case *restore != "":
		added, err := ingest.RestoreObjections(ctx, conn, backup)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "restored: %d objections in the file, %d added; the installation is confirmed\n", len(backup), added)
		return nil
	case *initEmpty:
		if err := ingest.ConfirmNoObjections(ctx, conn); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "confirmed: a new installation with no objections to restore")
		return nil
	case *undo:
		if *id == 0 {
			ids, err := ingest.ActiveObjections(ctx, conn, key)
			if err != nil {
				return err
			}
			if len(ids) != 1 {
				return fmt.Errorf("%s has %d active objections %v: name one with --undo --id N", key, len(ids), ids)
			}
			*id = ids[0]
		}
		still, err := ingest.RevokeObjection(ctx, conn, *id)
		if err != nil {
			return err
		}
		if len(still) > 0 {
			fmt.Fprintf(stdout, "objection #%d revoked; the subject is STILL kept out of name search by objection(s) %v\n", *id, still)
		} else {
			fmt.Fprintf(stdout, "objection #%d revoked; back in name search\n", *id)
		}
		return nil
	}
	oid, records, err := ingest.AddObjection(ctx, conn, key, *reason)
	if err != nil {
		return err
	}
	if records == 0 {
		fmt.Fprintf(stdout, "objection #%d recorded; no record matches %s yet, it applies when one appears. Check the identifier\n", oid, key)
	} else {
		fmt.Fprintf(stdout, "objection #%d recorded; %d record(s) kept out of name search; exact lookup is unaffected\n", oid, records)
	}
	return nil
}

// The backup of the objections: one tab-separated line per objection (key, recorded at, revoked at
// or "-", anchored record ids, reason), then a trailer with the row count and the SHA-256 of the
// lines before it. The trailer detects a truncated or damaged file. It does not prove who wrote
// the file: anyone who can change the file can recompute it. Keep the file encrypted and with
// restricted access: it lists the people who objected.
const objectionTrailer = "#accquix-register-objections-v2"

func objectionLine(o ingest.Objection) string {
	clean := strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace
	revoked := "-"
	if o.RevokedAt != nil {
		revoked = o.RevokedAt.UTC().Format(time.RFC3339Nano)
	}
	anchors := make([]string, len(o.Anchors))
	for i, a := range o.Anchors {
		anchors[i] = strconv.FormatInt(a, 10)
	}
	return strings.Join([]string{o.Key, o.CreatedAt.UTC().Format(time.RFC3339Nano), revoked, strings.Join(anchors, ","), clean(o.Reason)}, "\t") + "\n"
}

func writeObjections(w io.Writer, rows []ingest.Objection) error {
	h := sha256.New()
	for _, o := range rows {
		line := objectionLine(o)
		h.Write([]byte(line))
		if _, err := io.WriteString(w, line); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "%s\t%d\t%s\n", objectionTrailer, len(rows), hex.EncodeToString(h.Sum(nil)))
	return err
}

// exportObjections writes the backup atomically: to a temporary file in the same directory, synced,
// then renamed. An existing file is replaced only with overwrite, and never by an empty backup:
// that is what a rebuilt, empty database would produce, over the only good copy.
func exportObjections(path string, overwrite bool, rows []ingest.Objection) error {
	if _, err := os.Stat(path); err == nil {
		if !overwrite {
			return fmt.Errorf("%s exists; pass --overwrite to replace it", path)
		}
		if len(rows) == 0 {
			return fmt.Errorf("the database has no objections; refusing to replace %s with an empty backup", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".objections-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // the temporary name; the backup has its own
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := writeObjections(tmp, rows); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if !overwrite {
		// Link, not rename: it fails if the file appeared meanwhile.
		return os.Link(tmp.Name(), path)
	}
	return os.Rename(tmp.Name(), path)
}

func readObjections(r io.Reader) ([]ingest.Objection, error) {
	var out []ingest.Objection
	h := sha256.New()
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errors.New("restore: the file has no trailer; it is incomplete")
			}
			return nil, err
		}
		f := strings.Split(strings.TrimSuffix(line, "\n"), "\t")
		if f[0] == objectionTrailer {
			if len(f) != 3 || f[1] != strconv.Itoa(len(out)) || f[2] != hex.EncodeToString(h.Sum(nil)) {
				return nil, errors.New("restore: the file is damaged or incomplete (count or checksum mismatch)")
			}
			if rest, _ := io.ReadAll(br); len(strings.TrimSpace(string(rest))) > 0 {
				return nil, errors.New("restore: data after the trailer")
			}
			return out, nil
		}
		bad := fmt.Errorf("restore: line %d is not in the backup format", len(out)+1)
		if len(f) != 5 || !ingest.ValidObjectionKey(f[0]) {
			return nil, bad
		}
		o := ingest.Objection{Key: f[0], Reason: f[4]}
		if o.CreatedAt, err = time.Parse(time.RFC3339Nano, f[1]); err != nil {
			return nil, bad
		}
		if f[2] != "-" {
			at, err := time.Parse(time.RFC3339Nano, f[2])
			if err != nil {
				return nil, bad
			}
			o.RevokedAt = &at
		}
		if f[3] != "" {
			for _, a := range strings.Split(f[3], ",") {
				n, err := strconv.ParseInt(a, 10, 64)
				if err != nil || n <= 0 {
					return nil, bad
				}
				o.Anchors = append(o.Anchors, n)
			}
		}
		h.Write([]byte(line))
		out = append(out, o)
	}
}
