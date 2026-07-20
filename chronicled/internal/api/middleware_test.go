package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zkrebbekx/chronicle"
)

// TestRoutingErrorsAreJSON: the error contract holds even for requests the
// mux itself rejects — unknown paths and wrong methods.
func TestRoutingErrorsAreJSON(t *testing.T) {
	h := newHarness(t, false)

	// Unknown path outside /v1: 404 JSON, no auth involved.
	resp, body := h.request("GET", "/nope", "", nil)
	wantError(t, resp, body, http.StatusNotFound, "not_found")

	// Wrong method on a real endpoint: 405 JSON with the Allow header the
	// mux computed.
	resp, body = h.request("DELETE", "/healthz", "", nil)
	wantError(t, resp, body, http.StatusMethodNotAllowed, "method_not_allowed")
	if resp.Header.Get("Allow") == "" {
		t.Fatal("405 lost the Allow header")
	}

	// Wrong method under /v1, authenticated: 405 JSON.
	resp, body = h.request("DELETE", "/v1/employee/alice", writerToken, nil)
	wantError(t, resp, body, http.StatusMethodNotAllowed, "method_not_allowed")

	// Unknown path under /v1 without a token is 401 — routing does not leak
	// which endpoints exist to unauthenticated callers.
	resp, body = h.request("GET", "/v1/what/is/this/even", "", nil)
	wantError(t, resp, body, http.StatusUnauthorized, "unauthorized")

	// With a token, it is a JSON 404.
	resp, body = h.request("GET", "/v1", writerToken, nil)
	wantError(t, resp, body, http.StatusNotFound, "not_found")
}

// TestReadyzFailure: a failing DB ping renders 503 through the contract.
func TestReadyzFailure(t *testing.T) {
	store := chronicle.NewMemStore()
	handler := NewHandler(Deps{
		Log:     chronicle.NewLog(store),
		Store:   store,
		Keyring: chronicle.NewMemKeyring(),
		Auth:    testAuth(t),
		Logger:  discardLogger(),
		Ready: func(context.Context) error {
			return errors.New("connection refused")
		},
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, body := doRequest(t, srv, "GET", "/readyz", "", nil)
	eb := wantError(t, resp, body, http.StatusServiceUnavailable, "unavailable")
	// The underlying error is logged, not returned.
	if eb.Error != "database unreachable" {
		t.Fatalf("readyz error = %q, want the generic message", eb.Error)
	}
}

// storeOnly narrows MemStore to the bare chronicle.Store interface, modelling
// a third-party store with no hold capability.
type storeOnly struct{ inner *chronicle.MemStore }

func (s storeOnly) Apply(ctx context.Context, req chronicle.ApplyRequest) (t time.Time, err error) {
	return s.inner.Apply(ctx, req)
}
func (s storeOnly) Get(ctx context.Context, q chronicle.GetQuery) (*chronicle.Record, error) {
	return s.inner.Get(ctx, q)
}
func (s storeOnly) Query(ctx context.Context, q chronicle.Query) ([]chronicle.Record, chronicle.Cursor, error) {
	return s.inner.Query(ctx, q)
}

// TestHoldsUnsupportedStore: a store without the HoldStore capability turns
// every hold endpoint into a 501 rather than a panic or a 500.
func TestHoldsUnsupportedStore(t *testing.T) {
	store := storeOnly{inner: chronicle.NewMemStore()}
	handler := NewHandler(Deps{
		Log:    chronicle.NewLog(store),
		Store:  store,
		Auth:   testAuth(t),
		Logger: discardLogger(),
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	for _, ep := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/v1/holds", map[string]any{"id": "h"}},
		{"POST", "/v1/holds/h/release", map[string]any{}},
		{"GET", "/v1/holds", nil},
	} {
		resp, body := doRequest(t, srv, ep.method, ep.path, adminToken, ep.body)
		wantError(t, resp, body, http.StatusNotImplemented, "unsupported")
	}
}

// TestNoKeyringConfigured: a deployment without a keyring reports the
// capability's absence rather than a 500.
func TestNoKeyringConfigured(t *testing.T) {
	store := chronicle.NewMemStore()
	handler := NewHandler(Deps{
		Log:    chronicle.NewLog(store),
		Store:  store,
		Auth:   testAuth(t),
		Logger: discardLogger(),
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, body := doRequest(t, srv, "POST", "/v1/subjects/s/destroy-key", adminToken, nil)
	wantError(t, resp, body, http.StatusNotImplemented, "no_keyring")

	// A write naming a subject on a log without a keyring is the library's
	// ErrNoKeyring, mapped the same way.
	resp, body = doRequest(t, srv, "POST", "/v1/employee/x/records", writerToken, map[string]any{
		"data":      map[string]any{"v": 1},
		"validFrom": "2026-01-01T00:00:00Z",
		"subject":   "s",
	})
	wantError(t, resp, body, http.StatusNotImplemented, "no_keyring")
}
