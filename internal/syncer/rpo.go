// Package syncer drives the source state machines. Only one sync runs at a time (advisory lock on
// a direct connection); every step is a resumable unit, so the process can be killed anywhere.
package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/accquix/register/internal/ingest"
	"github.com/accquix/register/internal/sources/rpo"
)

// LockKey is the PostgreSQL advisory lock that serialises syncs.
const LockKey int64 = 0x4163_5265_6753_796e // "AcRegSyn"

var (
	// ErrLocked means another sync holds the lock; nothing was done.
	ErrLocked = errors.New("another sync is running")
	// ErrBlocked means the source is blocked by a failed validation until an operator unblocks it.
	ErrBlocked = errors.New("source is blocked")
)

// RPOConfig configures the RPO sync. Zero thresholds take the defaults of decision D23.
type RPOConfig struct {
	Bucket           *rpo.Bucket
	WorkDir          string
	Concurrency      int
	PartRecords      int     // records in every snapshot part except the last (100 000)
	DailyMaxRecords  int     // a daily file above this is rejected (20 000)
	SnapshotMinRatio float64 // a snapshot below this share of the previous one blocks absence marking (0.98)
	AbsenceMaxRatio  float64 // absence marking is blocked above this share of active rows (0.005)
	Log              *slog.Logger
}

