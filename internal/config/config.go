// Package config reads the configuration from environment variables only.
// Every secret NAME can instead be given as NAME_FILE, a path to a file holding the value.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is the process configuration. Later stages add fields here.
type Config struct {
	// DatabaseURL is the PostgreSQL connection string (REGISTER_DATABASE_URL, secret).
	DatabaseURL string
	// MigrateDatabaseURL is used by the migrate mode (REGISTER_MIGRATE_DATABASE_URL, secret).
	// It lets operators run migrations with a DDL role; when unset, DatabaseURL is used.
	MigrateDatabaseURL string
	// SyncDatabaseURL is used by the sync mode (REGISTER_SYNC_DATABASE_URL, secret). It must be a
	// direct connection, not a transaction pooler (advisory lock, COPY). When unset, DatabaseURL.
	SyncDatabaseURL string
	// LogLevel is REGISTER_LOG_LEVEL: debug, info (default), warn or error.
	LogLevel slog.Level

	// RPOBucketURL is the RPO bulk bucket (REGISTER_RPO_BUCKET_URL; default: the official bucket).
	RPOBucketURL string
	// WorkDir holds downloads in progress (REGISTER_WORK_DIR; default: <temp>/accquix-register).
	WorkDir string
	// DownloadConcurrency is the number of parallel snapshot downloads (REGISTER_DOWNLOAD_CONCURRENCY, 6).
	DownloadConcurrency int
	// Validation thresholds (D23).
	RPOPartRecords      int     // REGISTER_RPO_PART_RECORDS, 100000
	RPODailyMaxRecords  int     // REGISTER_RPO_DAILY_MAX_RECORDS, 20000
	RPOSnapshotMinRatio float64 // REGISTER_RPO_SNAPSHOT_MIN_RATIO, 0.98
	AbsenceMaxRatio     float64 // REGISTER_ABSENCE_MAX_RATIO, 0.005
	// FSBaseURL serves the Financial Administration list exports (REGISTER_FS_BASE_URL).
	FSBaseURL string
	// FSMaxDrop overrides every FS list's drop threshold when set (REGISTER_FS_MAX_DROP; default:
	// 0.03 for ds_dsrdp, ds_dphs, ds_iz_ran and 0.20 for the small lists).
	FSMaxDrop float64
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
	if c.SyncDatabaseURL, err = e.Secret("REGISTER_SYNC_DATABASE_URL"); err != nil {
		return c, err
	}
	if c.SyncDatabaseURL == "" {
		c.SyncDatabaseURL = c.DatabaseURL
	}
	c.RPOBucketURL = e.Getenv("REGISTER_RPO_BUCKET_URL")
	if c.RPOBucketURL == "" {
		c.RPOBucketURL = "https://frkqbrydxwdp.compat.objectstorage.eu-frankfurt-1.oraclecloud.com/susr-rpo"
	}
	c.WorkDir = e.Getenv("REGISTER_WORK_DIR")
	if c.WorkDir == "" {
		c.WorkDir = filepath.Join(os.TempDir(), "accquix-register")
	}
	ints := []struct {
		name string
		dst  *int
		def  int
	}{
		{"REGISTER_DOWNLOAD_CONCURRENCY", &c.DownloadConcurrency, 6},
		{"REGISTER_RPO_PART_RECORDS", &c.RPOPartRecords, 100_000},
		{"REGISTER_RPO_DAILY_MAX_RECORDS", &c.RPODailyMaxRecords, 20_000},
	}
	for _, v := range ints {
		if *v.dst, err = positiveInt(e.Getenv(v.name), v.def); err != nil {
			return c, fmt.Errorf("%s: %w", v.name, err)
		}
	}
	floats := []struct {
		name string
		dst  *float64
		def  float64
	}{
		{"REGISTER_RPO_SNAPSHOT_MIN_RATIO", &c.RPOSnapshotMinRatio, 0.98},
		{"REGISTER_ABSENCE_MAX_RATIO", &c.AbsenceMaxRatio, 0.005},
	}
	for _, v := range floats {
		if *v.dst, err = ratio(e.Getenv(v.name), v.def); err != nil {
			return c, fmt.Errorf("%s: %w", v.name, err)
		}
	}
	c.FSBaseURL = e.Getenv("REGISTER_FS_BASE_URL")
	if c.FSBaseURL == "" {
		c.FSBaseURL = "https://report.financnasprava.sk"
	}
	if v := e.Getenv("REGISTER_FS_MAX_DROP"); v != "" {
		if c.FSMaxDrop, err = ratio(v, 0); err != nil {
			return c, fmt.Errorf("REGISTER_FS_MAX_DROP: %w", err)
		}
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

func positiveInt(s string, def int) (int, error) {
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("want a positive integer, got %q", s)
	}
	return n, nil
}

func ratio(s string, def float64) (float64, error) {
	if s == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 || f > 1 {
		return 0, fmt.Errorf("want a number in (0, 1], got %q", s)
	}
	return f, nil
}
