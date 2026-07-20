package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestAuthentication(t *testing.T) {
	h := newHarness(t, false)

	cases := []struct {
		name  string
		token string
	}{
		{"no token", ""},
		{"unknown token", "not-a-configured-token"},
		{"near-miss token", writerToken + "x"},
		{"prefix of a real token", writerToken[:len(writerToken)-1]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := h.request("GET", "/v1/employee/alice", tc.token, nil)
			wantError(t, resp, body, http.StatusUnauthorized, "unauthorized")
			// The presented token must never be echoed back.
			if tc.token != "" && strings.Contains(string(body), tc.token) {
				t.Fatalf("response echoes the presented token: %s", body)
			}
		})
	}

	// A malformed Authorization scheme is 401 too.
	req, _ := http.NewRequest("GET", h.srv.URL+"/v1/employee/alice", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Basic auth: status = %d, want 401", resp.StatusCode)
	}
}

// TestRoleEnforcement walks every admin endpoint with the writer token and
// expects 403, then confirms both roles can read and write where they should.
func TestRoleEnforcement(t *testing.T) {
	h := newHarness(t, false)

	adminOnly := []struct {
		method, path string
		body         any
	}{
		{"POST", "/v1/holds", map[string]any{"id": "h1"}},
		{"POST", "/v1/holds/h1/release", map[string]any{}},
		{"GET", "/v1/holds", nil},
		{"POST", "/v1/retention/sweep", map[string]any{"policies": []any{}}},
		{"POST", "/v1/subjects/s1/destroy-key", nil},
		{"GET", "/v1/employee/alice/verify", nil},
		{"GET", "/v1/employee/alice/chain-head", nil},
	}
	for _, ep := range adminOnly {
		t.Run("writer forbidden "+ep.method+" "+ep.path, func(t *testing.T) {
			resp, body := h.request(ep.method, ep.path, writerToken, ep.body)
			wantError(t, resp, body, http.StatusForbidden, "forbidden")
		})
	}

	// The writer can write and read.
	resp, body := h.request("POST", "/v1/employee/alice/records", writerToken,
		putBody(`{"v":1}`, "2026-01-01T00:00:00Z"))
	wantStatus(t, resp, body, http.StatusCreated)

	// The admin can too — admin includes writer — and its writes are stamped
	// with the admin token's actor, not anything else.
	resp, body = h.request("POST", "/v1/employee/alice/corrections", adminToken,
		putBody(`{"v":2}`, "2026-01-01T00:00:00Z"))
	wantStatus(t, resp, body, http.StatusCreated)
	var res resultDTO
	decode(t, body, &res)
	if res.Record.Actor.ID != adminActor.ID {
		t.Fatalf("admin write stamped actor %q, want %q", res.Record.Actor.ID, adminActor.ID)
	}

	// Both roles can read.
	for _, token := range []string{writerToken, adminToken} {
		resp, body = h.request("GET", "/v1/employee/alice", token, nil)
		wantStatus(t, resp, body, http.StatusOK)
		resp, body = h.request("GET", "/v1/records?kind=employee", token, nil)
		wantStatus(t, resp, body, http.StatusOK)
	}
}

