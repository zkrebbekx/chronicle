package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/zkrebbekx/chronicle"
)

// writeRequest is the body of POST .../records and .../corrections.
//
// The trap fields at the bottom exist to be rejected. An audit service that
// silently ignored a caller-supplied actor or transaction time would teach
// its callers that sending them works; rejecting them with an explanation
// teaches the opposite, once, at integration time.
type writeRequest struct {
	Data      json.RawMessage   `json:"data"`
	ValidFrom *string           `json:"validFrom"`
	ValidTo   *string           `json:"validTo"`
	Reason    string            `json:"reason"`
	Meta      map[string]string `json:"meta"`
	// Subject opts the record's data into per-subject encryption, so a later
	// destroy-key renders it unrecoverable. Use a pseudonymous reference:
	// the subject string itself is stored in clear and survives shredding.
	Subject string `json:"subject"`

	// Trap fields — never accepted, always explained.
	Actor   json.RawMessage `json:"actor"`
	ActorID json.RawMessage `json:"actorId"`
	TxAt    json.RawMessage `json:"txAt"`
	TxFrom  json.RawMessage `json:"txFrom"`
	TxTo    json.RawMessage `json:"txTo"`
}

const (
	actorForbiddenMsg = "do not send an actor: the actor is stamped from the bearer token's configured identity. " +
		"An audit log that accepted caller-supplied actor claims would record fiction — " +
		"if you need to write as someone else, use that party's token"
	txForbiddenMsg = "do not send transaction time: it is assigned by the server when the write lands. " +
		"A transaction axis the caller could set would record what someone wanted to have believed, " +
		"not what was believed"
)

// decodeBody decodes a JSON request body strictly: unknown fields are
// rejected, trailing garbage is rejected, and the size is capped.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return &Error{Status: http.StatusRequestEntityTooLarge, Code: "body_too_large",
				Message: fmt.Sprintf("request body exceeds %d bytes", maxErr.Limit)}
		}
		return badRequest("invalid_body", "request body is not valid JSON for this endpoint: "+err.Error())
	}
	if dec.More() {
		return badRequest("invalid_body", "request body has trailing data after the JSON value")
	}
	return nil
}

// parseTime parses one RFC 3339 timestamp, naming the field in the error.
func parseTime(field, value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, badRequest("invalid_argument",
			fmt.Sprintf("%s must be an RFC 3339 timestamp such as 2026-03-01T00:00:00Z, got %q", field, value))
	}
	// Go's zero time renders as 0001-01-01T00:00:00Z, which is chronicle's
	// unbounded/now sentinel. Accepting it as a literal timestamp lets a
	// caller hit the sentinel by accident — an inverted-looking validTo that
	// silently means "still holds", a validAt that silently means "now". The
	// only way to ask for unbounded is to omit the field, so a literal zero is
	// a mistake, and rejected as one.
	if t.IsZero() {
		return time.Time{}, badRequest("invalid_argument",
			fmt.Sprintf("%s is chronicle's unbounded sentinel (%s); omit the field to mean unbounded or now", field, value))
	}
	return t.UTC(), nil
}

// storeGrain truncates a caller-supplied valid time to the store's microsecond
// resolution, so the value echoed in a write response is byte-identical to what
// a later read returns rather than the nanosecond input the store never held.
//
// This is applied only to the valid times a write records, never to a query
// instant: transaction time is assigned full-resolution by an in-memory store,
// and truncating a query's txAt downward would step it before the record's own
// TxFrom and miss it. Query points are matched as given.
func storeGrain(t time.Time) time.Time {
	return t.Truncate(time.Microsecond)
}

// queryTime parses an optional RFC 3339 query parameter; absent means zero.
func queryTime(q url.Values, name string) (time.Time, error) {
	v := q.Get(name)
	if v == "" {
		return time.Time{}, nil
	}
	return parseTime(name, v)
}

// queryBool parses an optional boolean query parameter; absent means false.
func queryBool(q url.Values, name string) (bool, error) {
	switch v := q.Get(name); v {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, badRequest("invalid_argument",
			fmt.Sprintf("%s must be \"true\" or \"false\", got %q", name, v))
	}
}

