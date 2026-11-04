package tracesqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/craggy-io/agent-sdk/adapters/tracesqlite"
	"github.com/craggy-io/agent-sdk/trace"
	"github.com/craggy-io/agent-sdk/trace/sinktest"
)

func TestSinkConformance(t *testing.T) {
	sinktest.Run(t, func(t *testing.T) trace.Sink {
		s, err := tracesqlite.Open(filepath.Join(t.TempDir(), "trace.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}

// Events and blobs survive a close/reopen (§7.2: durable on return).
func TestPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "trace.db")
	s, err := tracesqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.PutBlob(ctx, []byte("durable"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, trace.Event{Type: "run.start", Time: time.Now(), SessionID: "s", RunID: "r"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := tracesqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	evs, _, err := s2.ReadEvents(ctx, 0, 10)
	if err != nil || len(evs) != 1 || evs[0].Type != "run.start" {
		t.Fatalf("after reopen: evs=%v err=%v", evs, err)
	}
	b, err := s2.GetBlob(ctx, h)
	if err != nil || string(b) != "durable" {
		t.Errorf("blob after reopen=%q err=%v", b, err)
	}
	// Cursors keep increasing across reopen.
	if err := s2.Append(ctx, trace.Event{Type: "run.end", Time: time.Now(), SessionID: "s", RunID: "r"}); err != nil {
		t.Fatal(err)
	}
	evs, _, _ = s2.ReadEvents(ctx, 1, 10)
	if len(evs) != 1 || evs[0].Cursor != 2 {
		t.Errorf("cursor after reopen: %+v", evs)
	}
}
