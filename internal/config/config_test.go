package config

import (
	"errors"
	"log/slog"
	"os"
	"slices"
	"testing"
)

func env(vars map[string]string, files map[string]string) Env {
	return Env{
		Getenv: func(k string) string { return vars[k] },
		ReadFile: func(p string) ([]byte, error) {
			if v, ok := files[p]; ok {
				return []byte(v), nil
			}
			return nil, os.ErrNotExist
		},
	}
}

func TestSecretFromValue(t *testing.T) {
	v, err := env(map[string]string{"X": "abc"}, nil).Secret("X")
	if err != nil || v != "abc" {
		t.Fatalf("got (%q, %v)", v, err)
	}
}

func TestSecretFromFileTrimsNewline(t *testing.T) {
	e := env(map[string]string{"X_FILE": "/run/secrets/x"}, map[string]string{"/run/secrets/x": "s3cret\n"})
	v, err := e.Secret("X")
	if err != nil || v != "s3cret" {
		t.Fatalf("got (%q, %v)", v, err)
	}
}

func TestSecretBothSetIsAnError(t *testing.T) {
	e := env(map[string]string{"X": "a", "X_FILE": "/f"}, map[string]string{"/f": "b"})
	if _, err := e.Secret("X"); !errors.Is(err, ErrBothSet) {
		t.Fatalf("want ErrBothSet, got %v", err)
	}
}

func TestSecretMissingFile(t *testing.T) {
	if _, err := env(map[string]string{"X_FILE": "/nope"}, nil).Secret("X"); err == nil {
		t.Fatal("want an error for a missing file")
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{"REGISTER_DATABASE_URL": "postgres://a"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.MigrateDatabaseURL != "postgres://a" || c.LogLevel != slog.LevelInfo {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestLoadMigrateOverrideAndLevel(t *testing.T) {
	c, err := Load(env(map[string]string{
		"REGISTER_DATABASE_URL":         "postgres://api",
		"REGISTER_MIGRATE_DATABASE_URL": "postgres://ddl",
		"REGISTER_LOG_LEVEL":            "DEBUG",
	}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.MigrateDatabaseURL != "postgres://ddl" || c.LogLevel != slog.LevelDebug {
		t.Fatalf("unexpected config: %+v", c)
	}
}

func TestLoadBadLevel(t *testing.T) {
	if _, err := Load(env(map[string]string{"REGISTER_LOG_LEVEL": "loud"}, nil)); err == nil {
		t.Fatal("want an error for an unknown level")
	}
}

func TestLoadSyncDefaultsAndOverrides(t *testing.T) {
	c, err := Load(env(map[string]string{"REGISTER_DATABASE_URL": "postgres://a"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.SyncDatabaseURL != "postgres://a" || c.DownloadConcurrency != 6 || c.RPOPartRecords != 100_000 ||
		c.RPODailyMaxRecords != 20_000 || c.RPOSnapshotMinRatio != 0.98 || c.AbsenceMaxRatio != 0.005 || c.RPOBucketURL == "" {
		t.Fatalf("unexpected sync defaults: %+v", c)
	}
	c, err = Load(env(map[string]string{"REGISTER_SYNC_DATABASE_URL": "postgres://direct", "REGISTER_DOWNLOAD_CONCURRENCY": "2",
		"REGISTER_ABSENCE_MAX_RATIO": "0.01"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.SyncDatabaseURL != "postgres://direct" || c.DownloadConcurrency != 2 || c.AbsenceMaxRatio != 0.01 {
		t.Fatalf("overrides not applied: %+v", c)
	}
}

func TestLoadRejectsBadNumbers(t *testing.T) {
	for k, v := range map[string]string{"REGISTER_DOWNLOAD_CONCURRENCY": "0", "REGISTER_RPO_PART_RECORDS": "x", "REGISTER_ABSENCE_MAX_RATIO": "2"} {
		if _, err := Load(env(map[string]string{k: v}, nil)); err == nil {
			t.Errorf("%s=%s accepted", k, v)
		}
	}
}

// Per-key limits are off by default; anonymous callers keep theirs. A rate without a burst gets
// twice the rate as the burst.
func TestLoadLimits(t *testing.T) {
	c, err := Load(env(map[string]string{"REGISTER_DATABASE_URL": "postgres://a"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.RatePerSecond != 0 || c.RateBurst != 0 || c.FuzzyEvery != 0 ||
		c.AnonRatePerSecond != 20 || c.AnonRateBurst != 40 || c.AnonFuzzyEvery.String() != "5s" || c.FSMaxUnidentified != 0 {
		t.Fatalf("unexpected limit defaults: %+v", c)
	}
	c, err = Load(env(map[string]string{"REGISTER_DATABASE_URL": "postgres://a", "REGISTER_RATE_PER_SECOND": "2.5",
		"REGISTER_FUZZY_EVERY": "10s", "REGISTER_FS_MAX_UNIDENTIFIED": "800"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.RatePerSecond != 2.5 || c.RateBurst != 5 || c.FuzzyEvery.String() != "10s" || c.FSMaxUnidentified != 800 {
		t.Fatalf("limits not applied: %+v", c)
	}
	if !slices.Contains(c.Overrides(), "REGISTER_FS_MAX_UNIDENTIFIED=800") {
		t.Fatalf("the override must be logged: %v", c.Overrides())
	}
	for k, v := range map[string]string{"REGISTER_RATE_PER_SECOND": "0", "REGISTER_ANONYMOUS_RATE_PER_SECOND": "-1", "REGISTER_FS_MAX_UNIDENTIFIED": "x"} {
		if _, err := Load(env(map[string]string{"REGISTER_DATABASE_URL": "postgres://a", k: v}, nil)); err == nil {
			t.Errorf("%s=%s accepted", k, v)
		}
	}
}

// D56: taxpayer name search is off by default, and refused together with anonymous access.
func TestLoadTaxpayerSearch(t *testing.T) {
	c, err := Load(env(map[string]string{"REGISTER_DATABASE_URL": "postgres://a"}, nil))
	if err != nil || c.SearchTaxpayers || c.TaxpayerRatePerSecond != 1 || c.TaxpayerRateBurst != 10 {
		t.Fatalf("defaults: %v %+v", err, c)
	}
	c, err = Load(env(map[string]string{"REGISTER_DATABASE_URL": "postgres://a", "REGISTER_SEARCH_TAXPAYERS": "true",
		"REGISTER_SEARCH_TAXPAYERS_RATE_PER_SECOND": "1", "REGISTER_SEARCH_TAXPAYERS_RATE_BURST": "3"}, nil))
	if err != nil || !c.SearchTaxpayers || c.TaxpayerRatePerSecond != 1 || c.TaxpayerRateBurst != 3 {
		t.Fatalf("on: %v %+v", err, c)
	}
	if _, err := Load(env(map[string]string{"REGISTER_DATABASE_URL": "postgres://a", "REGISTER_SEARCH_TAXPAYERS": "true",
		"REGISTER_ALLOW_ANONYMOUS": "true"}, nil)); err == nil {
		t.Fatal("taxpayer search with anonymous access must be refused")
	}
}
