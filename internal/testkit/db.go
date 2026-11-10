package testkit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/accquix/register/internal/migrate"
)

// Database creates a fresh, migrated database on the server named by REGISTER_TEST_DATABASE_URL
// and returns its DSN. The test is skipped when the variable is unset. The database is dropped
// when the test ends.
func Database(t *testing.T) string {
	t.Helper()
	admin := os.Getenv("REGISTER_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("REGISTER_TEST_DATABASE_URL not set; skipping database test")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	name := "register_test_" + hex.EncodeToString(b)
	c, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := c.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	c.Close(ctx)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err == nil {
			c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(context.Background())
		}
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	dsn := u.String()
	if _, err := migrate.Up(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return dsn
}

// Conn opens a connection that is closed when the test ends.
func Conn(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

// Roles creates three login-less database roles with unique names (API, sync, operator) for tests
// of the privilege matrix, and drops them when the test ends. Call it BEFORE Database: cleanups
// run last-in first-out, and a role can only be dropped after the database that granted it rights.
func Roles(t *testing.T) (api, sync, operator string) {
	t.Helper()
	admin := os.Getenv("REGISTER_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("REGISTER_TEST_DATABASE_URL not set; skipping database test")
	}
	ctx := context.Background()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	suffix := hex.EncodeToString(b)
	names := []string{"reg_t_api_" + suffix, "reg_t_sync_" + suffix, "reg_t_op_" + suffix}
	c, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close(ctx)
	for _, n := range names {
		if _, err := c.Exec(ctx, "CREATE ROLE "+n); err != nil {
			t.Fatalf("create role: %v", err)
		}
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err == nil {
			for _, n := range names {
				c.Exec(context.Background(), "DROP ROLE IF EXISTS "+n)
			}
			c.Close(context.Background())
		}
	})
	return names[0], names[1], names[2]
}

// As opens a connection that acts as role (SET ROLE), closed when the test ends.
func As(t *testing.T, dsn, role string) *pgx.Conn {
	t.Helper()
	c := Conn(t, dsn)
	if _, err := c.Exec(context.Background(), "SET ROLE "+role); err != nil {
		t.Fatal(err)
	}
	return c
}
