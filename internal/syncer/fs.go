package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/accquix/register/internal/ingest"
	"github.com/accquix/register/internal/sources/fs"
)

// FSConfig configures the Financial Administration lists sync.
type FSConfig struct {
	Client  *fs.Client
	WorkDir string
	Lists   []fs.List // default: fs.Lists
	// MaxDrop, when > 0, replaces every list's drop/absence threshold (to accept a list that
	// legitimately shrank: unblock, then run once with a higher value).
	MaxDrop float64
	// SkipMinRows drops the per-list row floors (tests with small synthetic lists; an operator who
	// must accept a list that legitimately fell below its floor).
	SkipMinRows bool
	Now         func() time.Time // default time.Now
	Log         *slog.Logger
}

// FS syncs each list independently: a conditional GET, and a new version is one unit. A blocked
// list does not stop the others.
type FS struct {
	cfg  FSConfig
	conn *pgx.Conn
}

// NewFS returns the FS sync over a direct connection.
func NewFS(conn *pgx.Conn, cfg FSConfig) *FS {
	if cfg.Lists == nil {
		cfg.Lists = fs.Lists
	}
	if cfg.MaxDrop > 0 || cfg.SkipMinRows {
		lists := make([]fs.List, len(cfg.Lists))
		for i, l := range cfg.Lists {
			if cfg.MaxDrop > 0 {
				l.MaxDrop = cfg.MaxDrop
			}
			if cfg.SkipMinRows {
				l.MinRows = 0
			}
			lists[i] = l
		}
		cfg.Lists = lists
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &FS{cfg: cfg, conn: conn}
}

// Source is the sync_source name of a list.
func Source(slug string) string { return "fs:" + slug }

// Run syncs every list once. It returns ErrBlocked (joined with details) when any list is blocked.
func (s *FS) Run(ctx context.Context) error {
	unlock, err := lock(ctx, s.conn)
	if err != nil {
		return err
	}
	defer unlock()
	if err := rebuildIfOwed(ctx, s.conn, s.cfg.Log); err != nil {
		return err
	}
	var errs []error
	for _, l := range s.cfg.Lists {
		if err := s.one(ctx, l); err != nil {
			if !errors.Is(err, ErrBlocked) {
				s.cfg.Log.Error("fs list failed", "list", l.Slug, "error", err)
			}
			errs = append(errs, fmt.Errorf("%s: %w", l.Slug, err))
		}
	}
	return errors.Join(errs...)
}

func (s *FS) one(ctx context.Context, l fs.List) error {
	src := Source(l.Slug)
	st, err := LoadState(ctx, s.conn, src)
	if err != nil {
		return err
	}
	if st.State == "blocked" {
		return fmt.Errorf("%w: %s", ErrBlocked, st.Diagnostic)
	}
	dir := filepath.Join(s.cfg.WorkDir, "fs")
	f, err := s.cfg.Client.Fetch(ctx, l.Slug, st.Cursor.ETag, dir)
	if errors.Is(err, fs.ErrTooLarge) {
		// a rejected unit (D23): the list is blocked with a diagnostic, the data stays
		err = fmt.Errorf("%w: %w", ingest.ErrRejected, err)
		if rerr := ingest.RecordRejected(ctx, s.conn, src+":too-large", src, "", err.Error()); rerr != nil {
			return errors.Join(err, rerr)
		}
		st.PreviousState, st.State, st.Diagnostic = st.State, "blocked", err.Error()
		if serr := save(ctx, s.conn, src, st); serr != nil {
			return errors.Join(err, serr)
		}
		s.cfg.Log.Error("fs list blocked", "list", l.Slug, "diagnostic", st.Diagnostic)
		return fmt.Errorf("%w: %s", ErrBlocked, st.Diagnostic)
	}
	if err != nil {
		return err
	}
	if f.NotModified {
		s.cfg.Log.Debug("fs list not modified", "list", l.Slug)
		return nil
	}
	defer func() { _ = os.Remove(f.Path) }()
	if f.SHA256 == st.Cursor.SHA256 && st.State == "daily" {
		// same content under a new ETag: remember the ETag only
		st.Cursor.ETag = f.ETag
		return save(ctx, s.conn, src, st)
	}
	var prev time.Time
	if st.PublishedAsOf != "" {
		prev = parseDay(st.PublishedAsOf)
	}
	name := fmt.Sprintf("%s:%s", src, f.SHA256[:16])
	t0 := time.Now()
	res, err := ingest.ApplyFS(ctx, s.conn, ingest.FSUnit{
		List: l, Name: name, SHA256: f.SHA256, Path: f.Path, PrevAsOf: prev, Now: s.cfg.Now,
		After: func(ctx context.Context, tx pgx.Tx, r ingest.FSResult) error {
			ns := st
			day := fmtDay(r.AsOf)
			ns.State, ns.AppliedThrough, ns.PublishedAsOf, ns.Diagnostic, ns.PreviousState = "daily", day, day, "", ""
			ns.Cursor.ETag, ns.Cursor.SHA256, ns.Cursor.Skipped = f.ETag, f.SHA256, r.Skipped
			return saveState(ctx, tx, src, ns)
		},
	})
	if err != nil {
		if !errors.Is(err, ingest.ErrRejected) {
			return err
		}
		if rerr := ingest.RecordRejected(ctx, s.conn, name, src, f.SHA256, err.Error()); rerr != nil {
			return errors.Join(err, rerr)
		}
		st.PreviousState, st.State, st.Diagnostic = st.State, "blocked", err.Error()
		if serr := save(ctx, s.conn, src, st); serr != nil {
			return errors.Join(err, serr)
		}
		s.cfg.Log.Error("fs list blocked", "list", l.Slug, "diagnostic", st.Diagnostic)
		return fmt.Errorf("%w: %s", ErrBlocked, st.Diagnostic)
	}
	s.cfg.Log.Info("fs list applied", "list", l.Slug, "as_of", fmtDay(res.AsOf), "items", res.Items, "inserted", res.Inserted,
		"updated", res.Updated, "absent", res.Absent, "duplicates", res.Duplicates, "conflicts", res.Conflicts, "skipped", res.Skipped,
		"duration", time.Since(t0).Round(time.Millisecond))
	return nil
}
