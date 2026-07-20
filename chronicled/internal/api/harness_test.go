package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zkrebbekx/chronicle"
	"github.com/zkrebbekx/chronicle/chronicled/internal/auth"
)

// The fast-path harness: the full HTTP handler over MemStore and MemKeyring.
// No database, no network beyond the loopback httptest server — the same
// handler code production runs, over the reference store.

const (
	writerToken = "writer-secret-token"
	adminToken  = "admin-secret-token"
)

var (
	writerActor = chronicle.Actor{ID: "svc-writer", Type: "service", Name: "Writer Service"}
	adminActor  = chronicle.Actor{ID: "u-admin", Type: "user", Name: "Admin User"}
)

func testAuth(t *testing.T) *auth.Authenticator {
	t.Helper()
	a, err := auth.New([]auth.Credential{
		{Token: writerToken, Principal: auth.Principal{Actor: writerActor, Role: auth.RoleWriter}},
		{Token: adminToken, Principal: auth.Principal{Actor: adminActor, Role: auth.RoleAdmin}},
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	return a
}

type harness struct {
	t       *testing.T
	srv     *httptest.Server
	log     *chronicle.Log
	store   *chronicle.MemStore
	keyring *chronicle.MemKeyring
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newHarness(t *testing.T, chaining bool) *harness {
	t.Helper()
	store := chronicle.NewMemStore()
	keyring := chronicle.NewMemKeyring()
	opts := []chronicle.Option{chronicle.WithKeyring(keyring)}
	if chaining {
		opts = append(opts, chronicle.WithChaining())
	}
	log := chronicle.NewLog(store, opts...)
	handler := NewHandler(Deps{
		Log:     log,
		Store:   store,
		Keyring: keyring,
		Auth:    testAuth(t),
		Logger:  discardLogger(),
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &harness{t: t, srv: srv, log: log, store: store, keyring: keyring}
}

// request performs one HTTP call. body may be nil, a raw string, or a value
// to marshal as JSON. An empty token sends no Authorization header.
func (h *harness) request(method, path, token string, body any) (*http.Response, []byte) {
	h.t.Helper()
	return doRequest(h.t, h.srv, method, path, token, body)
}

func doRequest(t *testing.T, srv *httptest.Server, method, path, token string, body any) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rdr = strings.NewReader(b)
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp, data
}

// decode unmarshals a response body, failing the test on garbage.
func decode(t *testing.T, data []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(data, dst); err != nil {
		t.Fatalf("unmarshal response %q: %v", data, err)
	}
}

// wantStatus asserts a status code, printing the body on mismatch.
func wantStatus(t *testing.T, resp *http.Response, body []byte, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, want, body)
	}
}

// wantError asserts the error contract: the given status, the given code,
// and a JSON body carrying both.
func wantError(t *testing.T, resp *http.Response, body []byte, status int, code string) errorBody {
	t.Helper()
	wantStatus(t, resp, body, status)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("error Content-Type = %q, want application/json; body: %s", ct, body)
	}
	var eb errorBody
	decode(t, body, &eb)
	if eb.Code != code {
		t.Fatalf("error code = %q, want %q; body: %s", eb.Code, code, body)
	}
	if eb.Error == "" {
		t.Fatalf("error body has no message: %s", body)
	}
	return eb
}
