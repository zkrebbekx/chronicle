package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestZeroTimeSentinelRejected pins F2: the RFC 3339 rendering of Go's zero
// time is chronicle's unbounded/now sentinel, and must not be accepted as a
// literal timestamp on any path.
func TestZeroTimeSentinelRejected(t *testing.T) {
	h := newHarness(t, false)
	const zero = "0001-01-01T00:00:00Z"

	t.Run("given the zero-time sentinel as a literal value", func(t *testing.T) {
		t.Run("when a write sends it as validFrom", func(t *testing.T) {
			resp, body := h.request(http.MethodPost, "/v1/employee/e1/records", writerToken,
				putBody(`{"v":1}`, zero))
			t.Run("then it is rejected, not stored as unbounded", func(t *testing.T) {
				wantError(t, resp, body, http.StatusBadRequest, "invalid_argument")
			})
		})
		t.Run("when a write sends it as validTo", func(t *testing.T) {
			resp, body := h.request(http.MethodPost, "/v1/employee/e1/records", writerToken,
				map[string]any{"data": map[string]int{"v": 1}, "validFrom": "2026-01-01T00:00:00Z", "validTo": zero})
			t.Run("then it is rejected, not silently 'still holds'", func(t *testing.T) {
				wantError(t, resp, body, http.StatusBadRequest, "invalid_argument")
			})
		})
		t.Run("when a read sends it as validAt", func(t *testing.T) {
			resp, body := h.request(http.MethodGet, "/v1/employee/e1?validAt="+zero, writerToken, nil)
			t.Run("then it is rejected, not treated as now", func(t *testing.T) {
				wantError(t, resp, body, http.StatusBadRequest, "invalid_argument")
			})
		})
	})
}

// TestValidTimeTruncatedToStorage pins F1: caller valid times are truncated to
// the store's microsecond resolution at the boundary, so the 201 echo is
// byte-identical to a later read rather than the nanosecond input the store
// never held.
func TestValidTimeTruncatedToStorage(t *testing.T) {
	h := newHarness(t, false)
	t.Run("given a write with nanosecond-precision validFrom", func(t *testing.T) {
		resp, body := h.request(http.MethodPost, "/v1/employee/e1/records", writerToken,
			putBody(`{"v":1}`, "2026-01-02T03:04:05.123456789Z"))
		wantStatus(t, resp, body, http.StatusCreated)
		var put resultDTO
		decode(t, body, &put)
		t.Run("then the echoed validFrom is the stored microsecond value", func(t *testing.T) {
			const want = "2026-01-02T03:04:05.123456Z"
			if put.Record.ValidFrom != want {
				t.Fatalf("validFrom echoed %q, want %q — the 201 body must match storage", put.Record.ValidFrom, want)
			}
		})
	})
}

// TestNULInPathAndQueryIs400 pins F3: a NUL byte in a path segment or query
// filter is a 400, not a 500 minted by pgstore's driver.
func TestNULInPathAndQueryIs400(t *testing.T) {
	h := newHarness(t, false)
	t.Run("given a NUL byte in a read request", func(t *testing.T) {
		cases := []struct{ name, path string }{
			{"the kind path segment", "/v1/a%00b/e1"},
			{"history path", "/v1/a%00b/e1/history"},
			{"timeline path", "/v1/a%00b/e1/timeline"},
			{"diff path", "/v1/a%00b/e1/diff"},
			{"the records kind filter", "/v1/records?kind=a%00b"},
			{"the records actorId filter", "/v1/records?actorId=a%00b"},
		}
		for _, tc := range cases {
			t.Run("when it appears in "+tc.name, func(t *testing.T) {
				resp, body := h.request(http.MethodGet, tc.path, writerToken, nil)
				t.Run("then it is a 400, not a 500", func(t *testing.T) {
					wantError(t, resp, body, http.StatusBadRequest, "invalid_argument")
				})
			})
		}
	})
}

