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
