package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/accquix/register/internal/config"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accquix/register/internal/ingest"
	"github.com/accquix/register/internal/migrate"
	"github.com/accquix/register/internal/testkit"
)

func emptyEnv() config.Env {
	return config.Env{Getenv: func(string) string { return "" }, ReadFile: func(string) ([]byte, error) { return nil, nil }}
}

func TestRunExitCodes(t *testing.T) {
	cases := []struct {
		args []string
		code int
		out  string
	}{
		{nil, 2, "usage:"},
		{[]string{"version"}, 0, "dev"},
		{[]string{"help"}, 0, "modes:"},
		{[]string{"nope"}, 2, "unknown mode"},
		{[]string{"serve"}, 1, "no API keys"},
		{[]string{"openapi"}, 0, "openapi: 3.1"},
		{[]string{"sync"}, 1, "REGISTER_SYNC_DATABASE_URL or REGISTER_DATABASE_URL is required"},
		{[]string{"sync", "--source", "nope"}, 2, "unknown source"},
		{[]string{"suppress"}, 2, "--subject needs"},
		{[]string{"suppress", "--subject", "rx"}, 2, "--subject needs"},
		{[]string{"sync", "--unblock", "--source", "fs"}, 2, "--unblock needs"},
		{[]string{"sync", "--unblock", "--source", "fs:ds_iban"}, 2, "--unblock needs"},
		{[]string{"migrate"}, 1, "REGISTER_MIGRATE_DATABASE_URL or REGISTER_DATABASE_URL is required"},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		code := run(context.Background(), c.args, &out, &errb, emptyEnv())
		if code != c.code {
			t.Errorf("%v: exit %d, want %d (stderr %q)", c.args, code, c.code, errb.String())
		}
		if !strings.Contains(out.String()+errb.String(), c.out) {
			t.Errorf("%v: output %q does not contain %q", c.args, out.String()+errb.String(), c.out)
		}
	}
}

func TestRunMigrateBadArgs(t *testing.T) {
	env := config.Env{
		Getenv: func(k string) string {
			if k == "REGISTER_DATABASE_URL" {
				return "postgres://unused"
			}
			return ""
		},
		ReadFile: func(string) ([]byte, error) { return nil, nil },
	}
	for _, args := range [][]string{{"migrate", "down-to"}, {"migrate", "down-to", "x"}, {"migrate", "sideways"}} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), args, &out, &errb, env); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

// The committed api/openapi.yaml must match the generated document (regenerate with
// `go run ./cmd/accquix-register openapi > api/openapi.yaml`).
func TestOpenAPIUpToDate(t *testing.T) {
	var out bytes.Buffer
	if err := runOpenAPI(&out); err != nil {
		t.Fatal(err)
	}
	committed, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), committed) {
		t.Fatal("api/openapi.yaml is out of date: run `go run ./cmd/accquix-register openapi > api/openapi.yaml`")
	}
}

func TestObjectionBackupRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 10, 9, 30, 0, 123456000, time.UTC)
	revoked := at.Add(time.Hour)
	rows := []ingest.Objection{
		{ID: 1, Key: "99900004", Reason: "ticket 12\twith a tab", CreatedAt: at, Links: []ingest.Link{
			{RecordID: 900000004, Basis: ingest.BasisAtObjection, CreatedAt: at},
			{RecordID: 900000005, Basis: ingest.BasisCarriedICO, Source: "rpo:actual_2026-11-09.json.gz", CreatedAt: at, ReleasedAt: &revoked, ReleasedReason: "note\twith\ta tab"},
		}},
		{ID: 2, Key: "r900000006", CreatedAt: at, RevokedAt: &revoked},
	}
	var buf bytes.Buffer
	if err := writeObjections(&buf, rows); err != nil {
		t.Fatal(err)
	}
	got, err := readObjections(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Key != "99900004" || got[0].Reason != "ticket 12 with a tab" || !got[0].CreatedAt.Equal(at) ||
		len(got[0].Links) != 2 || got[0].Links[0].Basis != ingest.BasisAtObjection || got[0].Links[1].RecordID != 900000005 ||
		got[0].Links[1].Source != "rpo:actual_2026-11-09.json.gz" || got[0].Links[1].ReleasedAt == nil || !got[0].Links[1].ReleasedAt.Equal(revoked) ||
		got[0].Links[1].ReleasedReason != "note\twith\ta tab" || got[0].RevokedAt != nil ||
		got[1].Key != "r900000006" || got[1].RevokedAt == nil || !got[1].RevokedAt.Equal(revoked) || len(got[1].Links) != 0 {
		t.Fatalf("round trip: %+v", got)
	}
	file := buf.String()
	lines := strings.SplitAfter(file, "\n")
	for name, damaged := range map[string]string{
		"no trailer":  lines[0] + lines[1],
		"missing row": lines[0] + lines[2],
		"changed key": strings.Replace(file, "99900004\t", "99900005\t", 1),
		"bad key":     "not-a-key\t" + at.Format(time.RFC3339Nano) + "\t-\t[]\t\n" + lines[2],
		"bad basis":   strings.Replace(file, "at_objection", "whatever", 1),
		"extra data":  file + "99900004\tx\n",
	} {
		if _, err := readObjections(strings.NewReader(damaged)); err == nil {
			t.Errorf("%s: a damaged backup was accepted", name)
		}
	}
	// a long reason does not make the file unrestorable
	long := []ingest.Objection{{Key: "99900004", Reason: strings.Repeat("x", 2<<20), CreatedAt: at}}
	buf.Reset()
	if err := writeObjections(&buf, long); err != nil {
		t.Fatal(err)
	}
	if got, err := readObjections(&buf); err != nil || len(got[0].Reason) != 2<<20 {
		t.Fatalf("long reason: %v", err)
	}
}

