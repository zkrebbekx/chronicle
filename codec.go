package chronicle

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Codec decodes a record's Data into a structure that [Log.Diff] can compare
// field by field.
//
// chronicle stores Data as opaque bytes and never needs to interpret it on the
// write path — a log with no codec configured still stores, supersedes and
// queries perfectly well. The codec exists solely so that Diff can answer
// "which fields changed" rather than "the bytes differ".
//
// Implementations must be safe for concurrent use.
type Codec interface {
	// Name identifies the codec in error messages.
	Name() string
	// Decode turns record data into a map of field names to values. Values may
	// be scalars, []any, or map[string]any, nested arbitrarily; [Log.Diff]
	// walks whatever structure it is given.
	//
	// Decode must return an error for input it cannot interpret. Returning an
	// empty map for undecodable data would make Diff silently report no
	// changes, and a change log that under-reports changes is worse than no
	// change log.
	Decode(data []byte) (map[string]any, error)
}

// JSONCodec decodes JSON objects. It is the default codec.
//
// Numbers are decoded as [json.Number] rather than float64. That keeps
// comparison exact — 9007199254740993 and 9007199254740992 are different
// values here, where float64 decoding would silently collapse them — and it
// keeps a diff's reported values in the notation they were written in.
//
// A JSON null decodes to an empty map rather than an error, so that a null
// record body is a usable tombstone: diffing from a populated record to a null
// one reports every field removed. Empty data is an error, because an empty
// byte slice is much more likely to be a bug than an intention.
type JSONCodec struct{}

// Name implements [Codec].
func (JSONCodec) Name() string { return "json" }

// Decode implements [Codec].
func (JSONCodec) Decode(data []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("empty data")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after top-level JSON value")
	}

	switch v := raw.(type) {
	case nil:
		return map[string]any{}, nil
	case map[string]any:
		return v, nil
	default:
		return nil, errors.New("top-level JSON value is not an object")
	}
}

// Compile-time assertion.
var _ Codec = JSONCodec{}
