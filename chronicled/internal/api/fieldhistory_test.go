package api

import (
	"net/http"
	"net/url"
	"testing"
)

// seedCorrectedSalary writes salary 50000 valid from March, then corrects it to
// 55000 over the same interval, and returns the two transaction instants.
func seedCorrectedSalary(t *testing.T, h *harness, entity string) (firstTx, corrTx string) {
	t.Helper()
	resp, body := h.request("POST", "/v1/employee/"+entity+"/records", writerToken,
		putBody(`{"salary":50000,"title":"Engineer"}`, "2026-03-01T00:00:00Z"))
	wantStatus(t, resp, body, http.StatusCreated)
	var put resultDTO
	decode(t, body, &put)

	resp, body = h.request("POST", "/v1/employee/"+entity+"/corrections", adminToken,
		putBody(`{"salary":55000,"title":"Engineer"}`, "2026-03-01T00:00:00Z"))
	wantStatus(t, resp, body, http.StatusCreated)
	var corr resultDTO
	decode(t, body, &corr)
	return put.TxAt, corr.TxAt
}

// TestFieldHistoryHTTP is the flagship end-to-end: how our belief about the
// March salary changed over transaction time, and who changed it.
func TestFieldHistoryHTTP(t *testing.T) {
	h := newHarness(t, false)
	firstTx, corrTx := seedCorrectedSalary(t, h, "alice")

	march := "2026-03-15T00:00:00Z"
	resp, body := h.request("GET",
		"/v1/employee/alice/field-history?path="+url.QueryEscape("/salary")+"&validAt="+march,
		writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)

	var fh fieldHistoryResponse
	decode(t, body, &fh)

	if fh.Path != "/salary" {
		t.Fatalf("path = %q, want /salary", fh.Path)
	}
	if fh.ValidAt == "" {
		t.Fatal("response omitted the resolved validAt even though it was pinned")
	}
	if len(fh.Changes) != 2 {
		t.Fatalf("changes = %d, want 2: %s", len(fh.Changes), body)
	}

	first := fh.Changes[0]
	if first.From.Present {
		t.Fatalf("first change from = %+v, want absent", first.From)
	}
	if !first.To.Present || string(first.To.Value) != "50000" {
		t.Fatalf("first change to = %+v, want present 50000", first.To)
	}
	if first.Intent != "assert" || first.Actor.ID != writerActor.ID || first.TxAt != firstTx {
		t.Fatalf("first change = %+v, want assert by the writer at %s", first, firstTx)
	}

	second := fh.Changes[1]
	if string(second.From.Value) != "50000" || string(second.To.Value) != "55000" {
		t.Fatalf("second change = %+v, want 50000 -> 55000", second)
	}
	if second.Intent != "correction" || second.Actor.ID != adminActor.ID || second.TxAt != corrTx {
		t.Fatalf("second change = %+v, want correction by the admin at %s", second, corrTx)
	}
	if second.ValidFrom == "" {
		t.Fatalf("second change carries no validFrom: %+v", second)
	}
}

