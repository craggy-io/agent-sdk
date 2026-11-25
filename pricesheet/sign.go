package pricesheet

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/craggy-io/foundation-kit/eventlog"
)

// domain starts every signed statement, so a sheet signature can never be
// passed off as an event-log head signature made with the same key, or the
// other way round.
const domain = "craggy/pricesheet/v1\n"

const algorithm = "ed25519"

// ErrUntrusted is a file whose signature doesn't check, or that can't be read.
var ErrUntrusted = errors.New("pricesheet: untrusted")

type signedSheet struct {
	Signature eventlog.Signature `json:"signature"`
	Sheet     json.RawMessage    `json:"sheet"`
}

// Sign checks the sheet and returns the published file: the sheet's exact
// bytes and their signature in one object.
func Sign(ctx context.Context, s eventlog.Signer, sheet Sheet) ([]byte, error) {
	if err := sheet.Check(); err != nil {
		return nil, err
	}
	doc, err := json.Marshal(sheet)
	if err != nil {
		return nil, err
	}
	sig, err := s.Sign(ctx, append([]byte(domain), doc...))
	if err != nil {
		return nil, fmt.Errorf("pricesheet: signing: %w", err)
	}
	signature, err := json.Marshal(eventlog.Signature{Key: s.KeyID(), Algorithm: algorithm, Value: base64.StdEncoding.EncodeToString(sig)})
	if err != nil {
		return nil, err
	}
	// Written by hand so the signed bytes are exactly the bytes published.
	var out bytes.Buffer
	out.WriteString(`{"signature":`)
	out.Write(signature)
	out.WriteString(`,"sheet":`)
	out.Write(doc)
	out.WriteString("}\n")
	return out.Bytes(), nil
}

// Verify checks a published file against the root keys and returns its
// sheet. Nothing in the sheet is read before the signature checks.
func Verify(data []byte, roots []eventlog.Key) (Sheet, error) {
	var ss signedSheet
	if err := json.Unmarshal(data, &ss); err != nil || len(ss.Sheet) == 0 {
		return Sheet{}, fmt.Errorf("%w: the file can't be read", ErrUntrusted)
	}
	var root *eventlog.Key
	for i := range roots {
		if roots[i].ID == ss.Signature.Key {
			root = &roots[i]
		}
	}
	if root == nil {
		return Sheet{}, fmt.Errorf("%w: signed by the key %q, which is not trusted", ErrUntrusted, ss.Signature.Key)
	}
	pub, err := base64.StdEncoding.DecodeString(root.Public)
	sig, sigErr := base64.StdEncoding.DecodeString(ss.Signature.Value)
	if ss.Signature.Algorithm != algorithm || root.Algorithm != algorithm || err != nil || sigErr != nil ||
		len(pub) != ed25519.PublicKeySize ||
		!ed25519.Verify(ed25519.PublicKey(pub), append([]byte(domain), ss.Sheet...), sig) {
		return Sheet{}, fmt.Errorf("%w: the signature by the key %q does not match", ErrUntrusted, ss.Signature.Key)
	}
	var sheet Sheet
	dec := json.NewDecoder(bytes.NewReader(ss.Sheet))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sheet); err != nil {
		return Sheet{}, fmt.Errorf("%w: the sheet can't be read: %v", ErrUntrusted, err)
	}
	if err := sheet.Check(); err != nil {
		return Sheet{}, fmt.Errorf("%w: %v", ErrUntrusted, err)
	}
	// Earlier signed sheets had only the three general tiers and embedding.
	// The signature was checked over their original bytes above.
	for _, slot := range []string{Decision, Audio} {
		if _, ok := sheet.Models[slot]; !ok {
			sheet.Models[slot] = Builtin().Models[slot]
		}
	}
	return sheet, nil
}
