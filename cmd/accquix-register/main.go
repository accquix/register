// Command accquix-register is the single binary of Accquix Register.
//
//	accquix-register migrate [up|down-to N|status]
//	accquix-register serve
//	accquix-register sync [--source NAME]
//	accquix-register openapi
//	accquix-register version
package main

import (
	"context"
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
	"github.com/accquix/register/internal/migrate"
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
  sync [--source rpo|fs|all]      one pass over the sources (default: all)
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
			Log:              log,
		}).Run(ctx))
	}
	if src == "fs" || src == "all" {
		errs = append(errs, syncer.NewFS(conn, syncer.FSConfig{
			Client: fs.NewClient(cfg.FSBaseURL), WorkDir: cfg.WorkDir, MaxDrop: cfg.FSMaxDrop, Log: log,
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