func TestFieldHistoryDescendingHTTP(t *testing.T) {
	h := newHarness(t, false)
	_, corrTx := seedCorrectedSalary(t, h, "bob")

	resp, body := h.request("GET",
		"/v1/employee/bob/field-history?path="+url.QueryEscape("/salary")+
			"&validAt=2026-03-15T00:00:00Z&descending=true",
		writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var fh fieldHistoryResponse
	decode(t, body, &fh)
	if len(fh.Changes) != 2 || fh.Changes[0].TxAt != corrTx {
		t.Fatalf("descending changes = %+v, want the correction first", fh.Changes)
	}
}

func TestFieldHistoryNullTombstoneHTTP(t *testing.T) {
	h := newHarness(t, false)
	resp, body := h.request("POST", "/v1/employee/carol/records", writerToken,
		putBody(`{"salary":50000}`, "2026-03-01T00:00:00Z"))
	wantStatus(t, resp, body, http.StatusCreated)
	// A JSON null body is a usable tombstone: the record still covers the point
	// but the object no longer contains the field.
	resp, body = h.request("POST", "/v1/employee/carol/corrections", writerToken,
		putBody(`null`, "2026-03-01T00:00:00Z"))
	wantStatus(t, resp, body, http.StatusCreated)

	resp, body = h.request("GET",
		"/v1/employee/carol/field-history?path="+url.QueryEscape("/salary")+"&validAt=2026-03-15T00:00:00Z",
		writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var fh fieldHistoryResponse
	decode(t, body, &fh)
	if len(fh.Changes) != 2 {
		t.Fatalf("changes = %d, want 2 (appear then absent): %s", len(fh.Changes), body)
	}
	last := fh.Changes[1]
	if !last.From.Present || last.To.Present {
		t.Fatalf("last change = %+v, want present -> absent", last)
	}
	if last.To.Value != nil {
		t.Fatalf("absent field rendered a value %s, want it omitted", last.To.Value)
	}
}

func TestFieldHistoryValidationHTTP(t *testing.T) {
	h := newHarness(t, false)
	resp, body := h.request("POST", "/v1/employee/dave/records", writerToken,
		putBody(`{"salary":50000}`, "2026-03-01T00:00:00Z"))
	wantStatus(t, resp, body, http.StatusCreated)

	base := "/v1/employee/dave/field-history"

	t.Run("path is required", func(t *testing.T) {
		resp, body := h.request("GET", base+"?validAt=2026-03-15T00:00:00Z", writerToken, nil)
		wantError(t, resp, body, http.StatusBadRequest, "invalid_argument")
	})

	t.Run("a malformed pointer is invalid_path", func(t *testing.T) {
		resp, body := h.request("GET", base+"?path=salary", writerToken, nil) // no leading slash
		wantError(t, resp, body, http.StatusBadRequest, "invalid_path")
	})

	t.Run("a NUL in the path is rejected here, not by the store", func(t *testing.T) {
		resp, body := h.request("GET", base+"?path="+url.QueryEscape("/sal\x00ary"), writerToken, nil)
		wantError(t, resp, body, http.StatusBadRequest, "invalid_argument")
	})

	t.Run("descending must be a boolean", func(t *testing.T) {
		resp, body := h.request("GET", base+"?path="+url.QueryEscape("/salary")+"&descending=maybe", writerToken, nil)
		wantError(t, resp, body, http.StatusBadRequest, "invalid_argument")
	})

	t.Run("the zero-time sentinel is rejected on validAt", func(t *testing.T) {
		resp, body := h.request("GET", base+"?path="+url.QueryEscape("/salary")+
			"&validAt=0001-01-01T00:00:00Z", writerToken, nil)
		wantError(t, resp, body, http.StatusBadRequest, "invalid_argument")
	})

	t.Run("a never-present path is an empty result, not an error", func(t *testing.T) {
		resp, body := h.request("GET", base+"?path="+url.QueryEscape("/bonus")+
			"&validAt=2026-03-15T00:00:00Z", writerToken, nil)
		wantStatus(t, resp, body, http.StatusOK)
		var fh fieldHistoryResponse
		decode(t, body, &fh)
		if len(fh.Changes) != 0 {
			t.Fatalf("changes = %d, want 0 for a never-present path", len(fh.Changes))
		}
	})

	t.Run("an unknown entity is an empty result, not 404", func(t *testing.T) {
		resp, body := h.request("GET", "/v1/employee/ghost/field-history?path="+url.QueryEscape("/salary"),
			writerToken, nil)
		wantStatus(t, resp, body, http.StatusOK)
		var fh fieldHistoryResponse
		decode(t, body, &fh)
		if len(fh.Changes) != 0 {
			t.Fatalf("changes = %d, want 0 for an unknown entity", len(fh.Changes))
		}
	})

	t.Run("it needs a token but not the admin role", func(t *testing.T) {
		resp, body := h.request("GET", base+"?path="+url.QueryEscape("/salary"), "", nil)
		wantError(t, resp, body, http.StatusUnauthorized, "unauthorized")
	})
}
