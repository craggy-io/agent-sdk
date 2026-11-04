// Package memory is a small durable key-value store for agents and
// scripts: namespaced JSON values that outlive a run. It is the kind of
// capability the SDK is built around — implement the Go API here, and
// package agentcap registers it once, after which Starlark scripts,
// agent tools, MCP and HTTP all reach the same store.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned by Get for a missing key.
var ErrNotFound = errors.New("memory: not found")

// Entry is one stored value.
type Entry struct {
	Namespace string          `json:"namespace"`
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Store persists namespaced JSON values. Namespace and key must be
// non-empty and contain no "/" or "..".
type Store interface {
	Put(ctx context.Context, namespace, key string, value json.RawMessage) (Entry, error)
	Get(ctx context.Context, namespace, key string) (Entry, error)
	List(ctx context.Context, namespace string) ([]Entry, error)
	Delete(ctx context.Context, namespace, key string) error
}

func validPart(kind, s string) error {
	if s == "" {
		return fmt.Errorf("memory: %s is required", kind)
	}
	for _, r := range s {
		if r == '/' || r == '\\' || r == 0 {
			return fmt.Errorf("memory: invalid %s %q", kind, s)
		}
	}
	if s == ".." || s == "." {
		return fmt.Errorf("memory: invalid %s %q", kind, s)
	}
	return nil
}

// MemStore is an in-memory Store (tests, ephemeral hosts).
type MemStore struct {
	mu sync.RWMutex
	m  map[string]Entry
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore { return &MemStore{m: map[string]Entry{}} }

func id(ns, key string) string { return ns + "\x00" + key }

func (s *MemStore) Put(_ context.Context, ns, key string, value json.RawMessage) (Entry, error) {
	if err := validPart("namespace", ns); err != nil {
		return Entry{}, err
	}
	if err := validPart("key", key); err != nil {
		return Entry{}, err
	}
	if !json.Valid(value) {
		return Entry{}, fmt.Errorf("memory: value is not valid JSON")
	}
	e := Entry{Namespace: ns, Key: key, Value: append(json.RawMessage(nil), value...), UpdatedAt: time.Now().UTC()}
	s.mu.Lock()
	s.m[id(ns, key)] = e
	s.mu.Unlock()
	return e, nil
}

func (s *MemStore) Get(_ context.Context, ns, key string) (Entry, error) {
	s.mu.RLock()
	e, ok := s.m[id(ns, key)]
	s.mu.RUnlock()
	if !ok {
		return Entry{}, ErrNotFound
	}
	return e, nil
}

func (s *MemStore) List(_ context.Context, ns string) ([]Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Entry
	for _, e := range s.m {
		if e.Namespace == ns {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (s *MemStore) Delete(_ context.Context, ns, key string) error {
	s.mu.Lock()
	delete(s.m, id(ns, key))
	s.mu.Unlock()
	return nil
}

// FileStore is a Store persisted as one JSON document per namespace
// under a directory. Writes are atomic (temp file + rename) and the
// whole namespace is rewritten per Put: right-sized for agent memory,
// not a database.
type FileStore struct {
	dir string
	mu  sync.Mutex
	mem *MemStore
}

// OpenFileStore opens (creating if needed) a file-backed store at dir.
func OpenFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &FileStore{dir: dir, mem: NewMemStore()}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, de := range entries {
		if de.IsDir() || filepath.Ext(de.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, de.Name()))
		if err != nil {
			return nil, err
		}
		var list []Entry
		if err := json.Unmarshal(b, &list); err != nil {
			return nil, fmt.Errorf("memory: %s: %w", de.Name(), err)
		}
		for _, e := range list {
			s.mem.m[id(e.Namespace, e.Key)] = e
		}
	}
	return s, nil
}

func (s *FileStore) flushLocked(ns string) error {
	list, _ := s.mem.List(context.Background(), ns)
	if list == nil {
		list = []Entry{}
	}
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, ns+".json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, ns+".json"))
}

func (s *FileStore) Put(ctx context.Context, ns, key string, value json.RawMessage) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.mem.Put(ctx, ns, key, value)
	if err != nil {
		return Entry{}, err
	}
	if err := s.flushLocked(ns); err != nil {
		return Entry{}, err
	}
	return e, nil
}

func (s *FileStore) Get(ctx context.Context, ns, key string) (Entry, error) {
	return s.mem.Get(ctx, ns, key)
}

func (s *FileStore) List(ctx context.Context, ns string) ([]Entry, error) {
	return s.mem.List(ctx, ns)
}

func (s *FileStore) Delete(ctx context.Context, ns, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.mem.Delete(ctx, ns, key); err != nil {
		return err
	}
	return s.flushLocked(ns)
}

var (
	_ Store = (*MemStore)(nil)
	_ Store = (*FileStore)(nil)
)
