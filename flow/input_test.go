package flow_test

import (
	"testing"

	"github.com/craggy-io/agent-sdk/flow"
)

// A key is a column name, which may have a dash after its first letter
// (craggy-data's rfcs keys on doc-id), and a host quotes it as one name. A
// key that could carry anything else into a query is refused, and so is a
// filter of more than one expression.
func TestInputCheck(t *testing.T) {
	for _, key := range []string{"id", "url", "doc-id", "CWE_ID", "a1"} {
		if err := (flow.Input{Dataset: "d/t", Key: key}).Check(); err != nil {
			t.Errorf("key %q: %v", key, err)
		}
	}
	for _, key := range []string{"", "-id", "1a", "doc id", "a`b", "a;b", "a.b", "a)"} {
		if err := (flow.Input{Dataset: "d/t", Key: key}).Check(); err == nil {
			t.Errorf("key %q passed", key)
		}
	}
	if err := (flow.Input{Dataset: "d/t", Key: "id", Where: "a = 1; DROP TABLE t"}).Check(); err == nil {
		t.Error("a filter of two statements passed")
	}
	if got := flow.QuoteColumn("doc-id"); got != "`doc-id`" {
		t.Errorf("QuoteColumn = %s", got)
	}
}
