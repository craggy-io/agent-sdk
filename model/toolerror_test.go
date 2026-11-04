package model_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/craggy-io/agent-sdk/model"
)

// A failure's text is the registry's error envelope: code, optional
// reason, message, and retryable spelled out.
func TestFailRendersTheErrorEnvelope(t *testing.T) {
	cases := []struct {
		res  model.ToolResult
		want string
	}{
		{model.Fail(model.CodeInvalid, "invalid_args", "invalid arguments: %s", `need "q"`),
			`{"error":{"code":"invalid","reason":"invalid_args","message":"invalid arguments: need \"q\"","retryable":false}}`},
		{model.Fail(model.CodeUnavailable, "", "pool full"),
			`{"error":{"code":"unavailable","message":"pool full","retryable":true}}`},
	}
	for _, tc := range cases {
		if !tc.res.IsError || tc.res.Text != tc.want {
			t.Errorf("Fail = %+v, want %s", tc.res, tc.want)
		}
	}
}

// Failure keeps a coded error's code and reason through wrapping; a
// cancelled context is cancelled, anything else internal.
func TestFailureClassifiesErrors(t *testing.T) {
	coded := fmt.Errorf("spawn: %w", model.Errorf(model.CodeNotFound, "unknown_handle", "host: unknown handle h1"))
	cases := []struct {
		err  error
		want string
	}{
		{coded, `{"error":{"code":"not_found","reason":"unknown_handle","message":"host: unknown handle h1","retryable":false}}`},
		{fmt.Errorf("wait: %w", context.Canceled), `{"error":{"code":"cancelled","message":"wait: context canceled","retryable":false}}`},
		{errors.New("boom"), `{"error":{"code":"internal","message":"boom","retryable":false}}`},
	}
	for _, tc := range cases {
		if res := model.Failure(tc.err); !res.IsError || res.Text != tc.want {
			t.Errorf("Failure(%v) = %+v, want %s", tc.err, res, tc.want)
		}
	}
}

func TestNotesAndElapsed(t *testing.T) {
	if got := model.AppendNotes(`{"a":1}`, "x", "took 6s"); got != `{"a":1}`+"\nnote: x\nnote: took 6s" {
		t.Fatalf("AppendNotes = %q", got)
	}
	if n := model.ElapsedNote(model.SlowCall - time.Millisecond); n != "" {
		t.Fatalf("a quick call got a note: %q", n)
	}
	if n := model.ElapsedNote(12345 * time.Millisecond); n != "took 12.3s" {
		t.Fatalf("ElapsedNote = %q", n)
	}
}