// parseIntent maps the wire spelling of an intent to the library's value,
// mirroring Intent.String.
func parseIntent(v string) (chronicle.Intent, error) {
	switch v {
	case "assert":
		return chronicle.IntentAssert, nil
	case "correction":
		return chronicle.IntentCorrection, nil
	case "remainder":
		return chronicle.IntentRemainder, nil
	default:
		return 0, badRequest("unknown_intent",
			fmt.Sprintf("intent must be one of assert, correction, remainder; got %q", v))
	}
}

// handlePut is POST /v1/{kind}/{entity}/records.
func (s *Server) handlePut(w http.ResponseWriter, r *http.Request) {
	s.handleWrite(w, r, false)
}

// handleCorrect is POST /v1/{kind}/{entity}/corrections. Same shape as a
// put; the record is marked IntentCorrection, which is what makes "when did
// we discover we were wrong" answerable later.
func (s *Server) handleCorrect(w http.ResponseWriter, r *http.Request) {
	s.handleWrite(w, r, true)
}

func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request, correct bool) {
	kind, entity := r.PathValue("kind"), r.PathValue("entity")

	var req writeRequest
	if err := decodeBody(w, r, &req); err != nil {
		s.respondError(w, r, err)
		return
	}
	if req.Actor != nil || req.ActorID != nil {
		s.respondError(w, r, badRequest("actor_forbidden", actorForbiddenMsg))
		return
	}
	if req.TxAt != nil || req.TxFrom != nil || req.TxTo != nil {
		s.respondError(w, r, badRequest("tx_forbidden", txForbiddenMsg))
		return
	}
	if req.Data == nil {
		s.respondError(w, r, badRequest("invalid_body", "data is required: the entity's state as a JSON value"))
		return
	}
	// validFrom is required over HTTP even though the library accepts a zero
	// (unbounded) lower bound: in a JSON body an absent validFrom is far more
	// likely a forgotten field than an assertion that the fact has always
	// been true. See the phase 4 correction in docs/DESIGN.md.
	if req.ValidFrom == nil {
		s.respondError(w, r, badRequest("invalid_body",
			"validFrom is required (RFC 3339). To record a fact with no known end, omit validTo instead"))
		return
	}
	validFrom, err := parseTime("validFrom", *req.ValidFrom)
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	validFrom = storeGrain(validFrom)
	var validTo time.Time // zero = unbounded ("and it still holds")
	if req.ValidTo != nil {
		validTo, err = parseTime("validTo", *req.ValidTo)
		if err != nil {
			s.respondError(w, r, err)
			return
		}
		validTo = storeGrain(validTo)
	}

	// Compact the data so equivalent submissions store identical bytes; the
	// hash chain, when enabled, covers the bytes as stored.
	var compact bytes.Buffer
	if err := json.Compact(&compact, req.Data); err != nil {
		s.respondError(w, r, badRequest("invalid_body", "data is not valid JSON: "+err.Error()))
		return
	}

	var opts []chronicle.WriteOption
	if req.Reason != "" {
		opts = append(opts, chronicle.WithReason(req.Reason))
	}
	if req.Meta != nil {
		opts = append(opts, chronicle.WithMeta(req.Meta))
	}
	if req.Subject != "" {
		opts = append(opts, chronicle.WithSubject(req.Subject))
	}

	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	actor := principal.Actor
	write := s.log.Put
	if correct {
		write = s.log.Correct
	}
	res, err := write(r.Context(), kind, entity, compact.Bytes(), validFrom, validTo, actor, opts...)
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toResultDTO(res))
}

// handleGet is GET /v1/{kind}/{entity}?validAt=&txAt=. Absent instants mean
// "now" — this is a point lookup about the present unless pinned, matching
// chronicle.As. Contrast with /v1/records, where absent means "no filter".
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	kind, entity, ok := s.pathKindEntity(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	validAt, err := queryTime(q, "validAt")
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	txAt, err := queryTime(q, "txAt")
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	rec, err := s.log.Get(r.Context(), kind, entity,
		chronicle.As{ValidAt: validAt, TxAt: txAt})
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toRecordDTO(*rec))
}

