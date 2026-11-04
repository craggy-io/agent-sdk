package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Tool results the model reads have one shape, the registry's (see
// the capability registry error envelope): the payload — a tool's
// output verbatim, or on failure the error envelope below — then one
// "note: <text>" line per fact about the call itself. This file mirrors
// that shape for the results the loop and host make themselves, so the
// model sees one vocabulary whichever side failed. The registry's own
// results arrive already rendered; consumers test that both sides
// render identically.

// Code classifies a tool failure: the registry's closed set.
type Code string

const (
	CodeInvalid     Code = "invalid"     // the arguments are wrong; change them
	CodeNotFound    Code = "not_found"   // no such tool, handle or record
	CodeDenied      Code = "denied"      // not allowed (a budget, an ownership rule)
	CodeConflict    Code = "conflict"    // state changed; read what follows, then act
	CodeUnavailable Code = "unavailable" // try again later, unchanged
	CodeCancelled   Code = "cancelled"   // stopped before it finished
	CodeInternal    Code = "internal"    // a fault on this side
)

// Retryable reports whether a call that failed with c may succeed if made
// again unchanged, later: only CodeUnavailable.
func (c Code) Retryable() bool { return c == CodeUnavailable }

// ToolError is a coded tool failure: the error envelope's body.
type ToolError struct {
	Code      Code   `json:"code"`
	Reason    string `json:"reason,omitempty"` // stable lower_snake_case refinement of Code
	Message   string `json:"message"`          // what went wrong and what to change
	Retryable bool   `json:"retryable"`
}

func (e *ToolError) Error() string { return e.Message }

// Errorf is a coded error: returned from a built-in, it reaches the model
// with its code and reason (see Failure).
func Errorf(code Code, reason, format string, args ...any) error {
	return &ToolError{Code: code, Reason: reason, Message: fmt.Sprintf(format, args...), Retryable: code.Retryable()}
}

// Fail is the ToolResult for a failure the loop or session makes itself:
// IsError, with the error envelope as Text.
func Fail(code Code, reason, format string, args ...any) ToolResult {
	return Failure(Errorf(code, reason, format, args...))
}

// Failure is err as a failed ToolResult. A ToolError keeps its code and
// reason; a cancelled context is cancelled; anything else is internal.
func Failure(err error) ToolResult {
	var te *ToolError
	if !errors.As(err, &te) {
		code := CodeInternal
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			code = CodeCancelled
		}
		te = &ToolError{Code: code, Message: err.Error()}
	}
	te.Retryable = te.Code.Retryable()
	text, _ := json.Marshal(struct {
		Error *ToolError `json:"error"`
	}{te})
	return ToolResult{Text: string(text), IsError: true}
}

// AppendNotes is text followed by one "note: <note>" line per note.
func AppendNotes(text string, notes ...string) string {
	for _, n := range notes {
		text += "\nnote: " + n
	}
	return text
}

// SlowCall is how long a call must take before its result tells the model
// so (ElapsedNote).
const SlowCall = 5 * time.Second

// ElapsedNote is the note for a call that took d, e.g. "took 12.3s", or ""
// when d is under SlowCall.
func ElapsedNote(d time.Duration) string {
	if d < SlowCall {
		return ""
	}
	return "took " + d.Round(100*time.Millisecond).String()
}
