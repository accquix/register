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
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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
  suppress --subject ICO [--undo] [--reason TEXT]
                                  keep a subject out of name search (GDPR objection)
  suppress --list                 print every objection (the backup format) to standard output
  suppress --restore FILE         put a backup made with --list back ("-" reads standard input)
  sync --unblock --source S       after a validation block (S: rpo or fs:<list>)
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
			Log: log,
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
	log.Info("serving", "listen", cfg.Listen, "keys", len(keys), "anonymous", cfg.AllowAnonymous, "fuzzy", cfg.SearchFuzzy)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
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
	subject := fset.String("subject", "", "the subject's IČO (or r<RPO id> for a record without one)")
	undo := fset.Bool("undo", false, "put the subject back into name search")
	reason := fset.String("reason", "", "operator note, e.g. a ticket number; never personal data")
	list := fset.Bool("list", false, "print every objection in the backup format")
	restore := fset.String("restore", "", "restore objections from a file written by --list (- for standard input)")
	if err := fset.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	modes := 0
	for _, on := range []bool{*subject != "", *list, *restore != ""} {
		if on {
			modes++
		}
	}
	if modes != 1 || ((*list || *restore != "") && (*undo || *reason != "")) {
		return fmt.Errorf("%w: suppress: --subject needs an IČO (8 digits) or r<RPO id>; alone, use --list or --restore FILE", errUsage)
	}
	key := strings.TrimSpace(*subject)
	if *subject != "" {
		if ico, ok := normalize.ICO(key); ok {
			key = ico
		} else if !ingest.ValidSuppressionKey(key) {
			return fmt.Errorf("%w: --subject needs an IČO (8 digits) or r<RPO id>", errUsage)
		}
	}
	var backup []ingest.Suppression
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
		if backup, err = readSuppressions(in); err != nil {
			return err
		}
	}
	if cfg.SyncDatabaseURL == "" {
		return errors.New("REGISTER_SYNC_DATABASE_URL or REGISTER_DATABASE_URL is required")
	}
	conn, err := pgx.Connect(ctx, cfg.SyncDatabaseURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))
	switch {
	case *list:
		rows, err := ingest.ListSuppressions(ctx, conn)
		if err != nil {
			return err
		}
		return writeSuppressions(stdout, rows)
	case *restore != "":
		added, err := ingest.RestoreSuppressions(ctx, conn, backup)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "restored: %d objections in the file, %d added\n", len(backup), added)
		return nil
	}
	if err := ingest.Suppress(ctx, conn, key, *reason, !*undo); err != nil {
		return err
	}
	if *undo {
		fmt.Fprintln(stdout, "back in name search")
	} else {
		fmt.Fprintln(stdout, "kept out of name search; exact lookup is unaffected")
	}
	return nil
}

// The backup format of the objections: one tab-separated line per row (key, origin, created at,
// reason), then a trailer with the row count and the SHA-256 of the lines before it. The trailer
// lets a restore refuse a truncated or altered file. Keep the file private and encrypted: it lists
// the people who objected.
const suppressionTrailer = "#accquix-register-objections"

func suppressionLine(s ingest.Suppression) string {
	clean := strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace
	return strings.Join([]string{s.SKey, s.Origin, s.CreatedAt.UTC().Format(time.RFC3339Nano), clean(s.Reason)}, "\t") + "\n"
}

func writeSuppressions(w io.Writer, rows []ingest.Suppression) error {
	h := sha256.New()
	for _, s := range rows {
		line := suppressionLine(s)
		h.Write([]byte(line))
		if _, err := io.WriteString(w, line); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "%s\t%d\t%s\n", suppressionTrailer, len(rows), hex.EncodeToString(h.Sum(nil)))
	return err
}

func readSuppressions(r io.Reader) ([]ingest.Suppression, error) {
	var out []ingest.Suppression
	h := sha256.New()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		f := strings.Split(line, "\t")
		if f[0] == suppressionTrailer {
			if len(f) != 3 || f[1] != strconv.Itoa(len(out)) || f[2] != hex.EncodeToString(h.Sum(nil)) {
				return nil, errors.New("restore: the file is damaged or incomplete (count or checksum mismatch)")
			}
			if sc.Scan() {
				return nil, errors.New("restore: data after the trailer")
			}
			return out, sc.Err()
		}
		if len(f) != 4 {
			return nil, fmt.Errorf("restore: line %d is not in the backup format", len(out)+1)
		}
		at, err := time.Parse(time.RFC3339Nano, f[2])
		if err != nil || !ingest.ValidSuppressionKey(f[0]) || (f[1] != "" && !ingest.ValidSuppressionKey(f[1])) {
			return nil, fmt.Errorf("restore: line %d is not in the backup format", len(out)+1)
		}
		h.Write([]byte(line + "\n"))
		out = append(out, ingest.Suppression{SKey: f[0], Origin: f[1], CreatedAt: at, Reason: f[3]})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("restore: the file has no trailer; it is incomplete")
}
