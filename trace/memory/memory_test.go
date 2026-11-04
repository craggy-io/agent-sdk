package memory_test

import (
	"testing"

	"github.com/craggy-io/agent-sdk/trace"
	"github.com/craggy-io/agent-sdk/trace/memory"
	"github.com/craggy-io/agent-sdk/trace/sinktest"
)

func TestSinkConformance(t *testing.T) {
	sinktest.Run(t, func(t *testing.T) trace.Sink { return memory.New() })
}
