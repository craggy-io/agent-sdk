package flow

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// CallKey is a call's content address: the capability and its input,
// canonicalised, so the same input in another key order has the same key.
// A host keys what a run's calls answered by it: Studio's step cache and
// test recordings, the harness's run journal.
func CallKey(ref string, in json.RawMessage) string {
	h := sha256.New()
	h.Write([]byte(ref + "\n"))
	h.Write(canonical(in))
	return hex.EncodeToString(h.Sum(nil))
}

// canonical re-encodes JSON with sorted keys and numbers as written.
func canonical(in json.RawMessage) []byte {
	var v any
	dec := json.NewDecoder(bytes.NewReader(in))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return in
	}
	out, _ := json.Marshal(v)
	return out
}