// TestActorInBodyRejected is the load-bearing auth test: every write-shaped
// endpoint refuses a caller-supplied actor with an explanation, and refuses
// caller-supplied transaction time the same way.
func TestActorInBodyRejected(t *testing.T) {
	h := newHarness(t, false)

	valid := `"validFrom":"2026-01-01T00:00:00Z"`
	actorCases := []struct {
		name, method, path, token, body string
	}{
		{"actor on put", "POST", "/v1/employee/a/records", writerToken,
			`{"data":{},` + valid + `,"actor":{"id":"mallory"}}`},
		{"actorId on put", "POST", "/v1/employee/a/records", writerToken,
			`{"data":{},` + valid + `,"actorId":"mallory"}`},
		{"actor on correction", "POST", "/v1/employee/a/corrections", writerToken,
			`{"data":{},` + valid + `,"actor":{"id":"mallory"}}`},
		{"placedBy on hold", "POST", "/v1/holds", adminToken,
			`{"id":"h-actor","placedBy":{"id":"mallory"}}`},
		{"releasedBy on release", "POST", "/v1/holds/h-actor/release", adminToken,
			`{"releasedBy":{"id":"mallory"}}`},
	}
	for _, tc := range actorCases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := h.request(tc.method, tc.path, tc.token, tc.body)
			eb := wantError(t, resp, body, http.StatusBadRequest, "actor_forbidden")
			// The rejection explains itself rather than just refusing.
			if !strings.Contains(eb.Error, "token") {
				t.Fatalf("rejection does not explain where actors come from: %q", eb.Error)
			}
		})
	}

	txCases := []struct {
		name, method, path, token, body string
	}{
		{"txAt on put", "POST", "/v1/employee/a/records", writerToken,
			`{"data":{},` + valid + `,"txAt":"2026-01-01T00:00:00Z"}`},
		{"txFrom on put", "POST", "/v1/employee/a/records", writerToken,
			`{"data":{},` + valid + `,"txFrom":"2026-01-01T00:00:00Z"}`},
		{"txTo on correction", "POST", "/v1/employee/a/corrections", writerToken,
			`{"data":{},` + valid + `,"txTo":"2026-01-01T00:00:00Z"}`},
		{"placedAt on hold", "POST", "/v1/holds", adminToken,
			`{"id":"h-tx","placedAt":"2026-01-01T00:00:00Z"}`},
		{"releasedAt on release", "POST", "/v1/holds/h-tx/release", adminToken,
			`{"releasedAt":"2026-01-01T00:00:00Z"}`},
	}
	for _, tc := range txCases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := h.request(tc.method, tc.path, tc.token, tc.body)
			wantError(t, resp, body, http.StatusBadRequest, "tx_forbidden")
		})
	}

	// After all those rejections, nothing was written.
	resp, body := h.request("GET", "/v1/employee/a/history", writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var recs recordsResponse
	decode(t, body, &recs)
	if len(recs.Records) != 0 {
		t.Fatalf("rejected writes left records behind: %+v", recs.Records)
	}
}

// TestHealthEndpointsUnauthenticated: probes carry no tokens.
func TestHealthEndpointsUnauthenticated(t *testing.T) {
	h := newHarness(t, false)
	for _, path := range []string{"/healthz", "/readyz"} {
		resp, body := h.request("GET", path, "", nil)
		wantStatus(t, resp, body, http.StatusOK)
		var st map[string]string
		decode(t, body, &st)
		if st["status"] != "ok" {
			t.Fatalf("%s = %s, want status ok", path, body)
		}
	}
}

// TestOpenAPI: authenticated, YAML, and truthful — every routed v1 path and
// the health endpoints appear in the spec.
func TestOpenAPI(t *testing.T) {
	h := newHarness(t, false)

	resp, body := h.request("GET", "/v1/openapi.yaml", "", nil)
	wantError(t, resp, body, http.StatusUnauthorized, "unauthorized")

	resp, body = h.request("GET", "/v1/openapi.yaml", writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	if ct := resp.Header.Get("Content-Type"); ct != "application/yaml" {
		t.Fatalf("Content-Type = %q, want application/yaml", ct)
	}
	spec := string(body)
	for _, path := range []string{
		"/v1/{kind}/{entity}/records:",
		"/v1/{kind}/{entity}/corrections:",
		"/v1/{kind}/{entity}:",
		"/v1/{kind}/{entity}/history:",
		"/v1/{kind}/{entity}/timeline:",
		"/v1/{kind}/{entity}/diff:",
		"/v1/{kind}/{entity}/field-history:",
		"/v1/{kind}/{entity}/verify:",
		"/v1/{kind}/{entity}/chain-head:",
		"/v1/records:",
		"/v1/holds:",
		"/v1/holds/{id}/release:",
		"/v1/retention/sweep:",
		"/v1/subjects/{subject}/destroy-key:",
		"/v1/openapi.yaml:",
		"/healthz:",
		"/readyz:",
	} {
		if !strings.Contains(spec, path) {
			t.Errorf("openapi.yaml does not document %s", strings.TrimSuffix(path, ":"))
		}
	}
}

// TestQueryOnlyNeedsAnyRole double-checks /v1/records is not admin-gated.
func TestQueryOnlyNeedsAnyRole(t *testing.T) {
	h := newHarness(t, false)
	resp, body := h.request("GET", "/v1/records", writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var page recordsResponse
	decode(t, body, &page)
	if page.Records == nil {
		t.Fatalf("records must be [] not null: %s", body)
	}
}
