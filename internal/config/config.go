// Package config reads the configuration from environment variables only.
// Every secret NAME can instead be given as NAME_FILE, a path to a file holding the value.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Config is the process configuration. Later stages add fields here.
type Config struct {
	// DatabaseURL is the PostgreSQL connection string (REGISTER_DATABASE_URL, secret).
	DatabaseURL string
	// MigrateDatabaseURL is used by the migrate mode (REGISTER_MIGRATE_DATABASE_URL, secret).
	// It lets operators run migrations with a DDL role; when unset, DatabaseURL is used.
	MigrateDatabaseURL string
	// LogLevel is REGISTER_LOG_LEVEL: debug, info (default), warn or error.
	LogLevel slog.Level
}

// Env abstracts the process environment for tests.
type Env struct {
	Getenv   func(string) string
	ReadFile func(string) ([]byte, error)
}

// OSEnv is the real environment.
var OSEnv = Env{Getenv: os.Getenv, ReadFile: os.ReadFile}

// ErrBothSet is returned when both NAME and NAME_FILE are set.
var ErrBothSet = errors.New("both the variable and its _FILE variant are set")

// Secret returns the value of name, or the content of the file named by name_FILE.
// Setting both is an error, so that a stale value never silently wins.
func (e Env) Secret(name string) (string, error) {
	v, f := e.Getenv(name), e.Getenv(name+"_FILE")
	switch {
	case v != "" && f != "":
		return "", fmt.Errorf("%s: %w", name, ErrBothSet)
	case f != "":
		b, err := e.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", name, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	default:
		return v, nil
	}
}

// Load reads the configuration. It does not check which fields a mode needs; each mode does.
func Load(e Env) (Config, error) {
	var c Config
	var err error
	if c.DatabaseURL, err = e.Secret("REGISTER_DATABASE_URL"); err != nil {
		return c, err
	}
	if c.MigrateDatabaseURL, err = e.Secret("REGISTER_MIGRATE_DATABASE_URL"); err != nil {
		return c, err
	}
	if c.MigrateDatabaseURL == "" {
		c.MigrateDatabaseURL = c.DatabaseURL
	}
	switch lvl := strings.ToLower(e.Getenv("REGISTER_LOG_LEVEL")); lvl {
	case "", "info":
		c.LogLevel = slog.LevelInfo
	case "debug":
		c.LogLevel = slog.LevelDebug
	case "warn":
		c.LogLevel = slog.LevelWarn
	case "error":
		c.LogLevel = slog.LevelError
	default:
		return c, fmt.Errorf("REGISTER_LOG_LEVEL: unknown level %q", lvl)
	}
	return c, nil
}
