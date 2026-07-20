package api

import (
	_ "embed"
	"net/http"
)

// openapiSpec is the hand-written contract for this service, embedded so the
// binary documents itself. It is maintained by hand and kept truthful — no
// codegen, and a test asserts every routed path appears in it.
//
//go:embed openapi.yaml
var openapiSpec []byte

// handleOpenAPI is GET /v1/openapi.yaml. Authenticated like the rest of /v1:
// the spec names kinds of operations this deployment supports, which is more
// than an anonymous caller needs to know.
func (s *Server) handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openapiSpec)
}
