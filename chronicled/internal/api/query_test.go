package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/zkrebbekx/chronicle"
)

// TestCursorPassThrough is the pagination contract: a full walk over HTTP,
// passing each returned cursor back opaquely, visits exactly the records a
// direct library walk visits, in the same order.
func TestCursorPassThrough(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()

	// Seed through the library directly — same store the server writes.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		entity := fmt.Sprintf("e-%d", i%3)
		data := fmt.Sprintf(`{"n":%d}`, i)
		if _, err := h.log.Put(ctx, "widget", entity, []byte(data),
			base.AddDate(0, 0, i), time.Time{}, writerActor); err != nil {
			t.Fatalf("seed put %d: %v", i, err)
		}
	}

	// Reference walk, straight through the library.
	var want []string
	var cursor chronicle.Cursor
	for {
		page, next, err := h.log.Query(ctx, chronicle.Query{Kind: "widget", Limit: 3, After: cursor})
		if err != nil {
			t.Fatalf("library query: %v", err)
		}
		for _, r := range page {
			want = append(want, string(r.ID))
		}
		if next.IsZero() {
			break
		}
		cursor = next
	}
	if len(want) == 0 {
		t.Fatal("reference walk returned nothing")
	}

	// HTTP walk with the same page size, cursor passed through verbatim.
	var got []string
	pages := 0
	cursorParam := ""
	for {
		path := "/v1/records?kind=widget&limit=3"
		if cursorParam != "" {
			path += "&cursor=" + url.QueryEscape(cursorParam)
		}
		resp, body := h.request("GET", path, writerToken, nil)
		wantStatus(t, resp, body, http.StatusOK)
		var page recordsResponse
		decode(t, body, &page)
		for _, r := range page.Records {
			got = append(got, r.ID)
		}
		pages++
		if page.Cursor == "" {
			break
		}
		cursorParam = page.Cursor
		if pages > 20 {
			t.Fatal("cursor walk did not terminate")
		}
	}

	if len(got) != len(want) {
		t.Fatalf("HTTP walk saw %d records, library walk %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record %d: HTTP %s, library %s", i, got[i], want[i])
		}
	}
	if pages < 3 {
		t.Fatalf("walk took %d pages; the seed should span several", pages)
	}
}

func TestQueryFilters(t *testing.T) {
	h := newHarness(t, false)

	// Two kinds, one correction, via HTTP.
	resp, body := h.request("POST", "/v1/order/o-1/records", writerToken,
		putBody(`{"total":10}`, "2026-01-01T00:00:00Z"))
	wantStatus(t, resp, body, http.StatusCreated)
	resp, body = h.request("POST", "/v1/order/o-1/corrections", adminToken,
		putBody(`{"total":12}`, "2026-01-01T00:00:00Z"))
	wantStatus(t, resp, body, http.StatusCreated)
	var corr resultDTO
	decode(t, body, &corr)
	resp, body = h.request("POST", "/v1/invoice/i-1/records", writerToken,
		putBody(`{"amount":99}`, "2026-02-01T00:00:00Z"))
	wantStatus(t, resp, body, http.StatusCreated)

	get := func(query string) recordsResponse {
		t.Helper()
		resp, body := h.request("GET", "/v1/records?"+query, writerToken, nil)
		wantStatus(t, resp, body, http.StatusOK)
		var page recordsResponse
		decode(t, body, &page)
		return page
	}

	if page := get("kind=order"); len(page.Records) != 2 {
		t.Fatalf("kind=order: %d records, want 2 (superseded included)", len(page.Records))
	}
	if page := get("kind=order&currentOnly=true"); len(page.Records) != 1 {
		t.Fatalf("kind=order currentOnly: %d records, want 1", len(page.Records))
	}
	if page := get("kind=order&entityId=o-1&intent=correction"); len(page.Records) != 1 ||
		page.Records[0].ID != corr.Record.ID {
		t.Fatalf("intent=correction: %+v, want just the correction", page.Records)
	}
	if page := get("actorId=" + adminActor.ID); len(page.Records) != 1 {
		t.Fatalf("actorId admin: %d records, want 1", len(page.Records))
	}
	// Valid-interval range: only the invoice lives in February.
	if page := get("validFrom=2026-01-15T00:00:00Z&validTo=2026-02-15T00:00:00Z&kind=invoice"); len(page.Records) != 1 {
		t.Fatalf("valid range: %d records, want 1", len(page.Records))
	}
	// txAt pinned before everything existed: nothing.
	if page := get("txAt=2000-01-01T00:00:00Z"); len(page.Records) != 0 {
		t.Fatalf("ancient txAt: %d records, want 0", len(page.Records))
	}
	// Tx range covering everything: all five records (3 writes + 1
	// superseded original still visible + remainders as written).
	if page := get("txFrom=2000-01-01T00:00:00Z"); len(page.Records) == 0 {
		t.Fatal("open tx range returned nothing")
	}
	// Descending flips the order.
	asc, desc := get("kind=order"), get("kind=order&descending=true")
	if asc.Records[0].ID != desc.Records[len(desc.Records)-1].ID {
		t.Fatalf("descending is not the reverse of ascending")
	}
}

func TestQueryLimitBounds(t *testing.T) {
	h := newHarness(t, false)

	resp, body := h.request("GET", "/v1/records?limit=1001", writerToken, nil)
	wantError(t, resp, body, http.StatusBadRequest, "invalid_argument")

	resp, body = h.request("GET", "/v1/records?limit=-3", writerToken, nil)
	wantError(t, resp, body, http.StatusBadRequest, "invalid_argument")

	resp, body = h.request("GET", "/v1/records?limit=abc", writerToken, nil)
	wantError(t, resp, body, http.StatusBadRequest, "invalid_argument")

	// A garbage cursor is the library's typed rejection, not a 500.
	resp, body = h.request("GET", "/v1/records?cursor=garbage", writerToken, nil)
	wantError(t, resp, body, http.StatusBadRequest, "invalid_cursor")

	// Every malformed filter parameter is a 400, never a scan.
	for _, qs := range []string{
		"currentOnly=perhaps", "descending=upward", "intent=hope",
		"validAt=then", "txAt=then", "validFrom=then", "validTo=then",
		"txFrom=then", "txTo=then",
	} {
		resp, body := h.request("GET", "/v1/records?"+qs, writerToken, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("?%s: status = %d, want 400; body: %s", qs, resp.StatusCode, body)
		}
	}

	// An inverted range is the library's typed rejection through
	// Query.Validate.
	resp, body = h.request("GET",
		"/v1/records?validFrom=2026-02-01T00:00:00Z&validTo=2026-01-01T00:00:00Z", writerToken, nil)
	wantError(t, resp, body, http.StatusBadRequest, "invalid_interval")
}
