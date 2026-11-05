package openrouter

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/foundation-kit/retryafter"
)

// codeString normalises an error.code that may be a JSON string or number.
func codeString(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	return s
}

// MapHTTPError maps a non-200 OpenRouter response to *model.Error (§8):
// Transient for 408/429/502/503, ContextExceeded in any of the forms
// OpenRouter returns it, RetryAfter from the header, Raw and Provider from
// error.metadata so the trace shows the upstream message. A top-level
// "usage" object in the body, if the API ever sends one with an error,
// is carried on Error.Usage (OpenRouter's documented error body has
// none, so this is normally nil).
func MapHTTPError(status int, body []byte, retryAfter string) *model.Error {
	e := &model.Error{Status: status, Raw: string(body), Transient: model.TransientStatus(status)}
	var wrap struct {
		Error *orErrObj `json:"error"`
		Usage *orUsage  `json:"usage"`
	}
	if json.Unmarshal(body, &wrap) == nil {
		if wrap.Error != nil {
			fillFromErrObj(e, wrap.Error)
		}
		if wrap.Usage != nil {
			u := wrap.Usage.toModel()
			e.Usage = &u
		}
	}
	if e.Message == "" {
		e.Message = http.StatusText(status)
	}
	if d, _ := retryafter.Delay(retryAfter, time.Now()); d > 0 {
		e.RetryAfter = d
	}
	finalise(e)
	return e
}

// mapChunkError maps an in-stream (HTTP 200) error chunk (§8).
func mapChunkError(o *orErrObj) *model.Error {
	e := &model.Error{}
	fillFromErrObj(e, o)
	if st, err := strconv.Atoi(e.Code); err == nil && st >= 100 {
		e.Status = st
		e.Transient = model.TransientStatus(st)
	}
	finalise(e)
	return e
}

func fillFromErrObj(e *model.Error, o *orErrObj) {
	e.Code = codeString(o.Code)
	e.Message = o.Message
	e.Provider = o.Provider
	if o.Metadata != nil {
		if o.Metadata.ProviderName != "" && e.Provider == "" {
			e.Provider = o.Metadata.ProviderName
		}
		if len(o.Metadata.Raw) > 0 {
			var s string
			if json.Unmarshal(o.Metadata.Raw, &s) == nil {
				e.Raw = s
			} else {
				e.Raw = string(o.Metadata.Raw)
			}
		}
	}
}

// finalise sets ContextExceeded and a fallback Code.
func finalise(e *model.Error) {
	hay := strings.ToLower(e.Code + "\n" + e.Message + "\n" + e.Raw)
	for _, pat := range []string{
		"context_length_exceeded", "context length exceeded",
		"maximum context length", "prompt is too long",
		"exceeds the context window", "context window exceeded",
	} {
		if strings.Contains(hay, pat) {
			e.ContextExceeded = true
			break
		}
	}
	if e.Code == "" {
		switch {
		case e.ContextExceeded:
			e.Code = "context_length_exceeded"
		case e.Status != 0:
			e.Code = "http_" + strconv.Itoa(e.Status)
		default:
			e.Code = "provider_error"
		}
	}
}
