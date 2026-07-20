// Package api is chronicled's HTTP surface: a thin, JSON-speaking layer over
// one chronicle.Log. The temporal semantics live in the library; this package
// exists to hold the line on the things HTTP would otherwise erode — actor
// attribution comes from the bearer token and never from a request body,
// transaction time is never accepted from a caller, and every error is a
// typed JSON body whose code mirrors chronicle's sentinel taxonomy.
package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/zkrebbekx/chronicle"
	"github.com/zkrebbekx/chronicle/chronicled/internal/auth"
)

// maxBodyBytes caps request bodies at 4 MiB. Entity states are documents,
// not blobs; a body over this size is far more likely a mistake or an attack
// than a record.
const maxBodyBytes = 4 << 20

// Deps is everything the HTTP layer needs. Handler tests wire a MemStore and
// MemKeyring; production wires pgstore. The api package cannot tell the
// difference, which is the point.
type Deps struct {
	// Log is the one chronicle.Log this process writes through. One per
	// process: see the design note in docs/DESIGN.md — safe because pgstore
	// assigns transaction time, serialized because a Log serializes writes.
	Log *chronicle.Log
	// Store is the log's store, consulted directly for the capabilities the
	// Log does not front: legal holds (chronicle.HoldStore) and retention
	// (retain needs the store itself).
	Store chronicle.Store
	// Keyring destroys subject keys for the crypto-shredding endpoint.
	Keyring chronicle.Keyring
	// Auth resolves bearer tokens.
	Auth *auth.Authenticator
	// Logger receives one line per request. Never request bodies.
	Logger *slog.Logger
	// Ready reports whether the backing store is reachable, for /readyz.
	// Nil means always ready (tests over MemStore).
	Ready func(context.Context) error
}

// Server carries the dependencies into the handlers.
type Server struct {
	log     *chronicle.Log
	store   chronicle.Store
	holds   chronicle.HoldStore // nil when the store cannot hold legal holds
	keyring chronicle.Keyring
	auth    *auth.Authenticator
	logger  *slog.Logger
	ready   func(context.Context) error
}

// NewHandler builds the complete chronicled handler: routing, authentication,
// role enforcement, logging, and JSON-shaped routing errors.
func NewHandler(d Deps) http.Handler {
	s := &Server{
		log:     d.Log,
		store:   d.Store,
		keyring: d.Keyring,
		auth:    d.Auth,
		logger:  d.Logger,
		ready:   d.Ready,
	}
	if hs, ok := d.Store.(chronicle.HoldStore); ok {
		s.holds = hs
	}

	// Routes under /v1 require a valid token; the admin-only ones are
	// wrapped individually. Go 1.22 pattern routing keeps the fixed-path
	// endpoints (/v1/records, /v1/holds, ...) disjoint from the entity
	// tree (/v1/{kind}/{entity}/...) by segment count and literal
	// precedence — no two patterns below can match the same request.
	api := http.NewServeMux()
	api.HandleFunc("POST /v1/{kind}/{entity}/records", s.handlePut)
	api.HandleFunc("POST /v1/{kind}/{entity}/corrections", s.handleCorrect)
	api.HandleFunc("GET /v1/{kind}/{entity}", s.handleGet)
	api.HandleFunc("GET /v1/{kind}/{entity}/history", s.handleHistory)
	api.HandleFunc("GET /v1/{kind}/{entity}/timeline", s.handleTimeline)
	api.HandleFunc("GET /v1/{kind}/{entity}/diff", s.handleDiff)
	api.HandleFunc("GET /v1/{kind}/{entity}/verify", s.admin(s.handleVerify))
	api.HandleFunc("GET /v1/{kind}/{entity}/chain-head", s.admin(s.handleChainHead))
	api.HandleFunc("GET /v1/records", s.handleQuery)
	api.HandleFunc("POST /v1/holds", s.admin(s.handlePlaceHold))
	api.HandleFunc("POST /v1/holds/{id}/release", s.admin(s.handleReleaseHold))
	api.HandleFunc("GET /v1/holds", s.admin(s.handleListHolds))
	api.HandleFunc("POST /v1/retention/sweep", s.admin(s.handleSweep))
	api.HandleFunc("POST /v1/subjects/{subject}/destroy-key", s.admin(s.handleDestroyKey))
	api.HandleFunc("GET /v1/openapi.yaml", s.handleOpenAPI)

	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", s.handleHealthz)
	root.HandleFunc("GET /readyz", s.handleReadyz)
	root.Handle("/v1/", s.authenticate(api))

	return s.logging(jsonMuxErrors(root))
}

// handleHealthz is liveness: the process is up and serving. No auth — an
// orchestrator's probe carries no token — and no information beyond "up".
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz is readiness: the database answers a ping. Also unauthenticated,
// for the same probe reason; it reveals only reachability, never data.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.ready != nil {
		if err := s.ready(r.Context()); err != nil {
			s.logger.Warn("readiness check failed", "err", err.Error())
			writeJSON(w, http.StatusServiceUnavailable, errorBody{
				Error: "database unreachable",
				Code:  "unavailable",
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
