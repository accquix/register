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
	"time"
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

	// serve
	Listen          string        // REGISTER_LISTEN, ":8080"
	APIKeys         string        // REGISTER_API_KEYS (secret): "name:sha256hex,name2:sha256hex"
	AllowAnonymous  bool          // REGISTER_ALLOW_ANONYMOUS, false
	SearchFuzzy     bool          // REGISTER_SEARCH_FUZZY, true
	RatePerSecond   float64       // REGISTER_RATE_PER_SECOND, 20 (per key, per instance)
	RateBurst       int           // REGISTER_RATE_BURST, 40
	FuzzyEvery      time.Duration // REGISTER_FUZZY_EVERY, 5s: one fuzzy search per key per interval (burst 2)
	FuzzyConcurrent int           // REGISTER_FUZZY_CONCURRENCY, 1 (per instance)
	FuzzyTimeout    time.Duration // REGISTER_FUZZY_TIMEOUT, 2s
	QueryTimeout    time.Duration // REGISTER_QUERY_TIMEOUT, 3s: default search and lookup
	WordStepMax     int           // REGISTER_SEARCH_WORD_STEP_MAX, 1000
	WordCap         int           // REGISTER_SEARCH_WORD_CAP, 1000
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
	if err := c.loadServe(e); err != nil {
		return c, err
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

func boolVar(e Env, name string, def bool) (bool, error) {
	v := strings.ToLower(e.Getenv(name))
	switch v {
	case "":
		return def, nil
	case "true", "1", "yes":
		return true, nil
	case "false", "0", "no":
		return false, nil
	}
	return false, fmt.Errorf("%s: want true or false, got %q", name, v)
}

func durationVar(e Env, name string, def time.Duration) (time.Duration, error) {
	v := e.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: want a duration such as 2s, got %q", name, v)
	}
	return d, nil
}

func (c *Config) loadServe(e Env) error {
	var err error
	if c.Listen = e.Getenv("REGISTER_LISTEN"); c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.APIKeys, err = e.Secret("REGISTER_API_KEYS"); err != nil {
		return err
	}
	if c.AllowAnonymous, err = boolVar(e, "REGISTER_ALLOW_ANONYMOUS", false); err != nil {
		return err
	}
	if c.SearchFuzzy, err = boolVar(e, "REGISTER_SEARCH_FUZZY", true); err != nil {
		return err
	}
	if v := e.Getenv("REGISTER_RATE_PER_SECOND"); v == "" {
		c.RatePerSecond = 20
	} else if c.RatePerSecond, err = strconv.ParseFloat(v, 64); err != nil || c.RatePerSecond <= 0 {
		return fmt.Errorf("REGISTER_RATE_PER_SECOND: want a positive number, got %q", v)
	}
	for _, v := range []struct {
		name string
		dst  *int
		def  int
	}{
		{"REGISTER_RATE_BURST", &c.RateBurst, 40},
		{"REGISTER_FUZZY_CONCURRENCY", &c.FuzzyConcurrent, 1},
		{"REGISTER_SEARCH_WORD_STEP_MAX", &c.WordStepMax, 1000},
		{"REGISTER_SEARCH_WORD_CAP", &c.WordCap, 1000},
	} {
		if *v.dst, err = positiveInt(e.Getenv(v.name), v.def); err != nil {
			return fmt.Errorf("%s: %w", v.name, err)
		}
	}
	for _, v := range []struct {
		name string
		dst  *time.Duration
		def  time.Duration
	}{
		{"REGISTER_FUZZY_EVERY", &c.FuzzyEvery, 5 * time.Second},
		{"REGISTER_FUZZY_TIMEOUT", &c.FuzzyTimeout, 2 * time.Second},
		{"REGISTER_QUERY_TIMEOUT", &c.QueryTimeout, 3 * time.Second},
	} {
		if *v.dst, err = durationVar(e, v.name, v.def); err != nil {
			return err
		}
	}
	return nil
}
