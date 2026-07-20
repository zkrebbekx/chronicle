package api

import (
	"net/http"
)

// destroyKeyNote is the exact hedge chronicled makes about what key
// destruction is, mirroring docs/COMPLIANCE.md: the mechanism is described
// functionally, and the legal characterization is left where it belongs.
const destroyKeyNote = "destroying the key renders this subject's encrypted historical values " +
	"unrecoverable while preserving the records' structure; whether that constitutes erasure under " +
	"GDPR Art. 17 depends on your supervisory authority's position — chronicle makes no compliance claim"

// handleDestroyKey is POST /v1/subjects/{subject}/destroy-key (admin).
// Destruction is terminal and idempotent: repeating it succeeds, and no key
// can ever be minted for the subject again — a quietly re-minted key would
// make new writes readable under an identifier the caller believes erased.
//
// The destruction is exactly as strong as the keyring behind it. The default
// deployment keeps keys in the same Postgres database as the records, which
// means the same backups: a restored backup restores the key. A deployment
// that needs shredding to withstand its own backup retention should back the
// service with a KMS-based keyring instead (a code change; the interface is
// chronicle.Keyring).
func (s *Server) handleDestroyKey(w http.ResponseWriter, r *http.Request) {
	if s.keyring == nil {
		s.respondError(w, r, &Error{Status: http.StatusNotImplemented, Code: "no_keyring",
			Message: "this deployment has no keyring configured; subject encryption and key destruction are unavailable"})
		return
	}
	subject := r.PathValue("subject")
	if err := rejectNUL("subject", subject); err != nil {
		s.respondError(w, r, err)
		return
	}
	if err := s.keyring.DestroyKey(r.Context(), subject); err != nil {
		s.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"subject":   subject,
		"destroyed": true,
		"note":      destroyKeyNote,
	})
}