func (c *RPOConfig) defaults() {
	if c.Concurrency <= 0 {
		c.Concurrency = 6
	}
	if c.PartRecords <= 0 {
		c.PartRecords = 100_000
	}
	if c.DailyMaxRecords <= 0 {
		c.DailyMaxRecords = 20_000
	}
	if c.SnapshotMinRatio <= 0 {
		c.SnapshotMinRatio = 0.98
	}
	if c.AbsenceMaxRatio <= 0 {
		c.AbsenceMaxRatio = 0.005
	}
	if c.Log == nil {
		c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
}

// RPO is the RPO state machine:
// empty → bootstrap → daily ⇄ gap_wait, daily → reconcile → daily, any → blocked.
type RPO struct {
	cfg  RPOConfig
	conn *pgx.Conn
}

// NewRPO returns the RPO sync over a direct (non-pooled) connection.
func NewRPO(conn *pgx.Conn, cfg RPOConfig) *RPO {
	cfg.defaults()
	return &RPO{cfg: cfg, conn: conn}
}

// State is the persisted sync_source row.
type State struct {
	State          string
	BaseSnapshot   string // YYYY-MM-DD
	AppliedThrough string
	PublishedAsOf  string
	PreviousState  string
	Cursor         struct {
		Reconcile string `json:"reconcile,omitempty"`
	}
	Diagnostic string
}

const day = 24 * time.Hour

func parseDay(s string) time.Time { t, _ := time.Parse("2006-01-02", s); return t }
func fmtDay(t time.Time) string   { return t.Format("2006-01-02") }
func dateOrNil(s string) any {
	if s == "" {
		return nil
	}
	return parseDay(s)
}

// LoadState reads the source state; a missing row is the empty state.
func LoadState(ctx context.Context, q ingest.RowQuerier, source string) (State, error) {
	var st State
	var base, through, pub *time.Time
	var prev, diag *string
	var cursor []byte
	err := q.QueryRow(ctx, `SELECT state, base_snapshot, applied_through, published_as_of, previous_state, cursor, diagnostic
FROM sync_source WHERE source = $1`, source).Scan(&st.State, &base, &through, &pub, &prev, &cursor, &diag)
	if errors.Is(err, pgx.ErrNoRows) {
		return State{State: "empty"}, nil
	}
	if err != nil {
		return st, err
	}
	for _, p := range []struct {
		t   *time.Time
		dst *string
	}{{base, &st.BaseSnapshot}, {through, &st.AppliedThrough}, {pub, &st.PublishedAsOf}} {
		if p.t != nil {
			*p.dst = fmtDay(*p.t)
		}
	}
	if prev != nil {
		st.PreviousState = *prev
	}
	if diag != nil {
		st.Diagnostic = *diag
	}
	if len(cursor) > 0 {
		if err := json.Unmarshal(cursor, &st.Cursor); err != nil {
			return st, err
		}
	}
	return st, nil
}

func saveState(ctx context.Context, tx pgx.Tx, source string, st State) error {
	cur, err := json.Marshal(st.Cursor)
	if err != nil {
		return err
	}
	var prev, diag any
	if st.PreviousState != "" {
		prev = st.PreviousState
	}
	if st.Diagnostic != "" {
		diag = st.Diagnostic
	}
	_, err = tx.Exec(ctx, `INSERT INTO sync_source (source, state, base_snapshot, applied_through, published_as_of, previous_state, cursor, diagnostic, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
ON CONFLICT (source) DO UPDATE SET state = EXCLUDED.state, base_snapshot = EXCLUDED.base_snapshot,
  applied_through = EXCLUDED.applied_through, published_as_of = EXCLUDED.published_as_of,
  previous_state = EXCLUDED.previous_state, cursor = EXCLUDED.cursor, diagnostic = EXCLUDED.diagnostic, updated_at = now()`,
		source, st.State, dateOrNil(st.BaseSnapshot), dateOrNil(st.AppliedThrough), dateOrNil(st.PublishedAsOf), prev, cur, diag)
	return err
}

func (s *RPO) save(ctx context.Context, st State) error {
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := saveState(ctx, tx, "rpo", st); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Unblock restores the state that was active before the source was blocked.
func Unblock(ctx context.Context, conn *pgx.Conn, source string) error {
	tag, err := conn.Exec(ctx, `UPDATE sync_source SET state = coalesce(previous_state, 'empty'), previous_state = NULL,
  diagnostic = NULL, updated_at = now() WHERE source = $1 AND state = 'blocked'`, source)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s is not blocked", source)
	}
	return nil
}

// Run performs one sync pass: bootstrap, reconcile and daily files as far as the bucket allows.
func (s *RPO) Run(ctx context.Context) error {
	var locked bool
	if err := s.conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, LockKey).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return ErrLocked
	}
	defer s.conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, LockKey) //nolint:errcheck

	st, err := LoadState(ctx, s.conn, "rpo")
	if err != nil {
		return err
	}
	if st.State == "blocked" {
		return fmt.Errorf("%w: %s", ErrBlocked, st.Diagnostic)
	}
	if err := os.MkdirAll(filepath.Join(s.cfg.WorkDir, "rpo"), 0o700); err != nil {
		return err
	}
	inv, err := s.cfg.Bucket.Inventory(ctx)
	if err != nil {
		return err
	}
	switch st.State {
	case "empty", "bootstrap":
		if err := s.bootstrap(ctx, &st, inv); err != nil {
			return s.handle(ctx, &st, err)
		}
		if st.State != "daily" {
			return nil // still waiting for a complete snapshot
		}
	case "reconcile":
		if err := s.reconcile(ctx, &st, inv, st.Cursor.Reconcile); err != nil {
			return s.handle(ctx, &st, err)
		}
	}
	return s.handle(ctx, &st, s.daily(ctx, &st, inv))
}

// handle turns a validation rejection into the blocked state.
func (s *RPO) handle(ctx context.Context, st *State, err error) error {
	if err == nil || !errors.Is(err, ingest.ErrRejected) {
		return err
	}
	st.PreviousState = st.State
	st.State = "blocked"
	st.Diagnostic = err.Error()
	if serr := s.save(ctx, *st); serr != nil {
		return errors.Join(err, serr)
	}
	s.cfg.Log.Error("rpo blocked", "diagnostic", st.Diagnostic)
	return fmt.Errorf("%w: %s", ErrBlocked, st.Diagnostic)
}

