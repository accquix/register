package config

import (
	"errors"
	"log/slog"
	"os"
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
