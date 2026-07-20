package api

import (
	"encoding/hex"
	"net/http"
)

// handleVerify is GET /v1/{kind}/{entity}/verify (admin): recompute the
// entity's hash chain and report the first divergence, if any.
//
// These endpoints are only meaningful when the server writes with chaining
// enabled (CHRONICLED_CHAINING=on); an entity with no chained records is a
// 404 no_chain, which must not be mistaken for a verification that passed.
// The threat model is the library's, stated rather than implied: a chain
// detects retrospective edits by someone who does not control the chain
// head, and nothing about an administrator who can recompute every hash.
// Anchor heads externally (see chain-head) for more.
func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	kind, entity, ok := s.pathKindEntity(w, r)
	if !ok {
		return
	}
	report, err := s.log.Verify(r.Context(), kind, entity)
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toVerifyDTO(report))
}

// handleChainHead is GET /v1/{kind}/{entity}/chain-head (admin): the stored
// chain head, unverified — anchoring and verification are separate acts, and
// an anchor of what the database currently claims is exactly what makes a
// later recomputed chain provable. Lodge the value somewhere the database
// administrator cannot reach; chronicled ships no anchoring.
func (s *Server) handleChainHead(w http.ResponseWriter, r *http.Request) {
	kind, entity, ok := s.pathKindEntity(w, r)
	if !ok {
		return
	}
	head, err := s.log.ChainHead(r.Context(), kind, entity)
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"kind":     kind,
		"entityId": entity,
		"head":     hex.EncodeToString(head),
	})
}
