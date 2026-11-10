package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/accquix/register/internal/config"
	"github.com/accquix/register/internal/ingest"
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
		{ID: 1, Key: "99900004", Reason: "ticket 12\twith a tab", CreatedAt: at, Anchors: []int64{900000004, 900000005}},
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
		len(got[0].Anchors) != 2 || got[0].Anchors[1] != 900000005 || got[0].RevokedAt != nil ||
		got[1].Key != "r900000006" || got[1].RevokedAt == nil || !got[1].RevokedAt.Equal(revoked) {
		t.Fatalf("round trip: %+v", got)
	}
	file := buf.String()
	lines := strings.SplitAfter(file, "\n")
	for name, damaged := range map[string]string{
		"no trailer":  lines[0] + lines[1],
		"missing row": lines[0] + lines[2],
		"changed key": strings.Replace(file, "99900004\t", "99900005\t", 1),
		"bad key":     "not-a-key\t" + at.Format(time.RFC3339Nano) + "\t-\t\t\n" + lines[2],
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

func TestExportNeverDestroysABackup(t *testing.T) {
	at := time.Date(2026, 11, 10, 9, 30, 0, 0, time.UTC)
	rows := []ingest.Objection{{Key: "99900004", CreatedAt: at}}
	path := filepath.Join(t.TempDir(), "objections.tsv")
	if err := exportObjections(path, false, rows); err != nil {
		t.Fatal(err)
	}
	good, _ := os.ReadFile(path)
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("the backup must be private, got %v", st.Mode().Perm())
	}
	if err := exportObjections(path, false, rows); err == nil {
		t.Fatal("an existing backup was overwritten without --overwrite")
	}
	if err := exportObjections(path, true, nil); err == nil {
		t.Fatal("an empty backup replaced a good one")
	}
	if now, _ := os.ReadFile(path); !bytes.Equal(now, good) {
		t.Fatal("a refused export changed the file")
	}
	more := []ingest.Objection{rows[0], {Key: "r900000006", CreatedAt: at}}
	if err := exportObjections(path, true, more); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(path)
	defer f.Close()
	if got, err := readObjections(f); err != nil || len(got) != 2 {
		t.Fatalf("after --overwrite: %v %v", got, err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".objections-*")); len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
}
