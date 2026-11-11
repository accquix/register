// Package migrate applies the embedded goose migrations.
package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"

	"github.com/accquix/register/db"
)

// Open returns a goose provider over the embedded migrations and the database at dsn.
// The caller closes the returned *sql.DB.
func Open(dsn string) (*goose.Provider, *sql.DB, error) {
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open database: %w", err)
	}
	sub, err := fs.Sub(db.Migrations, "migrations")
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	p, err := goose.NewProvider(database.DialectPostgres, conn, sub)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("goose: %w", err)
	}
	return p, conn, nil
}

// Up applies every pending migration and returns how many were applied.
func Up(ctx context.Context, dsn string) (int, error) {
	p, conn, err := Open(dsn)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	res, err := p.Up(ctx)
	return len(res), err
}

// DownTo rolls back to version (0 = empty schema).
func DownTo(ctx context.Context, dsn string, version int64) (int, error) {
	p, conn, err := Open(dsn)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	res, err := p.DownTo(ctx, version)
	return len(res), err
}

// Status describes each migration: version, file and whether it is applied.
type Status struct {
	Version int64
	Source  string
	Applied bool
}

// Statuses lists the migrations and their state.
func Statuses(ctx context.Context, dsn string) ([]Status, error) {
	p, conn, err := Open(dsn)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	st, err := p.Status(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Status, 0, len(st))
	for _, s := range st {
		out = append(out, Status{Version: s.Source.Version, Source: s.Source.Path, Applied: s.State == goose.StateApplied})
	}
	return out, nil
}

// Latest is the newest migration version embedded in this binary.
func Latest() (int64, error) {
	sub, err := fs.Sub(db.Migrations, "migrations")
	if err != nil {
		return 0, err
	}
	entries, err := fs.ReadDir(sub, ".")
	if err != nil {
		return 0, err
	}
	var latest int64
	for _, e := range entries {
		var v int64
		if _, err := fmt.Sscanf(e.Name(), "%d_", &v); err == nil && v > latest {
			latest = v
		}
	}
	return latest, nil
}

// RowQuerier is what Applied needs: a connection or a pool.
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Applied is the newest migration version applied to the database.
func Applied(ctx context.Context, q RowQuerier) (int64, error) {
	var v int64
	err := q.QueryRow(ctx, `SELECT coalesce(max(version_id), 0) FROM goose_db_version WHERE is_applied`).Scan(&v)
	return v, err
}
