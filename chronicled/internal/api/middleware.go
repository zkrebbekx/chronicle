package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/zkrebbekx/chronicle/chronicled/internal/auth"
)

// writeJSON renders one response. Every body chronicled produces goes through
// here, so the Content-Type and the encoding cannot drift between handlers.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// Encoding a value built from our own DTOs cannot fail except for a
	// broken connection, which has no useful recovery.
	_ = json.NewEncoder(w).Encode(body)
}

type ctxKey int

const principalCtxKey ctxKey = 0

// principalHolder rides the request context from the logging middleware
// inward. The auth middleware fills it, handlers read it, and the logging
// middleware — which wrapped the request before authentication ran — can
// still see who the caller was once the handler returns. A plain
// context.WithValue in the auth middleware could not deliver that, because
// the derived context never propagates back out.
type principalHolder struct {
	principal *auth.Principal
}

func principalFrom(ctx context.Context) *auth.Principal {
	if h, ok := ctx.Value(principalCtxKey).(*principalHolder); ok {
		return h.principal
	}
	return nil
}

// rejectNUL reports a 400 for any caller-supplied string carrying a NUL byte,
// which no store can hold. The library rejects NUL on its write paths but not
// its read paths, so without this a NUL in a path segment or query filter
// reaches pgstore and surfaces as a 500 — a caller minting internal errors at
// will. Rejecting it here keeps the library's "identically everywhere" posture
// true for reads too. The pairs are name/value; the first offender wins, so
// the message is deterministic.
func rejectNUL(pairs ...string) error {
	for i := 0; i+1 < len(pairs); i += 2 {
		if strings.IndexByte(pairs[i+1], 0) >= 0 {
			return badRequest("invalid_argument",
				pairs[i]+" contains a NUL byte, which no store can hold")
		}
	}
	return nil
}

// pathKindEntity reads the {kind} and {entity} path segments and rejects a NUL
// in either, the common preamble to every entity-scoped read handler. It
// writes the error itself and reports ok=false when the caller should return.
func (s *Server) pathKindEntity(w http.ResponseWriter, r *http.Request) (kind, entity string, ok bool) {
	kind, entity = r.PathValue("kind"), r.PathValue("entity")
	if err := rejectNUL("kind", kind, "entity", entity); err != nil {
		s.respondError(w, r, err)
		return "", "", false
	}
	return kind, entity, true
}

// requirePrincipal returns the authenticated actor for a write, or writes a
// 500 and reports false. The authenticate middleware always populates the
// principal before any /v1 handler runs, so a nil here means that invariant
// was broken by a future wiring change — not caller input. Enforcing it beats
// dereferencing on faith on the one path where a nil would record an empty
// actor, the exact failure the whole service exists to prevent.
func (s *Server) requirePrincipal(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	if p := principalFrom(r.Context()); p != nil {
		return p, true
	}
	s.logger.Error("write reached a handler with no authenticated principal",
		"method", r.Method, "path", r.URL.Path)
	writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal error", Code: "internal"})
	return nil, false
}

// authenticate guards everything under /v1/. It resolves the bearer token to
// a principal or ends the request with 401. Role enforcement is per-handler
// (see Server.admin); this middleware only establishes identity.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const scheme = "Bearer "
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, scheme) {
			writeJSON(w, http.StatusUnauthorized, errorBody{
				Error: "missing bearer token: send Authorization: Bearer <token>",
				Code:  "unauthorized",
			})
			return
		}
		principal, ok := s.auth.Authenticate(strings.TrimPrefix(header, scheme))
		if !ok {
			// The presented token is never echoed back — it is a secret,
			// even (especially) a wrong one.
			writeJSON(w, http.StatusUnauthorized, errorBody{
				Error: "unknown token",
				Code:  "unauthorized",
			})
			return
		}
		if h, okHolder := r.Context().Value(principalCtxKey).(*principalHolder); okHolder {
			h.principal = &principal
		}
		next.ServeHTTP(w, r)
	})
}

// admin wraps a handler that requires the admin role.
func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		if p == nil || p.Role != auth.RoleAdmin {
			writeJSON(w, http.StatusForbidden, errorBody{
				Error: "this endpoint requires the admin role",
				Code:  "forbidden",
			})
			return
		}
		next(w, r)
	}
}

// statusRecorder captures the response status for the request log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// recovering turns a handler panic into the JSON error contract rather than a
// bare dropped connection. Nothing in the service is expected to panic — the
// write handlers guard their one assumed invariant explicitly — so this is a
// backstop: it keeps "every error is a JSON body, nothing internal leaks" true
// even for a logic bug, and logs the panic server-side for the operator. The
// recovered value never reaches the client.
func (s *Server) recovering(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.logger.Error("handler panic",
					"method", r.Method, "path", r.URL.Path, "panic", v)
				// If the handler already began writing, the status is sent and
				// there is nothing to correct; only emit the error body when
				// the response has not started.
				if rec, ok := w.(*statusRecorder); !ok || rec.status == 0 {
					writeJSON(w, http.StatusInternalServerError, errorBody{
						Error: "internal error",
						Code:  "internal",
					})
				}
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// logging emits one structured line per request: method, path, status,
// duration and the authenticated actor's ID.
//
// Request bodies are never logged, at any level. They carry the regulated
// data this service exists to record — salaries, health attributes, whatever
// the caller's entities hold — and a debug log that duplicates them would be
// a second, unversioned, retention-free copy of exactly the data the primary
// store handles carefully. The token is never logged either; the actor ID is
// the loggable identity.
func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		holder := &principalHolder{}
		r = r.WithContext(context.WithValue(r.Context(), principalCtxKey, holder))
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		actorID := ""
		if holder.principal != nil {
			actorID = holder.principal.Actor.ID
		}
		s.logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"actor", actorID,
		)
	})
}

// muxErrorWriter rewrites http.ServeMux's own plain-text 404 and 405
// responses into the JSON error contract, so that "every error is a JSON
// body" holds for routing failures too, not just handler failures. Handler
// responses pass through untouched — they set an application/json
// Content-Type before writing, which is the discriminator.
type muxErrorWriter struct {
	http.ResponseWriter
	wroteHeader bool
	intercepted bool
}

func (w *muxErrorWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	fromMux := (code == http.StatusNotFound || code == http.StatusMethodNotAllowed) &&
		!strings.HasPrefix(w.Header().Get("Content-Type"), "application/json")
	if !fromMux {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	w.intercepted = true
	body := errorBody{Error: "no such endpoint", Code: "not_found"}
	if code == http.StatusMethodNotAllowed {
		// The mux has already set the Allow header; keep it.
		body = errorBody{Error: "method not allowed for this endpoint", Code: "method_not_allowed"}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.ResponseWriter.WriteHeader(code)
	_ = json.NewEncoder(w.ResponseWriter).Encode(body)
}

func (w *muxErrorWriter) Write(b []byte) (int, error) {
	if w.intercepted {
		// Swallow the mux's text body; the JSON body is already written.
		return len(b), nil
	}
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func jsonMuxErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&muxErrorWriter{ResponseWriter: w}, r)
	})
}
