package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/craggy-io/agent-sdk/memory"
)

func exercise(t *testing.T, s memory.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Get(ctx, "ns", "k"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("get missing err=%v", err)
	}
	if _, err := s.Put(ctx, "ns", "k", json.RawMessage(`{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "ns", "a", json.RawMessage(`"x"`)); err != nil {
		t.Fatal(err)
	}
	e, err := s.Get(ctx, "ns", "k")
	if err != nil || string(e.Value) != `{"n":1}` {
		t.Fatalf("get=%+v err=%v", e, err)
	}
	list, err := s.List(ctx, "ns")
	if err != nil || len(list) != 2 || list[0].Key != "a" {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	if err := s.Delete(ctx, "ns", "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "ns", "k"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("get after delete err=%v", err)
	}
	if _, err := s.Put(ctx, "bad/ns", "k", json.RawMessage(`1`)); err == nil {
		t.Fatal("invalid namespace accepted")
	}
	if _, err := s.Put(ctx, "ns", "k", json.RawMessage(`{`)); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

func TestMemStore(t *testing.T) { exercise(t, memory.NewMemStore()) }

func TestFileStorePersists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mem")
	s, err := memory.OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	exercise(t, s)
	s2, err := memory.OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s2.Get(context.Background(), "ns", "a")
	if err != nil || string(e.Value) != `"x"` {
		t.Fatalf("after reopen get=%+v err=%v", e, err)
	}
}
