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
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/accquix/register/internal/config"
	"github.com/accquix/register/internal/migrate"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

const usage = `usage: accquix-register <mode> [args]

modes:
  migrate [up|down-to N|status]   apply or inspect the embedded schema migrations (default: up)
  serve                           read-only HTTP API (not implemented yet)
  sync [--source NAME]            download and merge the sources (not implemented yet)
  openapi                         print the OpenAPI 3.1 document (not implemented yet)
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
	case "serve", "sync", "openapi":
		err = fmt.Errorf("mode %q is not implemented yet", mode)
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