// A backup can only grow: an export never replaces a file that holds an objection the database
// does not have (an empty or rebuilt database over the only good copy).
func TestExportNeverDestroysABackup(t *testing.T) {
	at := time.Date(2026, 11, 10, 9, 30, 0, 0, time.UTC)
	first := ingest.Objection{Key: "99900004", CreatedAt: at}
	second := ingest.Objection{Key: "r900000006", CreatedAt: at}
	path := filepath.Join(t.TempDir(), "objections.tsv")
	if err := exportObjections(path, false, []ingest.Objection{first}); err != nil {
		t.Fatal(err)
	}
	good, _ := os.ReadFile(path)
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("the backup must be private, got %v", st.Mode().Perm())
	}
	for name, rows := range map[string][]ingest.Objection{
		"no --overwrite":                 {first, second},
		"an empty database":              nil,
		"a database without the old row": {second},
	} {
		if err := exportObjections(path, name != "no --overwrite", rows); err == nil {
			t.Fatalf("%s: the backup was replaced", name)
		}
		if now, _ := os.ReadFile(path); !bytes.Equal(now, good) {
			t.Fatalf("%s: a refused export changed the file", name)
		}
	}
	if err := exportObjections(path, true, []ingest.Objection{first, second}); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(path)
	defer f.Close()
	if got, err := readObjections(f); err != nil || len(got) != 2 {
		t.Fatalf("after --overwrite: %v %v", got, err)
	}
	// a file that is not a backup is never replaced either
	junk := filepath.Join(filepath.Dir(path), "notes.txt")
	if err := os.WriteFile(junk, []byte("not a backup\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := exportObjections(junk, true, []ingest.Objection{first}); err == nil {
		t.Fatal("a file that is not a backup was replaced")
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".objections-*")); len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
}

// TestDemoCommand: demo migrates and loads an empty database; sync then refuses it, so real and
// fictional data never mix (D41).
func TestDemoCommand(t *testing.T) {
	dsn := testkit.Database(t)
	env := config.Env{Getenv: func(k string) string {
		if k == "REGISTER_DATABASE_URL" {
			return dsn
		}
		return ""
	}, ReadFile: os.ReadFile}
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"demo"}, &out, &errb, env); code != 0 {
		t.Fatalf("demo: exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "fictional subjects") {
		t.Fatalf("demo output: %q", out.String())
	}
	errb.Reset()
	if code := run(context.Background(), []string{"sync", "--source", "fs"}, &out, &errb, env); code != 1 || !strings.Contains(errb.String(), "demo data") {
		t.Fatalf("sync on a demo database: exit %d: %s", code, errb.String())
	}
}

// TestServeRefusesAnOldSchema: serve stops at start, with the fix in the message, instead of
// answering every request with "database unavailable" (found on a real test database).
func TestServeRefusesAnOldSchema(t *testing.T) {
	ctx := context.Background()
	dsn := testkit.Database(t)
	latest, err := migrate.Latest()
	if err != nil || latest < 4 {
		t.Fatalf("latest migration %d: %v", latest, err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	log := slog.New(slog.DiscardHandler)
	if err := checkSchema(ctx, pool, log); err != nil {
		t.Fatalf("current schema refused: %v", err)
	}
	if _, err := migrate.DownTo(ctx, dsn, latest-1); err != nil {
		t.Fatal(err)
	}
	if err := checkSchema(ctx, pool, log); err == nil || !strings.Contains(err.Error(), "run `accquix-register migrate` first") {
		t.Fatalf("old schema: %v", err)
	}
}