// handleHistory is GET /v1/{kind}/{entity}/history with axis filters.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	kind, entity, ok := s.pathKindEntity(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	var opts []chronicle.HistoryOption

	currentOnly, err := queryBool(q, "currentOnly")
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	if currentOnly {
		opts = append(opts, chronicle.CurrentOnly())
	}
	descending, err := queryBool(q, "descending")
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	if descending {
		opts = append(opts, chronicle.Descending())
	}
	validFrom, err := queryTime(q, "validFrom")
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	validTo, err := queryTime(q, "validTo")
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	if !validFrom.IsZero() || !validTo.IsZero() {
		opts = append(opts, chronicle.InValidRange(chronicle.Between(validFrom, validTo)))
	}
	txFrom, err := queryTime(q, "txFrom")
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	txTo, err := queryTime(q, "txTo")
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	if !txFrom.IsZero() || !txTo.IsZero() {
		opts = append(opts, chronicle.InTxRange(chronicle.Between(txFrom, txTo)))
	}
	if v := q.Get("intent"); v != "" {
		intent, err := parseIntent(v)
		if err != nil {
			s.respondError(w, r, err)
			return
		}
		opts = append(opts, chronicle.WithIntent(intent))
	}
	if v := q.Get("actorId"); v != "" {
		opts = append(opts, chronicle.ByActor(v))
	}
	if v := q.Get("limit"); v != "" {
		n, err := parseLimit(v)
		if err != nil {
			s.respondError(w, r, err)
			return
		}
		opts = append(opts, chronicle.Limit(n))
	}

	recs, err := s.log.History(r.Context(), kind, entity, opts...)
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, recordsResponse{Records: toRecordDTOs(recs)})
}

// handleTimeline is GET /v1/{kind}/{entity}/timeline?txAt=: the valid-time
// sequence as believed at one transaction instant. Absent txAt means now.
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	kind, entity, ok := s.pathKindEntity(w, r)
	if !ok {
		return
	}
	txAt, err := queryTime(r.URL.Query(), "txAt")
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	recs, err := s.log.Timeline(r.Context(), kind, entity,
		chronicle.As{TxAt: txAt})
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, recordsResponse{Records: toRecordDTOs(recs)})
}

// handleDiff is GET /v1/{kind}/{entity}/diff with two bitemporal points.
// Absent instants mean "now", per chronicle.As, so pinning only fromTxAt
// diffs a past belief against the present one.
func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	kind, entity, ok := s.pathKindEntity(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	var from, to chronicle.As
	var err error
	if from.ValidAt, err = queryTime(q, "fromValidAt"); err != nil {
		s.respondError(w, r, err)
		return
	}
	if from.TxAt, err = queryTime(q, "fromTxAt"); err != nil {
		s.respondError(w, r, err)
		return
	}
	if to.ValidAt, err = queryTime(q, "toValidAt"); err != nil {
		s.respondError(w, r, err)
		return
	}
	if to.TxAt, err = queryTime(q, "toTxAt"); err != nil {
		s.respondError(w, r, err)
		return
	}
	delta, err := s.log.Diff(r.Context(), kind, entity, from, to)
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toDeltaDTO(delta))
}

