package workflow

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// parallel() branches commit journal results concurrently (§6.2); the
// journal maps must be mutex-guarded or this is a concurrent-map-write
// crash under -race.
func TestJournalConcurrentCommitLookup(t *testing.T) {
	j := newJournal(nil, nil, "run-1")
	ctx := context.Background()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				key := fmt.Sprintf("k-%d-%d", w, i)
				ent := &journalEntry{Key: key, Builtin: "agent"}
				if err := j.commit(ctx, nil, ent, nil); err != nil {
					t.Error(err)
					return
				}
				if _, ok := j.lookup(key); !ok {
					t.Errorf("lookup(%s) miss after commit", key)
					return
				}
				_ = j.emitOp(ctx, nil, "operation.intent",
					&opState{Key: opKey(key), Tool: "t", Effect: "read-only"}, nil)
				if _, ok := j.opSnapshot(opKey(key)); !ok {
					t.Errorf("opSnapshot(%s) miss after intent", key)
					return
				}
			}
		}(w)
	}
	wg.Wait()
}
