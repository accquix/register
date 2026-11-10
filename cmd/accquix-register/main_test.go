package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/accquix/register/internal/config"
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
		{[]string{"serve"}, 1, "not implemented"},
		{[]string{"sync"}, 1, "REGISTER_SYNC_DATABASE_URL or REGISTER_DATABASE_URL is required"},
		{[]string{"sync", "--source", "fs"}, 2, "not yet implemented source"},
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
