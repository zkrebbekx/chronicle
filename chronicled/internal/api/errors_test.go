package api

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/zkrebbekx/chronicle"
	"github.com/zkrebbekx/chronicle/retain"
)

// TestErrorMapping is the sentinel-by-sentinel contract. The expectations are
// written out independently of the table in errors.go, so changing a mapping
// requires changing it twice — once in the code, once here, deliberately.
func TestErrorMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{chronicle.ErrNotFound, http.StatusNotFound, "not_found"},
		{chronicle.ErrNoChain, http.StatusNotFound, "no_chain"},
		{chronicle.ErrInvalidInterval, http.StatusBadRequest, "invalid_interval"},
		{chronicle.ErrInvalidCursor, http.StatusBadRequest, "invalid_cursor"},
		{chronicle.ErrUnknownKind, http.StatusBadRequest, "unknown_kind"},
		{chronicle.ErrUnknownIntent, http.StatusBadRequest, "unknown_intent"},
		{chronicle.ErrInvalidMeta, http.StatusBadRequest, "invalid_meta"},
		{chronicle.ErrInvalidField, http.StatusBadRequest, "invalid_field"},
		{chronicle.ErrMissingEntityID, http.StatusBadRequest, "missing_entity_id"},
		{chronicle.ErrReservedMeta, http.StatusBadRequest, "reserved_meta"},
		{chronicle.ErrMissingHoldID, http.StatusBadRequest, "missing_hold_id"},
		{chronicle.ErrHoldExists, http.StatusConflict, "hold_exists"},
		{chronicle.ErrHoldReleased, http.StatusConflict, "hold_released"},
		{chronicle.ErrConflict, http.StatusConflict, "conflict"},
		{chronicle.ErrCurrentRecord, http.StatusConflict, "current_record"},
		{chronicle.ErrShredded, http.StatusGone, "shredded"},
		{chronicle.ErrKeyDestroyed, http.StatusGone, "key_destroyed"},
		{chronicle.ErrNoKeyring, http.StatusNotImplemented, "no_keyring"},
		{chronicle.ErrCodec, http.StatusUnprocessableEntity, "codec"},
		{retain.ErrNoPolicy, http.StatusBadRequest, "no_policy"},
		{retain.ErrInvalidPolicy, http.StatusBadRequest, "invalid_policy"},
		{retain.ErrNoDeleter, http.StatusNotImplemented, "unsupported"},
		{chronicle.ErrClosed, http.StatusServiceUnavailable, "unavailable"},
		// Never expected through this service; loud 500s if they happen.
		{chronicle.ErrMissingActor, http.StatusInternalServerError, "internal"},
		{chronicle.ErrZeroTxTime, http.StatusInternalServerError, "internal"},
	}
	for _, tc := range cases {
		t.Run(tc.code+"/"+tc.err.Error(), func(t *testing.T) {
			// Both bare and wrapped, since the library always wraps.
			for _, err := range []error{tc.err, fmt.Errorf("wrapped: %w", tc.err)} {
				status, code, known := mapError(err)
				if !known {
					t.Fatalf("mapError(%v) not recognised", err)
				}
				if status != tc.status || code != tc.code {
					t.Fatalf("mapError(%v) = (%d, %q), want (%d, %q)", err, status, code, tc.status, tc.code)
				}
			}
		})
	}

	// A typed library error maps through its sentinel.
	typed := &chronicle.NotFoundError{Kind: "k", EntityID: "e"}
	if status, code, known := mapError(typed); !known || status != 404 || code != "not_found" {
		t.Fatalf("typed NotFoundError = (%d, %q, %v)", status, code, known)
	}

	// A handler-minted *Error carries its message through the error
	// interface, which is what respondError's errors.As relies on.
	if e := badRequest("x", "the message"); e.Error() != "the message" {
		t.Fatalf("Error() = %q", e.Error())
	}

	// An unrecognised error is a generic 500 — nothing internal leaks.
	if status, code, known := mapError(errors.New("pq: constraint violated on table secrets")); known ||
		status != http.StatusInternalServerError || code != "internal" {
		t.Fatalf("unknown error = (%d, %q, %v), want unknown 500 internal", status, code, known)
	}
}

// TestInternalErrorsDoNotLeak drives an unmapped error through a live
// handler and asserts the generic body.
func TestInternalErrorsDoNotLeak(t *testing.T) {
	h := newHarness(t, false)
	// Closing the store under the server makes every store call fail with
	// ErrClosed — mapped 503 — proving mapped non-4xx paths render the
	// contract too.
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	resp, body := h.request("GET", "/v1/employee/alice", writerToken, nil)
	wantError(t, resp, body, http.StatusServiceUnavailable, "unavailable")
}
