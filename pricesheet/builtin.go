package pricesheet

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/craggy-io/foundation-kit/eventlog"
)

//go:embed builtin.json
var builtinJSON []byte

// Builtin is the sheet compiled into this module, version 0: what a Reader
// holds before its first fetch and whenever nothing better can be had.
func Builtin() Sheet {
	var s Sheet
	dec := json.NewDecoder(bytes.NewReader(builtinJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		panic(fmt.Sprintf("pricesheet: builtin.json: %v", err)) // a build error; TestBuiltin catches it
	}
	return s
}

// roots are the keys a published sheet may be signed with, "id:base64"
// separated by commas. Rotating the key is a release that lists both until
// every consumer has it.
const roots = "craggy-prices-1:cgCeDS1/wR8G83Y2LGGuqgRsZH6MKsUvDuqgXXEw3bM="

// TrustedRoots returns the keys a published sheet may be signed with.
func TrustedRoots() []eventlog.Key {
	keys, err := eventlog.ParseRoots(roots)
	if err != nil {
		panic(err) // a build error; TestTrustedRoots catches it
	}
	return keys
}
