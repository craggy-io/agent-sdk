// Package memory is the in-memory trace.Sink for tests (§7.2). It
// implements the full contract, including writer-lease fencing.
package memory

import (
	"context"
	"sync"
	"time"

	"github.com/craggy-io/agent-sdk/trace"
)

// DefaultLeaseTTL is how long a writer lease lasts without Renew.
const DefaultLeaseTTL = 30 * time.Second

type leaseState struct {
	token   int64
	expires time.Time
}

// Store is an in-memory Sink. Safe for concurrent use.
type Store struct {
	mu     sync.Mutex
	events []trace.Event
	blobs  map[trace.Hash][]byte
	leases map[string]*leaseState
	tokens map[string]int64 // last issued token per session, survives release
	TTL    time.Duration    // lease TTL; DefaultLeaseTTL when zero
}

var _ trace.Sink = (*Store)(nil)

// New returns an empty Store.
func New() *Store {
	return &Store{
		blobs:  map[trace.Hash][]byte{},
		leases: map[string]*leaseState{},
		tokens: map[string]int64{},
	}
}

func (s *Store) ttl() time.Duration {
	if s.TTL > 0 {
		return s.TTL
	}
	return DefaultLeaseTTL
}

// checkLeaseLocked enforces fencing for one session: if a live lease exists,
// ctx must carry its token.
func (s *Store) checkLeaseLocked(ctx context.Context, sessionID string) error {
	ls, ok := s.leases[sessionID]
	if !ok || time.Now().After(ls.expires) {
		return nil
	}
	tok, has := trace.LeaseToken(ctx)
	if !has || tok != ls.token {
		return trace.ErrStaleWriter
	}
	return nil
}

func (s *Store) Append(ctx context.Context, events ...trace.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for _, e := range events {
		if !seen[e.SessionID] {
			seen[e.SessionID] = true
			if err := s.checkLeaseLocked(ctx, e.SessionID); err != nil {
				return err
			}
		}
	}
	for _, e := range events {
		e.Cursor = int64(len(s.events)) + 1
		s.events = append(s.events, e)
	}
	return nil
}

func (s *Store) PutBlob(ctx context.Context, data []byte) (trace.Hash, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	h := trace.HashOf(data)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.blobs[h]; !ok {
		cp := make([]byte, len(data))
		copy(cp, data)
		s.blobs[h] = cp
	}
	return h, nil
}

func (s *Store) GetBlob(ctx context.Context, h trace.Hash) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.blobs[h]
	if !ok {
		return nil, trace.ErrNotFound
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp, nil
}

func (s *Store) ReadEvents(ctx context.Context, after int64, limit int) ([]trace.Event, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, after, err
	}
	if limit <= 0 {
		limit = 100
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []trace.Event
	next := after
	for _, e := range s.events {
		if e.Cursor <= after {
			continue
		}
		out = append(out, e)
		next = e.Cursor
		if len(out) >= limit {
			break
		}
	}
	return out, next, nil
}

func (s *Store) AcquireWriter(ctx context.Context, sessionID string) (trace.WriterLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ls, ok := s.leases[sessionID]; ok && time.Now().Before(ls.expires) {
		return nil, trace.ErrLeaseHeld
	}
	s.tokens[sessionID]++
	tok := s.tokens[sessionID]
	s.leases[sessionID] = &leaseState{token: tok, expires: time.Now().Add(s.ttl())}
	return &lease{store: s, session: sessionID, token: tok}, nil
}

type lease struct {
	store   *Store
	session string
	token   int64
}

func (l *lease) Token() int64 { return l.token }

func (l *lease) Renew(ctx context.Context) error {
	l.store.mu.Lock()
	defer l.store.mu.Unlock()
	ls, ok := l.store.leases[l.session]
	if !ok || ls.token != l.token {
		return trace.ErrStaleWriter
	}
	ls.expires = time.Now().Add(l.store.ttl())
	return nil
}

func (l *lease) Release(ctx context.Context) error {
	l.store.mu.Lock()
	defer l.store.mu.Unlock()
	if ls, ok := l.store.leases[l.session]; ok && ls.token == l.token {
		delete(l.store.leases, l.session)
	}
	return nil
}
