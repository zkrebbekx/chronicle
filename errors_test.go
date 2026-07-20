package chronicle

import (
	"errors"
	"testing"
)

// The error types are what a caller pattern-matches on, so their messages and
// their unwrapping are API rather than diagnostics: a message that names the
// wrong field, or an Unwrap that drops the cause, breaks callers as surely as a
// changed signature would.

func TestKindError(t *testing.T) {
	t.Run("given a rejected kind", func(t *testing.T) {
		t.Run("when the caller named one", func(t *testing.T) {
			err := &KindError{Kind: "porcupine", Err: ErrUnknownKind}
			t.Run("then the message quotes it", func(t *testing.T) {
				if got, want := err.Error(), `chronicle: unknown kind "porcupine"`; got != want {
					t.Fatalf("Error() = %q; want %q", got, want)
				}
			})
			t.Run("then it unwraps to the sentinel", func(t *testing.T) {
				if !errors.Is(err, ErrUnknownKind) {
					t.Fatalf("errors.Is(%v, ErrUnknownKind) = false", err)
				}
			})
		})

		t.Run("when the caller named none", func(t *testing.T) {
			err := &KindError{Err: ErrUnknownKind}
			t.Run("then the message says a kind was required rather than quoting an empty one", func(t *testing.T) {
				if got, want := err.Error(), "chronicle: kind required"; got != want {
					t.Fatalf("Error() = %q; want %q", got, want)
				}
			})
		})
	})
}

func TestCodecError(t *testing.T) {
	cause := errors.New("unexpected end of JSON input")

	t.Run("given a codec failure on a known record", func(t *testing.T) {
		err := &CodecError{Codec: "json", RecordID: "r-1", Err: cause}
		t.Run("when it is rendered", func(t *testing.T) {
			t.Run("then it names the codec, the record and the cause", func(t *testing.T) {
				want := "chronicle: codec json: record r-1: unexpected end of JSON input"
				if got := err.Error(); got != want {
					t.Fatalf("Error() = %q; want %q", got, want)
				}
			})
		})
		t.Run("when it is unwrapped", func(t *testing.T) {
			t.Run("then the underlying failure is reachable", func(t *testing.T) {
				if got := errors.Unwrap(err); !errors.Is(got, cause) {
					t.Fatalf("Unwrap() = %v; want %v — a caller that cannot reach the cause "+
						"cannot tell a malformed body from an unreadable one", got, cause)
				}
			})
			t.Run("then it still matches the sentinel", func(t *testing.T) {
				if !errors.Is(err, ErrCodec) {
					t.Fatalf("errors.Is(%v, ErrCodec) = false", err)
				}
			})
		})
	})

	t.Run("given a codec failure with no record to blame", func(t *testing.T) {
		err := &CodecError{Codec: "json", Err: cause}
		t.Run("when it is rendered", func(t *testing.T) {
			t.Run("then the record clause is omitted rather than left empty", func(t *testing.T) {
				want := "chronicle: codec json: unexpected end of JSON input"
				if got := err.Error(); got != want {
					t.Fatalf("Error() = %q; want %q", got, want)
				}
			})
		})
	})
}

func TestIntentError(t *testing.T) {
	t.Run("given a query naming an undefined intent", func(t *testing.T) {
		err := &IntentError{Intent: Intent(200), Err: ErrUnknownIntent}
		t.Run("when it is rendered", func(t *testing.T) {
			t.Run("then the message carries the offending value", func(t *testing.T) {
				if got, want := err.Error(), "chronicle: unknown intent 200"; got != want {
					t.Fatalf("Error() = %q; want %q", got, want)
				}
			})
		})
		t.Run("when it is matched", func(t *testing.T) {
			t.Run("then it unwraps to its own sentinel and not the kind's", func(t *testing.T) {
				if !errors.Is(err, ErrUnknownIntent) {
					t.Fatalf("errors.Is(%v, ErrUnknownIntent) = false", err)
				}
				if errors.Is(err, ErrUnknownKind) {
					t.Fatal("an intent error must not match ErrUnknownKind; the two name " +
						"different caller mistakes")
				}
			})
		})
	})
}
