// Package sinktest runs the shared trace.Sink conformance suite (§7.2)
// against any Sink implementation. Each sink's tests call Run with a factory.
package sinktest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/craggy-io/agent-sdk/trace"
)

// Factory builds a fresh, empty Sink.
type Factory func(t *testing.T) trace.Sink

// Run executes the conformance suite as subtests.
func Run(t *testing.T, newSink Factory) {
	t.Helper()
	t.Run("AppendReadPaging", func(t *testing.T) { appendReadPaging(t, newSink(t)) })
	t.Run("BlobRoundtripDedupe", func(t *testing.T) { blobRoundtrip(t, newSink(t)) })
	t.Run("LeaseContentionAndFencing", func(t *testing.T) { leaseFencing(t, newSink(t)) })
	t.Run("AtomicBatch", func(t *testing.T) { atomicBatch(t, newSink(t)) })
}

func appendReadPaging(t *testing.T, s trace.Sink) {
	t.Helper()
	ctx := context.Background()
	const n = 25
	for i := 0; i < n; i++ {
		err := s.Append(ctx, trace.Event{
			Type: "test.event", Time: time.Unix(int64(1000+i), 0).UTC(),
			SessionID: "sess", RunID: "run",
			Data: []byte(fmt.Sprintf(`{"i":%d}`, i)),
		})
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	// Page through with a small limit; cursors must be dense and ordered.
	var got []trace.Event
	after := int64(0)
	for {
		page, next, err := s.ReadEvents(ctx, after, 7)
		if err != nil {
			t.Fatalf("ReadEvents(after=%d): %v", after, err)
		}
		if len(page) == 0 {
			if next != after {
				t.Errorf("empty page moved cursor: after=%d next=%d", after, next)
			}
			break
		}
		if next != page[len(page)-1].Cursor {
			t.Errorf("next=%d, last cursor=%d", next, page[len(page)-1].Cursor)
		}
		got = append(got, page...)
		after = next
	}
	if len(got) != n {
		t.Fatalf("read %d events, want %d", len(got), n)
	}
	for i, e := range got {
		if e.Cursor != int64(i+1) {
			t.Errorf("event %d cursor=%d, want %d", i, e.Cursor, i+1)
		}
		if want := fmt.Sprintf(`{"i":%d}`, i); string(e.Data) != want {
			t.Errorf("event %d data=%s, want %s", i, e.Data, want)
		}
	}
}

func blobRoundtrip(t *testing.T, s trace.Sink) {
	t.Helper()
	ctx := context.Background()
	h1, err := s.PutBlob(ctx, []byte("hello trace"))
	if err != nil {
		t.Fatal(err)
	}
	if h1 != trace.HashOf([]byte("hello trace")) {
		t.Errorf("hash=%s, want content hash %s", h1, trace.HashOf([]byte("hello trace")))
	}
	h2, err := s.PutBlob(ctx, []byte("hello trace")) // dedupe: idempotent (§7.2)
	if err != nil {
		t.Fatal(err)
	}
	if h2 != h1 {
		t.Errorf("duplicate PutBlob hash=%s, want %s", h2, h1)
	}
	b, err := s.GetBlob(ctx, h1)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello trace" {
		t.Errorf("GetBlob=%q", b)
	}
	if _, err := s.GetBlob(ctx, trace.Hash("deadbeef")); !errors.Is(err, trace.ErrNotFound) {
		t.Errorf("missing blob err=%v, want ErrNotFound", err)
	}
}

func leaseFencing(t *testing.T, s trace.Sink) {
	t.Helper()
	ctx := context.Background()
	l1, err := s.AcquireWriter(ctx, "sess")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireWriter(ctx, "sess"); !errors.Is(err, trace.ErrLeaseHeld) {
		t.Fatalf("second AcquireWriter err=%v, want ErrLeaseHeld", err)
	}
	ev := trace.Event{Type: "test.event", Time: time.Now(), SessionID: "sess"}
	// Append without the lease token is fenced while a lease is live (§7.2).
	if err := s.Append(ctx, ev); !errors.Is(err, trace.ErrStaleWriter) {
		t.Errorf("unleased Append err=%v, want ErrStaleWriter", err)
	}
	ctx1 := trace.WithLease(ctx, l1.Token())
	if err := s.Append(ctx1, ev); err != nil {
		t.Errorf("leased Append: %v", err)
	}
	if err := l1.Renew(ctx); err != nil {
		t.Errorf("Renew: %v", err)
	}
	if err := l1.Release(ctx); err != nil {
		t.Fatal(err)
	}
	l2, err := s.AcquireWriter(ctx, "sess")
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Release(ctx)
	if l2.Token() <= l1.Token() {
		t.Errorf("fencing token not monotonic: %d then %d", l1.Token(), l2.Token())
	}
	// The old writer is now stale (§7.2: the sink rejects stale writers).
	if err := s.Append(ctx1, ev); !errors.Is(err, trace.ErrStaleWriter) {
		t.Errorf("stale Append err=%v, want ErrStaleWriter", err)
	}
	if err := l1.Renew(ctx); !errors.Is(err, trace.ErrStaleWriter) {
		t.Errorf("stale Renew err=%v, want ErrStaleWriter", err)
	}
	// A different session is unaffected.
	other, err := s.AcquireWriter(ctx, "other")
	if err != nil {
		t.Errorf("AcquireWriter(other): %v", err)
	} else {
		other.Release(ctx)
	}
}

func atomicBatch(t *testing.T, s trace.Sink) {
	t.Helper()
	ctx := context.Background()
	batch := make([]trace.Event, 5)
	for i := range batch {
		batch[i] = trace.Event{Type: "test.batch", Time: time.Now(), SessionID: "sess"}
	}
	if err := s.Append(ctx, batch...); err != nil {
		t.Fatal(err)
	}
	evs, _, err := s.ReadEvents(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 5 {
		t.Fatalf("batch stored %d events, want 5", len(evs))
	}
	for i, e := range evs {
		if e.Cursor != int64(i+1) {
			t.Errorf("batch event %d cursor=%d", i, e.Cursor)
		}
	}
}