// TestLimitContract pins the unified limit semantics across the two endpoints
// that accept one: a positive integer at most maxQueryLimit, with zero and
// oversized values rejected identically rather than each endpoint inventing
// its own meaning for them.
func TestLimitContract(t *testing.T) {
	h := newHarness(t, false)
	if resp, body := h.request(http.MethodPost, "/v1/employee/e1/records", writerToken,
		putBody(`{"v":1}`, "2026-01-01T00:00:00Z")); resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed write: %d %s", resp.StatusCode, body)
	}

	t.Run("given the history and records endpoints", func(t *testing.T) {
		for _, ep := range []struct {
			name, path string
		}{
			{"history", "/v1/employee/e1/history?limit="},
			{"records", "/v1/records?limit="},
		} {
			t.Run("when "+ep.name+" gets a huge limit", func(t *testing.T) {
				resp, body := h.request(http.MethodGet, ep.path+"2000000000", writerToken, nil)
				t.Run("then it is rejected, not silently accepted", func(t *testing.T) {
					wantError(t, resp, body, http.StatusBadRequest, "invalid_argument")
				})
			})
			t.Run("when "+ep.name+" gets limit=0", func(t *testing.T) {
				resp, body := h.request(http.MethodGet, ep.path+"0", writerToken, nil)
				t.Run("then zero is rejected on both, not unbounded on one", func(t *testing.T) {
					wantError(t, resp, body, http.StatusBadRequest, "invalid_argument")
				})
			})
			t.Run("when "+ep.name+" gets a valid limit", func(t *testing.T) {
				resp, body := h.request(http.MethodGet, ep.path+"10", writerToken, nil)
				t.Run("then it is accepted", func(t *testing.T) {
					wantStatus(t, resp, body, http.StatusOK)
				})
			})
		}
	})

	t.Run("given history with no limit at all", func(t *testing.T) {
		resp, body := h.request(http.MethodGet, "/v1/employee/e1/history", writerToken, nil)
		t.Run("then the full history is returned, not a truncated page", func(t *testing.T) {
			wantStatus(t, resp, body, http.StatusOK)
		})
	})
}

// TestPanicIsRecoveredAsJSON proves the recover middleware keeps the error
// contract even for a handler that panics: the client sees a JSON internal
// error, not a dropped connection.
func TestPanicIsRecoveredAsJSON(t *testing.T) {
	t.Run("given a handler that panics", func(t *testing.T) {
		s := &Server{logger: discardLogger()}
		panicky := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("boom")
		})
		// Wrap exactly as production does: recorder (via logging) outside,
		// recovering just inside it.
		handler := s.logging(s.recovering(panicky))
		srv := httptest.NewServer(handler)
		t.Cleanup(srv.Close)

		resp, body := doRequest(t, srv, http.MethodGet, "/anything", "", nil)
		t.Run("then the client gets a JSON 500, not a closed socket", func(t *testing.T) {
			wantError(t, resp, body, http.StatusInternalServerError, "internal")
		})
	})
}

// TestWriteWithoutPrincipalFailsClosed proves a write that somehow reaches a
// handler with no authenticated principal returns a clean 500 rather than
// panicking on a nil dereference — the invariant is enforced, not assumed.
func TestWriteWithoutPrincipalFailsClosed(t *testing.T) {
	t.Run("given a write handler reached with no principal in context", func(t *testing.T) {
		h := newHarness(t, false)
		s := &Server{log: h.log, store: h.store, logger: discardLogger()}
		// Call the handler directly, bypassing the authenticate middleware
		// that would normally install the principal.
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/employee/e1/records",
			strings.NewReader(`{"data":{"v":1},"validFrom":"2026-01-01T00:00:00Z"}`))
		req.Header.Set("Content-Type", "application/json")
		s.handleWrite(rec, req, false)

		t.Run("then it fails closed with an internal error, not a panic", func(t *testing.T) {
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500; body: %s", rec.Code, rec.Body.String())
			}
		})
	})
}
