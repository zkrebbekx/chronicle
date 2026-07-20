package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/zkrebbekx/chronicle"
	"github.com/zkrebbekx/chronicle/retain"
)

// errorBody is the wire shape of every error chronicled returns. The code is
// a stable string mirroring chronicle's sentinel taxonomy; clients switch on
// it, never on the human-readable message.
type errorBody struct {
	Error  string `json:"error"`
	Code   string `json:"code"`
	Detail any    `json:"detail,omitempty"`
}

// Error is an error minted by a handler itself — a parse failure, an auth
// failure, a forbidden field — carrying its own status and code.
type Error struct {
	Status  int
	Code    string
	Message string
	Detail  any
}

// Error implements the error interface.
func (e *Error) Error() string { return e.Message }

func badRequest(code, msg string) *Error {
	return &Error{Status: http.StatusBadRequest, Code: code, Message: msg}
}

// mapping ties one chronicle sentinel to its HTTP rendering.
type mapping struct {
	sentinel error
	status   int
	code     string
}

// errorTable maps chronicle's errors.Is taxonomy onto HTTP. Order matters
// only where one concrete error matches two sentinels: a *ShredError matches
// ErrShredded and often ErrKeyDestroyed too, and the read should report
// "shredded", so ErrShredded is checked first.
//
// ErrMissingActor and ErrZeroTxTime are mapped to 500 deliberately: the token
// table stamps the actor and the store stamps transaction time, so neither
// can be a caller's fault. If either surfaces, chronicled has a bug and the
// response should say "internal", loudly, rather than blame the request.
var errorTable = []mapping{
	{chronicle.ErrShredded, http.StatusGone, "shredded"},
	{chronicle.ErrKeyDestroyed, http.StatusGone, "key_destroyed"},
	{chronicle.ErrNoKeyring, http.StatusNotImplemented, "no_keyring"},
	{chronicle.ErrNotFound, http.StatusNotFound, "not_found"},
	{chronicle.ErrNoChain, http.StatusNotFound, "no_chain"},
	{chronicle.ErrInvalidInterval, http.StatusBadRequest, "invalid_interval"},
	{chronicle.ErrInvalidCursor, http.StatusBadRequest, "invalid_cursor"},
	{chronicle.ErrInvalidPath, http.StatusBadRequest, "invalid_path"},
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
	{chronicle.ErrCodec, http.StatusUnprocessableEntity, "codec"},
	{retain.ErrNoPolicy, http.StatusBadRequest, "no_policy"},
	{retain.ErrInvalidPolicy, http.StatusBadRequest, "invalid_policy"},
	{retain.ErrNoDeleter, http.StatusNotImplemented, "unsupported"},
	{chronicle.ErrClosed, http.StatusServiceUnavailable, "unavailable"},
	{chronicle.ErrMissingActor, http.StatusInternalServerError, "internal"},
	{chronicle.ErrZeroTxTime, http.StatusInternalServerError, "internal"},
}

// mapError resolves an error from the chronicle libraries to its HTTP status
// and code. The bool reports whether the error was recognised; unrecognised
// errors are the server's problem, not the caller's, and become a generic 500
// so that no internal error string — a driver message, a SQL fragment — leaks
// into a response body.
func mapError(err error) (status int, code string, ok bool) {
	for _, m := range errorTable {
		if errors.Is(err, m.sentinel) {
			return m.status, m.code, true
		}
	}
	return http.StatusInternalServerError, "internal", false
}

// errorDetail extracts a structured detail object for errors that carry
// coordinates worth returning — currently the not-found lookup, whose
// resolved bitemporal point tells the caller exactly what was asked.
func errorDetail(err error) any {
	var nf *chronicle.NotFoundError
	if errors.As(err, &nf) {
		return map[string]string{
			"kind":     nf.Kind,
			"entityId": nf.EntityID,
			"validAt":  fmtTime(nf.As.ValidAt),
			"txAt":     fmtTime(nf.As.TxAt),
		}
	}
	return nil
}

// respondError renders any error as the JSON error contract. Handler-minted
// *Error values pass through as-is; chronicle errors go through the mapping
// table; everything else — and anything the table maps to a 5xx — is logged
// server-side and reported as a generic "internal" without the underlying
// message, which may name tables, constraints or hosts.
func (s *Server) respondError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		writeJSON(w, apiErr.Status, errorBody{Error: apiErr.Message, Code: apiErr.Code, Detail: apiErr.Detail})
		return
	}
	status, code, known := mapError(err)
	if !known || status >= http.StatusInternalServerError {
		s.logger.Error("internal error", "method", r.Method, "path", r.URL.Path, "err", err.Error())
		msg := "internal error"
		if status != http.StatusInternalServerError {
			msg = strings.ToLower(http.StatusText(status))
		}
		writeJSON(w, status, errorBody{Error: msg, Code: code})
		return
	}
	writeJSON(w, status, errorBody{Error: err.Error(), Code: code, Detail: errorDetail(err)})
}
