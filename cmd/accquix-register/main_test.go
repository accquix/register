package main

import (
	"bytes"
	"context"
	"os"
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
	rows := []ingest.Suppression{
		{SKey: "99900004", Reason: "ticket 12\twith a tab", CreatedAt: at},
		{SKey: "r900000004", Origin: "99900004", CreatedAt: at},
	}
	var buf bytes.Buffer
	if err := writeSuppressions(&buf, rows); err != nil {
		t.Fatal(err)
	}
	got, err := readSuppressions(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].SKey != "99900004" || got[0].Reason != "ticket 12 with a tab" || !got[0].CreatedAt.Equal(at) || got[1].Origin != "99900004" {
		t.Fatalf("round trip: %+v", got)
	}
	file := buf.String()
	cut := file[:strings.LastIndex(strings.TrimRight(file, "\n"), "\n")+1] // without the trailer
	lines := strings.SplitAfter(file, "\n")
	for name, damaged := range map[string]string{
		"no trailer":  cut,
		"missing row": lines[0] + lines[2],
		"changed key": strings.Replace(file, "99900004\t\t", "99900005\t\t", 1),
		"bad key":     "not-a-key\t\t" + at.Format(time.RFC3339Nano) + "\t\n" + lines[2],
	} {
		if _, err := readSuppressions(strings.NewReader(damaged)); err == nil {
			t.Errorf("%s: a damaged backup was accepted", name)
		}
	}
}
