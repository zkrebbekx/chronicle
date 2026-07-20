package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// putBody is the canonical minimal write.
func putBody(data string, validFrom string) map[string]any {
	return map[string]any{
		"data":      json.RawMessage(data),
		"validFrom": validFrom,
	}
}

// TestBitemporalFlagship drives the library's headline scenario end to end
// through HTTP: an assertion, a retroactive correction, and then the read
// that uni-temporal systems get wrong — with transaction time pinned to the
// original belief, the original data comes back.
func TestBitemporalFlagship(t *testing.T) {
	h := newHarness(t, false)

	// Salary asserted effective 1 March.
	resp, body := h.request("POST", "/v1/employee/alice/records", writerToken,
		putBody(`{"salary":50000,"title":"Engineer"}`, "2026-03-01T00:00:00Z"))
	wantStatus(t, resp, body, http.StatusCreated)
	var put resultDTO
	decode(t, body, &put)
	if put.TxAt == "" {
		t.Fatal("result carries no txAt")
	}
	if put.Record.Intent != "assert" {
		t.Fatalf("intent = %q, want assert", put.Record.Intent)
	}
	if put.Record.Actor.ID != writerActor.ID {
		t.Fatalf("actor = %q, want the token's actor %q", put.Record.Actor.ID, writerActor.ID)
	}
	if len(put.Superseded) != 0 {
		t.Fatalf("first write superseded %v, want nothing", put.Superseded)
	}

	// Retroactive correction: the March salary was actually 55000.
	resp, body = h.request("POST", "/v1/employee/alice/corrections", writerToken,
		putBody(`{"salary":55000,"title":"Engineer"}`, "2026-03-01T00:00:00Z"))
	wantStatus(t, resp, body, http.StatusCreated)
	var corr resultDTO
	decode(t, body, &corr)
	if corr.Record.Intent != "correction" {
		t.Fatalf("intent = %q, want correction", corr.Record.Intent)
	}
	if len(corr.Superseded) != 1 || corr.Superseded[0] != put.Record.ID {
		t.Fatalf("correction superseded %v, want [%s]", corr.Superseded, put.Record.ID)
	}

	march := "2026-03-15T00:00:00Z"

	// Current belief about March: the corrected figure.
	resp, body = h.request("GET", "/v1/employee/alice?validAt="+march, writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var now recordDTO
	decode(t, body, &now)
	if !strings.Contains(string(now.Data), "55000") {
		t.Fatalf("current belief = %s, want the corrected 55000", now.Data)
	}

	// Belief pinned to the original transaction instant: the original
	// figure, unrewritten. This is the read that justifies the second axis.
	resp, body = h.request("GET",
		"/v1/employee/alice?validAt="+march+"&txAt="+url.QueryEscape(put.TxAt), writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var then recordDTO
	decode(t, body, &then)
	if !strings.Contains(string(then.Data), "50000") {
		t.Fatalf("pinned belief = %s, want the original 50000", then.Data)
	}
	if then.ID != put.Record.ID {
		t.Fatalf("pinned read returned %s, want the original record %s", then.ID, put.Record.ID)
	}

	// The diff across the transaction axis shows exactly what the
	// correction changed about the same moment in the world.
	resp, body = h.request("GET", "/v1/employee/alice/diff?fromValidAt="+march+
		"&toValidAt="+march+"&fromTxAt="+url.QueryEscape(put.TxAt), writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var delta deltaDTO
	decode(t, body, &delta)
	if len(delta.Changes) != 1 {
		t.Fatalf("diff changes = %+v, want exactly the salary change", delta.Changes)
	}
	if delta.Changes[0].Path != "/salary" || delta.Changes[0].Op != "modified" {
		t.Fatalf("change = %+v, want /salary modified", delta.Changes[0])
	}
}

func TestHistoryFiltersAndTimeline(t *testing.T) {
	h := newHarness(t, false)

	resp, body := h.request("POST", "/v1/employee/bob/records", writerToken,
		putBody(`{"salary":10}`, "2026-01-01T00:00:00Z"))
	wantStatus(t, resp, body, http.StatusCreated)
	var first resultDTO
	decode(t, body, &first)

	resp, body = h.request("POST", "/v1/employee/bob/corrections", writerToken,
		putBody(`{"salary":20}`, "2026-01-01T00:00:00Z"))
	wantStatus(t, resp, body, http.StatusCreated)

	// Full history: both versions, superseded included.
	resp, body = h.request("GET", "/v1/employee/bob/history", writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var all recordsResponse
	decode(t, body, &all)
	if len(all.Records) != 2 {
		t.Fatalf("history = %d records, want 2", len(all.Records))
	}

	// Current only: just the correction.
	resp, body = h.request("GET", "/v1/employee/bob/history?currentOnly=true", writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var current recordsResponse
	decode(t, body, &current)
	if len(current.Records) != 1 || current.Records[0].Intent != "correction" {
		t.Fatalf("currentOnly history = %+v, want the single correction", current.Records)
	}

	// Intent filter.
	resp, body = h.request("GET", "/v1/employee/bob/history?intent=assert", writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var asserts recordsResponse
	decode(t, body, &asserts)
	if len(asserts.Records) != 1 || asserts.Records[0].ID != first.Record.ID {
		t.Fatalf("intent=assert history = %+v, want the original record", asserts.Records)
	}

	// Actor filter matches the token's stamped actor; a different ID matches
	// nothing, proving the filter really runs against the stamped identity.
	resp, body = h.request("GET", "/v1/employee/bob/history?actorId="+writerActor.ID, writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var byActor recordsResponse
	decode(t, body, &byActor)
	if len(byActor.Records) != 2 {
		t.Fatalf("actorId history = %d records, want 2", len(byActor.Records))
	}
	resp, body = h.request("GET", "/v1/employee/bob/history?actorId=nobody", writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var none recordsResponse
	decode(t, body, &none)
	if len(none.Records) != 0 {
		t.Fatalf("actorId=nobody history = %+v, want empty", none.Records)
	}

	// History of an unknown entity is empty, not 404 — a fact, not a failure.
	resp, body = h.request("GET", "/v1/employee/nobody/history", writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)

	// Timeline pinned to the first belief shows the original tiling.
	resp, body = h.request("GET", "/v1/employee/bob/timeline?txAt="+url.QueryEscape(first.TxAt), writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var timeline recordsResponse
	decode(t, body, &timeline)
	if len(timeline.Records) != 1 || timeline.Records[0].ID != first.Record.ID {
		t.Fatalf("timeline at first belief = %+v, want the original record", timeline.Records)
	}

	// Limit and descending, exercised for coverage of the option plumbing.
	resp, body = h.request("GET", "/v1/employee/bob/history?limit=1&descending=true", writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var page recordsResponse
	decode(t, body, &page)
	if len(page.Records) != 1 {
		t.Fatalf("limited history = %d records, want 1", len(page.Records))
	}
}

func TestWriteValidation(t *testing.T) {
	h := newHarness(t, false)
	path := "/v1/employee/carol/records"

	cases := []struct {
		name   string
		body   any
		status int
		code   string
	}{
		{"missing data", map[string]any{"validFrom": "2026-01-01T00:00:00Z"}, 400, "invalid_body"},
		{"missing validFrom", map[string]any{"data": json.RawMessage(`{}`)}, 400, "invalid_body"},
		{"null validFrom", `{"data":{},"validFrom":null}`, 400, "invalid_body"},
		{"bad validFrom", putBody(`{}`, "yesterday"), 400, "invalid_argument"},
		{"bad validTo", map[string]any{"data": json.RawMessage(`{}`), "validFrom": "2026-01-01T00:00:00Z", "validTo": "later"}, 400, "invalid_argument"},
		{"inverted interval", map[string]any{"data": json.RawMessage(`{}`), "validFrom": "2026-02-01T00:00:00Z", "validTo": "2026-01-01T00:00:00Z"}, 400, "invalid_interval"},
		{"empty interval", map[string]any{"data": json.RawMessage(`{}`), "validFrom": "2026-01-01T00:00:00Z", "validTo": "2026-01-01T00:00:00Z"}, 400, "invalid_interval"},
		{"not JSON", `{"data":`, 400, "invalid_body"},
		{"trailing garbage", `{"data":{},"validFrom":"2026-01-01T00:00:00Z"} extra`, 400, "invalid_body"},
		{"unknown field", `{"data":{},"validFrom":"2026-01-01T00:00:00Z","wat":1}`, 400, "invalid_body"},
		{"reserved meta", map[string]any{"data": json.RawMessage(`{}`), "validFrom": "2026-01-01T00:00:00Z", "meta": map[string]string{"chronicle:chain": "x"}}, 400, "reserved_meta"},
		{"NUL in reason", map[string]any{"data": json.RawMessage(`{}`), "validFrom": "2026-01-01T00:00:00Z", "reason": "a\x00b"}, 400, "invalid_field"},
		{"NUL in meta", map[string]any{"data": json.RawMessage(`{}`), "validFrom": "2026-01-01T00:00:00Z", "meta": map[string]string{"k": "a\x00b"}}, 400, "invalid_meta"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := h.request("POST", path, writerToken, tc.body)
			wantError(t, resp, body, tc.status, tc.code)
		})
	}

	// Corrections share the same decoding; one spot check.
	resp, body := h.request("POST", "/v1/employee/carol/corrections", writerToken,
		map[string]any{"data": json.RawMessage(`{}`)})
	wantError(t, resp, body, 400, "invalid_body")

	// An oversized body is 413, not an unexplained connection reset.
	huge := `{"data":{"blob":"` + strings.Repeat("x", maxBodyBytes) + `"},"validFrom":"2026-01-01T00:00:00Z"}`
	resp, body = h.request("POST", path, writerToken, huge)
	wantError(t, resp, body, http.StatusRequestEntityTooLarge, "body_too_large")
}

func TestValidToNullMeansUnbounded(t *testing.T) {
	h := newHarness(t, false)

	// Explicit null and absent validTo mean the same thing: unbounded.
	resp, body := h.request("POST", "/v1/employee/dave/records", writerToken,
		`{"data":{"v":1},"validFrom":"2026-01-01T00:00:00Z","validTo":null}`)
	wantStatus(t, resp, body, http.StatusCreated)
	var res resultDTO
	decode(t, body, &res)
	if res.Record.ValidTo != "" {
		t.Fatalf("validTo = %q, want omitted (unbounded)", res.Record.ValidTo)
	}
	// Far-future valid instant still covered.
	resp, body = h.request("GET", "/v1/employee/dave?validAt=2100-01-01T00:00:00Z", writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
}

func TestGetNotFoundCarriesCoordinates(t *testing.T) {
	h := newHarness(t, false)
	resp, body := h.request("GET", "/v1/employee/ghost", writerToken, nil)
	eb := wantError(t, resp, body, http.StatusNotFound, "not_found")
	detail, ok := eb.Detail.(map[string]any)
	if !ok {
		t.Fatalf("not_found detail = %#v, want an object with the coordinates", eb.Detail)
	}
	if detail["kind"] != "employee" || detail["entityId"] != "ghost" {
		t.Fatalf("detail = %#v, want kind/entityId echoed", detail)
	}
	// The As was resolved: absent axes became real instants.
	if detail["validAt"] == "" || detail["txAt"] == "" {
		t.Fatalf("detail = %#v, want resolved instants", detail)
	}
}

func TestBadQueryTimestamps(t *testing.T) {
	h := newHarness(t, false)
	for _, path := range []string{
		"/v1/employee/x?validAt=nope",
		"/v1/employee/x?txAt=nope",
		"/v1/employee/x/history?validFrom=nope",
		"/v1/employee/x/history?txTo=nope",
		"/v1/employee/x/history?currentOnly=maybe",
		"/v1/employee/x/history?limit=-1",
		"/v1/employee/x/history?intent=wish",
		"/v1/employee/x/timeline?txAt=nope",
		"/v1/employee/x/diff?fromValidAt=nope",
		"/v1/employee/x/diff?toTxAt=nope",
	} {
		resp, body := h.request("GET", path, writerToken, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400; body: %s", path, resp.StatusCode, body)
		}
	}
}

func TestDiffAcrossMissingSides(t *testing.T) {
	h := newHarness(t, false)

	// No state at either point: 404.
	resp, body := h.request("GET", "/v1/employee/eve/diff", writerToken, nil)
	wantError(t, resp, body, http.StatusNotFound, "not_found")

	// State on one side only: everything reported as added.
	resp, body = h.request("POST", "/v1/employee/eve/records", writerToken,
		putBody(`{"a":1}`, "2026-06-01T00:00:00Z"))
	wantStatus(t, resp, body, http.StatusCreated)
	resp, body = h.request("GET",
		"/v1/employee/eve/diff?fromValidAt=2026-01-01T00:00:00Z&toValidAt=2026-06-02T00:00:00Z",
		writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var delta deltaDTO
	decode(t, body, &delta)
	if delta.FromRecord != nil || delta.ToRecord == nil {
		t.Fatalf("delta records = (%v, %v), want (nil, record)", delta.FromRecord, delta.ToRecord)
	}
	if len(delta.Changes) != 1 || delta.Changes[0].Op != "added" {
		t.Fatalf("changes = %+v, want a single addition", delta.Changes)
	}
}