// completeSnapshot returns the newest snapshot (optionally only newer than after) whose manifest
// and every listed part are in the bucket, with its parts in manifest order.
func (s *RPO) completeSnapshot(ctx context.Context, inv rpo.Inventory, after string) (*rpo.Snapshot, []rpo.Object, string, error) {
	for _, d := range inv.SnapshotDates() {
		if after != "" && d <= after {
			break
		}
		snap := inv.Snapshots[d]
		if snap.Manifest == nil {
			continue
		}
		names, err := s.cfg.Bucket.ReadManifest(ctx, snap.Manifest.Key)
		if err != nil {
			return nil, nil, "", err
		}
		if len(names) == 0 {
			continue
		}
		var parts []rpo.Object
		complete := true
		for _, n := range names {
			o, ok := snap.Parts[n]
			if !ok {
				complete = false
				break
			}
			parts = append(parts, o)
		}
		if complete {
			h := sha256.Sum256([]byte(strings.Join(names, "\n")))
			return snap, parts, hex.EncodeToString(h[:]), nil
		}
	}
	return nil, nil, "", nil
}

func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// apply downloads (if needed) and applies one file as a unit.
func (s *RPO) apply(ctx context.Context, obj rpo.Object, path string, u ingest.RPOUnit) (ingest.UnitResult, error) {
	var err error
	if path == "" {
		if path, err = s.cfg.Bucket.Download(ctx, obj, filepath.Join(s.cfg.WorkDir, "rpo")); err != nil {
			return ingest.UnitResult{}, err
		}
	}
	if u.SHA256, err = fileSHA(path); err != nil {
		return ingest.UnitResult{}, err
	}
	u.Open = func() (io.ReadCloser, error) { return os.Open(path) }
	t0 := time.Now()
	res, err := ingest.ApplyRPO(ctx, s.conn, u)
	if err != nil {
		if errors.Is(err, ingest.ErrRejected) {
			if rerr := ingest.RecordRejected(ctx, s.conn, u.Name, "rpo", u.SHA256, err.Error()); rerr != nil {
				return res, errors.Join(err, rerr)
			}
		}
		return res, err
	}
	s.cfg.Log.Info("rpo unit applied", "unit", u.Name, "records", res.Records, "inserted", res.Inserted,
		"updated", res.Updated, "duration", time.Since(t0).Round(time.Millisecond))
	_ = os.Remove(path)
	return res, nil
}

func (s *RPO) partValidator(i, n int) func(int) error {
	want := s.cfg.PartRecords
	return func(records int) error {
		switch {
		case records == 0:
			return errors.New("empty snapshot part")
		case i < n-1 && records != want:
			return fmt.Errorf("snapshot part has %d records, want %d", records, want)
		case records > want:
			return fmt.Errorf("last snapshot part has %d records, more than %d", records, want)
		}
		return nil
	}
}

func (s *RPO) dailyValidator() func(int) error {
	return func(records int) error {
		if records > s.cfg.DailyMaxRecords {
			return fmt.Errorf("daily file has %d records, more than %d", records, s.cfg.DailyMaxRecords)
		}
		return nil
	}
}

func unitName(obj rpo.Object) string { return "rpo:" + filepath.Base(obj.Key) }

// applyParts applies the parts of a snapshot that are not applied yet, downloading them in
// parallel first. It returns the total records of all parts.
func (s *RPO) applyParts(ctx context.Context, parts []rpo.Object, refresh, markSeen bool) (int64, error) {
	var todo []rpo.Object
	var idx []int
	for i, p := range parts {
		done, err := ingest.Applied(ctx, s.conn, unitName(p))
		if err != nil {
			return 0, err
		}
		if !done {
			todo = append(todo, p)
			idx = append(idx, i)
		}
	}
	if len(todo) > 0 {
		paths, err := s.cfg.Bucket.DownloadAll(ctx, todo, filepath.Join(s.cfg.WorkDir, "rpo"), s.cfg.Concurrency)
		if err != nil {
			return 0, err
		}
		for k, p := range todo {
			if _, err := s.apply(ctx, p, paths[k], ingest.RPOUnit{
				Name: unitName(p), Rank: 1, RefreshSearch: refresh, MarkSeen: markSeen,
				Validate: s.partValidator(idx[k], len(parts)),
			}); err != nil {
				return 0, err
			}
		}
	}
	names := make([]string, len(parts))
	for i, p := range parts {
		names[i] = unitName(p)
	}
	var total int64
	err := s.conn.QueryRow(ctx, `SELECT coalesce(sum(records), 0) FROM sync_unit WHERE unit = ANY($1) AND status = 'applied'`, names).Scan(&total)
	return total, err
}