// handleFieldHistory is GET /v1/{kind}/{entity}/field-history?path=&validAt=&descending=:
// the single-field audit trail. It walks how the recorded value of one field
// changed over transaction time, holding the valid point fixed — who changed
// it, when we learned it, and whether it was an assertion or a correction.
//
// path is required and is an RFC 6901 JSON Pointer (URL-encode the slashes).
// validAt is optional and, absent, means now — this is a point in valid time,
// like Get and Diff, not the "no restriction" a cross-entity query would read
// an absent instant as.
func (s *Server) handleFieldHistory(w http.ResponseWriter, r *http.Request) {
	kind, entity, ok := s.pathKindEntity(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	// path is required. It is URL-decoded by Query() already; reject a NUL like
	// every other caller-supplied string, so a malformed pointer is the
	// library's 400 and a NUL is this layer's, never a driver 500.
	path := q.Get("path")
	if path == "" {
		s.respondError(w, r, badRequest("invalid_argument",
			"path is required: an RFC 6901 JSON Pointer such as /salary or /address/city (URL-encode the slashes)"))
		return
	}
	if err := rejectNUL("path", path); err != nil {
		s.respondError(w, r, err)
		return
	}
	validAt, err := queryTime(q, "validAt")
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	descending, err := queryBool(q, "descending")
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	var opts []chronicle.FieldHistoryOption
	if descending {
		opts = append(opts, chronicle.FieldHistoryDescending())
	}

	revs, err := s.log.FieldHistory(r.Context(), kind, entity, path,
		chronicle.As{ValidAt: validAt}, opts...)
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	resp, err := toFieldHistoryResponse(path, validAt, revs)
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

const (
	defaultQueryLimit = 100
	maxQueryLimit     = 1000
)

// parseLimit reads an explicit limit query parameter, the same way for every
// endpoint that accepts one: a positive integer, at most maxQueryLimit. Zero
// is rejected rather than silently meaning "unbounded" on one endpoint and
// "default" on another — a page size of zero asks for nothing, which is always
// a mistake. Absence is handled by each caller (a full history, or the default
// page size), never here.
func parseLimit(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, badRequest("invalid_argument",
			fmt.Sprintf("limit must be a positive integer, got %q", v))
	}
	if n > maxQueryLimit {
		return 0, badRequest("invalid_argument",
			fmt.Sprintf("limit must be at most %d; page with the cursor instead", maxQueryLimit))
	}
	return n, nil
}

// handleQuery is GET /v1/records: the cross-entity scan. Zero time filters
// mean "no restriction" here — a scan that defaulted to "now" would silently
// hide everything superseded, which is most of an audit log.
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := rejectNUL("kind", q.Get("kind"), "entityId", q.Get("entityId"), "actorId", q.Get("actorId")); err != nil {
		s.respondError(w, r, err)
		return
	}
	query := chronicle.Query{
		Kind:     q.Get("kind"),
		EntityID: q.Get("entityId"),
		ActorID:  q.Get("actorId"),
		// The cursor passes through opaquely; the store validates it.
		After: chronicle.Cursor(q.Get("cursor")),
		Limit: defaultQueryLimit,
	}
	var err error
	if query.CurrentOnly, err = queryBool(q, "currentOnly"); err != nil {
		s.respondError(w, r, err)
		return
	}
	if query.Descending, err = queryBool(q, "descending"); err != nil {
		s.respondError(w, r, err)
		return
	}
	if v := q.Get("intent"); v != "" {
		if query.Intent, err = parseIntent(v); err != nil {
			s.respondError(w, r, err)
			return
		}
		query.HasIntent = true
	}
	if query.ValidAt, err = queryTime(q, "validAt"); err != nil {
		s.respondError(w, r, err)
		return
	}
	if query.TxAt, err = queryTime(q, "txAt"); err != nil {
		s.respondError(w, r, err)
		return
	}
	var vf, vt, tf, tt time.Time
	if vf, err = queryTime(q, "validFrom"); err != nil {
		s.respondError(w, r, err)
		return
	}
	if vt, err = queryTime(q, "validTo"); err != nil {
		s.respondError(w, r, err)
		return
	}
	query.Valid = chronicle.Between(vf, vt)
	if tf, err = queryTime(q, "txFrom"); err != nil {
		s.respondError(w, r, err)
		return
	}
	if tt, err = queryTime(q, "txTo"); err != nil {
		s.respondError(w, r, err)
		return
	}
	query.Tx = chronicle.Between(tf, tt)
	if v := q.Get("limit"); v != "" {
		n, err := parseLimit(v)
		if err != nil {
			s.respondError(w, r, err)
			return
		}
		query.Limit = n
	}

	recs, cursor, err := s.log.Query(r.Context(), query)
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, recordsResponse{
		Records: toRecordDTOs(recs),
		Cursor:  string(cursor),
	})
}
