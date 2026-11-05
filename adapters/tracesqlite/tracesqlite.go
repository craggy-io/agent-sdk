// Package tracesqlite is the durable trace.Sink (§7.2): modernc.org/sqlite,
// pure Go, synchronous=FULL. Tables: events, blobs, writer_lease.
package tracesqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/craggy-io/agent-sdk/trace"
	"github.com/craggy-io/foundation-kit/adapters/sqlite"
)

// DefaultLeaseTTL matches trace/memory.
const DefaultLeaseTTL = 30 * time.Second

const schema = `
CREATE TABLE IF NOT EXISTS events (
  cursor     INTEGER PRIMARY KEY AUTOINCREMENT,
  type       TEXT NOT NULL,
  time_nano  INTEGER NOT NULL,
  session_id TEXT NOT NULL DEFAULT '',
  run_id     TEXT NOT NULL DEFAULT '',
  node_id    TEXT NOT NULL DEFAULT '',
  step_id    TEXT NOT NULL DEFAULT '',
  data       BLOB
);
CREATE INDEX IF NOT EXISTS events_by_run ON events(run_id, cursor);
CREATE INDEX IF NOT EXISTS events_by_step ON events(step_id, cursor);
CREATE TABLE IF NOT EXISTS blobs (
  hash TEXT PRIMARY KEY,
  data BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS writer_lease (
  session_id  TEXT PRIMARY KEY,
  token       INTEGER NOT NULL,
  expires_nano INTEGER NOT NULL
);`

// Store is a SQLite-backed Sink. One Store per file; writes are serialized
// through a single connection so lease claims are transactional (§7.2).
type Store struct {
	db  *sql.DB
	TTL time.Duration
}

var _ trace.Sink = (*Store)(nil)

// Open opens (creating if needed) the trace database at path. Use
// ":memory:" for an ephemeral database private to this Store.
func Open(path string) (*Store, error) {
	// One connection, WAL, FULL sync and BEGIN IMMEDIATE, so lease claims are
	// transactional and durable.
	db, err := sqlite.Open(context.Background(), path, sqlite.Options{Synchronous: sqlite.SyncFull})
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("trace/sqlite: schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) ttl() time.Duration {
	if s.TTL > 0 {
		return s.TTL
	}
	return DefaultLeaseTTL
}

// checkLeaseTx enforces fencing for one session inside tx.
func checkLeaseTx(ctx context.Context, tx *sql.Tx, sessionID string) error {
	var token, expires int64
	err := tx.QueryRowContext(ctx,
		`SELECT token, expires_nano FROM writer_lease WHERE session_id=?`, sessionID).
		Scan(&token, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if time.Now().UnixNano() > expires {
		return nil // expired lease fences nobody
	}
	tok, has := trace.LeaseToken(ctx)
	if !has || tok != token {
		return trace.ErrStaleWriter
	}
	return nil
}

func (s *Store) Append(ctx context.Context, events ...trace.Event) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seen := map[string]bool{}
	for _, e := range events {
		if !seen[e.SessionID] {
			seen[e.SessionID] = true
			if err := checkLeaseTx(ctx, tx, e.SessionID); err != nil {
				return err
			}
		}
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO events(type,time_nano,session_id,run_id,node_id,step_id,data) VALUES(?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range events {
		if _, err := stmt.ExecContext(ctx, e.Type, e.Time.UnixNano(), e.SessionID, e.RunID, e.NodeID, e.StepID, []byte(e.Data)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) PutBlob(ctx context.Context, data []byte) (trace.Hash, error) {
	h := trace.HashOf(data)
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO blobs(hash,data) VALUES(?,?)`, string(h), data)
	if err != nil {
		return "", err
	}
	return h, nil
}

func (s *Store) GetBlob(ctx context.Context, h trace.Hash) ([]byte, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM blobs WHERE hash=?`, string(h)).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, trace.ErrNotFound
	}
	return b, err
}

func (s *Store) ReadEvents(ctx context.Context, after int64, limit int) ([]trace.Event, int64, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT cursor,type,time_nano,session_id,run_id,node_id,step_id,data FROM events WHERE cursor>? ORDER BY cursor LIMIT ?`,
		after, limit)
	if err != nil {
		return nil, after, err
	}
	defer rows.Close()
	var out []trace.Event
	next := after
	for rows.Next() {
		var e trace.Event
		var nano int64
		var data []byte
		if err := rows.Scan(&e.Cursor, &e.Type, &nano, &e.SessionID, &e.RunID, &e.NodeID, &e.StepID, &data); err != nil {
			return nil, after, err
		}
		e.Time = time.Unix(0, nano).UTC()
		e.Data = data
		out = append(out, e)
		next = e.Cursor
	}
	return out, next, rows.Err()
}

func (s *Store) AcquireWriter(ctx context.Context, sessionID string) (trace.WriterLease, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var token, expires int64
	err = tx.QueryRowContext(ctx,
		`SELECT token, expires_nano FROM writer_lease WHERE session_id=?`, sessionID).
		Scan(&token, &expires)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		token = 0
	case err != nil:
		return nil, err
	case time.Now().UnixNano() <= expires:
		return nil, trace.ErrLeaseHeld
	}
	token++
	exp := time.Now().Add(s.ttl()).UnixNano()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO writer_lease(session_id,token,expires_nano) VALUES(?,?,?)
		 ON CONFLICT(session_id) DO UPDATE SET token=excluded.token, expires_nano=excluded.expires_nano`,
		sessionID, token, exp); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &lease{store: s, session: sessionID, token: token}, nil
}

type lease struct {
	store   *Store
	session string
	token   int64
}

func (l *lease) Token() int64 { return l.token }

func (l *lease) Renew(ctx context.Context) error {
	exp := time.Now().Add(l.store.ttl()).UnixNano()
	res, err := l.store.db.ExecContext(ctx,
		`UPDATE writer_lease SET expires_nano=? WHERE session_id=? AND token=? AND expires_nano>?`,
		exp, l.session, l.token, time.Now().UnixNano())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return trace.ErrStaleWriter
	}
	return nil
}

func (l *lease) Release(ctx context.Context) error {
	// Expire in place, never DELETE: the row carries the last fencing
	// token, which must stay monotonic across releases (§7.2).
	_, err := l.store.db.ExecContext(ctx,
		`UPDATE writer_lease SET expires_nano=0 WHERE session_id=? AND token=?`, l.session, l.token)
	return err
}