func (s *RPO) bootstrap(ctx context.Context, st *State, inv rpo.Inventory) error {
	snap, parts, manifestSHA, err := s.completeSnapshot(ctx, inv, "")
	if err != nil {
		return err
	}
	if snap == nil {
		s.cfg.Log.Info("rpo bootstrap waiting: no complete snapshot in the bucket")
		return nil
	}
	date := fmtDay(snap.Date)
	if st.State == "empty" || st.BaseSnapshot != date {
		st.State, st.BaseSnapshot = "bootstrap", date
		if err := s.save(ctx, *st); err != nil {
			return err
		}
	}
	s.cfg.Log.Info("rpo bootstrap", "snapshot", date, "parts", len(parts))
	total, err := s.applyParts(ctx, parts, false, false)
	if err != nil {
		return err
	}
	through := date
	for d := snap.Date.Add(day); ; d = d.Add(day) {
		obj, ok := inv.Dailies[fmtDay(d)]
		if !ok {
			break
		}
		if done, err := ingest.Applied(ctx, s.conn, unitName(obj)); err != nil {
			return err
		} else if !done {
			if _, err := s.apply(ctx, obj, "", ingest.RPOUnit{Name: unitName(obj), Rank: 0, Validate: s.dailyValidator()}); err != nil {
				return err
			}
		}
		through = fmtDay(d)
	}

	// Publish: build the search layer and switch the state in one transaction (D22).
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := ingest.RebuildSearch(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO rpo_snapshot_seen (snapshot_date, parts, records, manifest_sha256) VALUES ($1, $2, $3, $4)
ON CONFLICT (snapshot_date) DO UPDATE SET parts = EXCLUDED.parts, records = EXCLUDED.records, manifest_sha256 = EXCLUDED.manifest_sha256`,
		snap.Date, len(parts), total, manifestSHA); err != nil {
		return err
	}
	st.State, st.AppliedThrough, st.PublishedAsOf, st.Diagnostic = "daily", through, through, ""
	if err := saveState(ctx, tx, "rpo", *st); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.cfg.Log.Info("rpo published", "snapshot", date, "records", total, "applied_through", through)
	return nil
}

func (s *RPO) reconcile(ctx context.Context, st *State, inv rpo.Inventory, date string) error {
	snap, ok := inv.Snapshots[date]
	if !ok {
		// the snapshot left the bucket mid-reconcile: give up on it, the next one will heal.
		st.State, st.Cursor.Reconcile = "daily", ""
		return s.save(ctx, *st)
	}
	_, parts, manifestSHA, err := s.completeSnapshot(ctx, rpo.Inventory{Snapshots: map[string]*rpo.Snapshot{date: snap}}, "")
	if err != nil {
		return err
	}
	if parts == nil {
		return fmt.Errorf("snapshot %s is no longer complete", date)
	}
	if st.State != "reconcile" || st.Cursor.Reconcile != date {
		tx, err := s.conn.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `TRUNCATE rpo_seen`); err != nil {
			return err
		}
		st.State, st.Cursor.Reconcile = "reconcile", date
		if err := saveState(ctx, tx, "rpo", *st); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	s.cfg.Log.Info("rpo reconcile", "snapshot", date, "parts", len(parts))
	total, err := s.applyParts(ctx, parts, true, true)
	if err != nil {
		return err
	}
	var prev int64
	if err := s.conn.QueryRow(ctx, `SELECT coalesce((SELECT records FROM rpo_snapshot_seen WHERE snapshot_date < $1 ORDER BY snapshot_date DESC LIMIT 1), 0)`,
		snap.Date).Scan(&prev); err != nil {
		return err
	}
	if prev > 0 && float64(total) < s.cfg.SnapshotMinRatio*float64(prev) {
		return fmt.Errorf("%w: snapshot %s has %d records, previous had %d (minimum ratio %.2f); absence marking not done",
			ingest.ErrRejected, date, total, prev, s.cfg.SnapshotMinRatio)
	}

	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	ab, err := ingest.MarkAbsent(ctx, tx, snap.Date, s.cfg.AbsenceMaxRatio)
	if err != nil {
		return err
	}
	if ab.Candidates > 0 && !ab.Marked {
		return fmt.Errorf("%w: snapshot %s: %d of %d active records would be marked absent (maximum ratio %.4f)",
			ingest.ErrRejected, date, ab.Candidates, ab.Active, s.cfg.AbsenceMaxRatio)
	}
	if _, err := tx.Exec(ctx, `TRUNCATE rpo_seen`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO rpo_snapshot_seen (snapshot_date, parts, records, manifest_sha256) VALUES ($1, $2, $3, $4)
ON CONFLICT (snapshot_date) DO UPDATE SET parts = EXCLUDED.parts, records = EXCLUDED.records, manifest_sha256 = EXCLUDED.manifest_sha256`,
		snap.Date, len(parts), total, manifestSHA); err != nil {
		return err
	}
	st.BaseSnapshot = date
	if st.AppliedThrough < date {
		st.AppliedThrough = date
	}
	st.PublishedAsOf, st.State, st.Cursor.Reconcile, st.Diagnostic = st.AppliedThrough, "daily", "", ""
	if err := saveState(ctx, tx, "rpo", *st); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.cfg.Log.Info("rpo reconciled", "snapshot", date, "records", total, "absent_marked", ab.Candidates)
	return nil
}

