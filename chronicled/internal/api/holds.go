package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/zkrebbekx/chronicle"
)

// placeHoldRequest is the body of POST /v1/holds.
//
// PlacedBy is stamped from the admin token's actor, exactly as record writes
// stamp theirs — the no-actor-in-body rule holds for the compliance surface
// too, and a hold whose placer could be asserted by the caller would prove
// nothing about who placed it. The trap fields reject the attempt with an
// explanation rather than ignoring it.
type placeHoldRequest struct {
	ID string `json:"id"`
	// Kind and EntityID scope the hold. Both empty holds everything;
	// EntityID without Kind holds that entity ID across all kinds.
	Kind     string `json:"kind"`
	EntityID string `json:"entityId"`
	// EffectiveFrom may be backdated — deliberately. FRCP 37(e)'s
	// preservation duty attaches on anticipation of litigation, judged after
	// the fact, so an operator must be able to assert "our duty attached
	// last month". Absent means the hold is effective over all time until
	// released.
	EffectiveFrom *string `json:"effectiveFrom"`
	Reason        string  `json:"reason"`

	// Trap fields — never accepted, always explained.
	PlacedBy   json.RawMessage `json:"placedBy"`
	PlacedAt   json.RawMessage `json:"placedAt"`
	ReleasedBy json.RawMessage `json:"releasedBy"`
	ReleasedAt json.RawMessage `json:"releasedAt"`
}

// releaseHoldRequest is the body of POST /v1/holds/{id}/release. ReleasedBy
// is stamped from the token; ReleasedAt from the store.
type releaseHoldRequest struct {
	Reason string `json:"reason"`

	ReleasedBy json.RawMessage `json:"releasedBy"`
	ReleasedAt json.RawMessage `json:"releasedAt"`
}

const holdActorForbiddenMsg = "do not send placedBy or releasedBy: hold attribution is stamped from the " +
	"bearer token's configured identity, for the same reason record writes are — an audit control whose " +
	"operator could be asserted by the caller would prove nothing"

const holdTimeForbiddenMsg = "do not send placedAt or releasedAt: the store assigns them. " +
	"effectiveFrom is the operator-asserted instant, and it may be backdated"

// handlePlaceHold is POST /v1/holds (admin).
func (s *Server) handlePlaceHold(w http.ResponseWriter, r *http.Request) {
	if s.holds == nil {
		s.respondError(w, r, &Error{Status: http.StatusNotImplemented, Code: "unsupported",
			Message: "the configured store does not support legal holds"})
		return
	}
	var req placeHoldRequest
	if err := decodeBody(w, r, &req); err != nil {
		s.respondError(w, r, err)
		return
	}
	if req.PlacedBy != nil || req.ReleasedBy != nil {
		s.respondError(w, r, badRequest("actor_forbidden", holdActorForbiddenMsg))
		return
	}
	if req.PlacedAt != nil || req.ReleasedAt != nil {
		s.respondError(w, r, badRequest("tx_forbidden", holdTimeForbiddenMsg))
		return
	}
	var effectiveFrom time.Time
	if req.EffectiveFrom != nil {
		var err error
		if effectiveFrom, err = parseTime("effectiveFrom", *req.EffectiveFrom); err != nil {
			s.respondError(w, r, err)
			return
		}
	}
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}

	hold, err := s.holds.PlaceHold(r.Context(), chronicle.Hold{
		ID:            req.ID,
		Kind:          req.Kind,
		EntityID:      req.EntityID,
		EffectiveFrom: effectiveFrom,
		Reason:        req.Reason,
		PlacedBy:      principal.Actor,
	})
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toHoldDTO(hold))
}

// handleReleaseHold is POST /v1/holds/{id}/release (admin). The hold row
// survives with its release half filled in; releasing twice is a 409, not a
// quiet no-op, because a second release would rewrite the first one's
// attribution.
func (s *Server) handleReleaseHold(w http.ResponseWriter, r *http.Request) {
	if s.holds == nil {
		s.respondError(w, r, &Error{Status: http.StatusNotImplemented, Code: "unsupported",
			Message: "the configured store does not support legal holds"})
		return
	}
	var req releaseHoldRequest
	if err := decodeBody(w, r, &req); err != nil {
		s.respondError(w, r, err)
		return
	}
	if req.ReleasedBy != nil {
		s.respondError(w, r, badRequest("actor_forbidden", holdActorForbiddenMsg))
		return
	}
	if req.ReleasedAt != nil {
		s.respondError(w, r, badRequest("tx_forbidden", holdTimeForbiddenMsg))
		return
	}
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	hold, err := s.holds.ReleaseHold(r.Context(), r.PathValue("id"),
		principal.Actor, req.Reason)
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toHoldDTO(hold))
}

// handleListHolds is GET /v1/holds (admin): every hold ever recorded,
// released ones included, in placement order — the lifecycle is the audit
// value.
func (s *Server) handleListHolds(w http.ResponseWriter, r *http.Request) {
	if s.holds == nil {
		s.respondError(w, r, &Error{Status: http.StatusNotImplemented, Code: "unsupported",
			Message: "the configured store does not support legal holds"})
		return
	}
	holds, err := s.holds.Holds(r.Context())
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	dtos := make([]holdDTO, 0, len(holds))
	for _, h := range holds {
		dtos = append(dtos, toHoldDTO(h))
	}
	writeJSON(w, http.StatusOK, map[string][]holdDTO{"holds": dtos})
}