func (s *RPO) daily(ctx context.Context, st *State, inv rpo.Inventory) error {
	for {
		// A complete snapshot newer than the base is reconciled first (monthly; it also heals gaps).
		snap, _, _, err := s.completeSnapshot(ctx, inv, st.BaseSnapshot)
		if err != nil {
			return err
		}
		if snap != nil {
			if err := s.reconcile(ctx, st, inv, fmtDay(snap.Date)); err != nil {
				return err
			}
			continue
		}
		next := fmtDay(parseDay(st.AppliedThrough).Add(day))
		obj, ok := inv.Dailies[next]
		if !ok {
			if newest := inv.NewestDaily(); newest > next {
				if st.State != "gap_wait" {
					st.State, st.Diagnostic = "gap_wait", "waiting for actual_"+next+"; later daily files are not applied out of order"
					s.cfg.Log.Warn("rpo gap", "missing", next, "newest", newest)
					return s.save(ctx, *st)
				}
				return nil
			}
			if st.State != "daily" || st.Diagnostic != "" {
				st.State, st.Diagnostic = "daily", ""
				return s.save(ctx, *st)
			}
			return nil
		}
		date := next
		if _, err := s.apply(ctx, obj, "", ingest.RPOUnit{
			Name: unitName(obj), Rank: 0, RefreshSearch: true, Validate: s.dailyValidator(),
			After: func(ctx context.Context, tx pgx.Tx, _ ingest.UnitResult) error {
				ns := *st
				ns.State, ns.AppliedThrough, ns.PublishedAsOf, ns.Diagnostic = "daily", date, date, ""
				return saveState(ctx, tx, "rpo", ns)
			},
		}); err != nil {
			return err
		}
		st.State, st.AppliedThrough, st.PublishedAsOf, st.Diagnostic = "daily", date, date, ""
	}
}
