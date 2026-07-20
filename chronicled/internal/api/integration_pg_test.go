package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/zkrebbekx/chronicle"
	"github.com/zkrebbekx/chronicle/pgstore"
)

// The integration suite: the same HTTP handler over a live Postgres, gated on
// CHRONICLE_TEST_DSN and skipping cleanly without it. Each test isolates in
// its own schema, dropped afterwards, mirroring pgstore's own suite.

func pgHarness(t *testing.T, chaining bool) *struct {
	srv *httptest.Server
	log *chronicle.Log
} {
	t.Helper()
	dsn := os.Getenv("CHRONICLE_TEST_DSN")
	if dsn == "" {
		t.Skip("CHRONICLE_TEST_DSN not set; skipping Postgres integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	schema := fmt.Sprintf("chronicled_it_%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	})

	store, err := pgstore.New(db, pgstore.WithSchema(schema))
	if err != nil {
		t.Fatalf("pgstore.New: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	keyring, err := pgstore.NewKeyring(db, pgstore.WithSchema(schema))
	if err != nil {
		t.Fatalf("pgstore.NewKeyring: %v", err)
	}
	if err := keyring.Migrate(ctx); err != nil {
		t.Fatalf("migrate keyring: %v", err)
	}

	opts := []chronicle.Option{chronicle.WithKeyring(keyring)}
	if chaining {
		opts = append(opts, chronicle.WithChaining())
	}
	log := chronicle.NewLog(store, opts...)
	srv := httptest.NewServer(NewHandler(Deps{
		Log:     log,
		Store:   store,
		Keyring: keyring,
		Auth:    testAuth(t),
		Logger:  discardLogger(),
		Ready:   db.PingContext,
	}))
	t.Cleanup(srv.Close)
	return &struct {
		srv *httptest.Server
		log *chronicle.Log
	}{srv, log}
}

// TestPGBitemporalFlagship is the flagship scenario against the real store:
// put, retroactive correct, read pinned to the original belief.
func TestPGBitemporalFlagship(t *testing.T) {
	h := pgHarness(t, false)

	resp, body := doRequest(t, h.srv, "POST", "/v1/employee/alice/records", writerToken,
		putBody(`{"salary":50000}`, "2026-03-01T00:00:00Z"))
	wantStatus(t, resp, body, http.StatusCreated)
	var put resultDTO
	decode(t, body, &put)

	// pgstore assigns transaction time database-side and truncates to
	// microseconds; the returned instant must reflect that.
	txAt, err := time.Parse(time.RFC3339Nano, put.TxAt)
	if err != nil {
		t.Fatalf("txAt %q: %v", put.TxAt, err)
	}
	if txAt.Nanosecond()%1000 != 0 {
		t.Fatalf("txAt %v has sub-microsecond precision; pgstore should truncate", txAt)
	}

	resp, body = doRequest(t, h.srv, "POST", "/v1/employee/alice/corrections", writerToken,
		putBody(`{"salary":55000}`, "2026-03-01T00:00:00Z"))
	wantStatus(t, resp, body, http.StatusCreated)

	march := "2026-03-15T00:00:00Z"
	resp, body = doRequest(t, h.srv, "GET", "/v1/employee/alice?validAt="+march, writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var now recordDTO
	decode(t, body, &now)
	if !strings.Contains(string(now.Data), "55000") {
		t.Fatalf("current belief = %s", now.Data)
	}

	resp, body = doRequest(t, h.srv, "GET",
		"/v1/employee/alice?validAt="+march+"&txAt="+url.QueryEscape(put.TxAt), writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var then recordDTO
	decode(t, body, &then)
	if !strings.Contains(string(then.Data), "50000") {
		t.Fatalf("pinned belief = %s, want the original 50000", then.Data)
	}
	if then.ID != put.Record.ID {
		t.Fatalf("pinned read = %s, want %s", then.ID, put.Record.ID)
	}
}

// TestPGCursorPassThrough: the HTTP pagination walk equals the library walk
// with keyset pagination pushed down into SQL.
func TestPGCursorPassThrough(t *testing.T) {
	h := pgHarness(t, false)
	ctx := context.Background()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		if _, err := h.log.Put(ctx, "widget", fmt.Sprintf("e-%d", i%3),
			[]byte(fmt.Sprintf(`{"n":%d}`, i)), base.AddDate(0, 0, i), time.Time{}, writerActor); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	var want []string
	var cursor chronicle.Cursor
	for {
		page, next, err := h.log.Query(ctx, chronicle.Query{Kind: "widget", Limit: 3, After: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page {
			want = append(want, string(r.ID))
		}
		if next.IsZero() {
			break
		}
		cursor = next
	}

	var got []string
	cursorParam := ""
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("walk did not terminate")
		}
		path := "/v1/records?kind=widget&limit=3"
		if cursorParam != "" {
			path += "&cursor=" + url.QueryEscape(cursorParam)
		}
		resp, body := doRequest(t, h.srv, "GET", path, writerToken, nil)
		wantStatus(t, resp, body, http.StatusOK)
		var page recordsResponse
		decode(t, body, &page)
		for _, r := range page.Records {
			got = append(got, r.ID)
		}
		if page.Cursor == "" {
			break
		}
		cursorParam = page.Cursor
	}

	// 8 asserts plus the remainders their supersessions produced; the exact
	// total belongs to the library. What this test owns is the equality of
	// the two walks.
	if len(got) != len(want) || len(got) < 8 {
		t.Fatalf("HTTP walk %d records, library walk %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record %d: HTTP %s, library %s", i, got[i], want[i])
		}
	}
}

// TestPGComplianceRoundTrip: holds, retention with withholding, shredding and
// chain verification against the real store in one flow.
func TestPGComplianceRoundTrip(t *testing.T) {
	h := pgHarness(t, true) // chaining on

	// Two versions so one is superseded and sweepable.
	resp, body := doRequest(t, h.srv, "POST", "/v1/patient/p1/records", writerToken, map[string]any{
		"data": map[string]any{"status": "admitted"}, "validFrom": "2026-01-01T00:00:00Z",
		"subject": "subj-p1",
	})
	wantStatus(t, resp, body, http.StatusCreated)
	resp, body = doRequest(t, h.srv, "POST", "/v1/patient/p1/records", writerToken, map[string]any{
		"data": map[string]any{"status": "discharged"}, "validFrom": "2026-01-01T00:00:00Z",
		"subject": "subj-p1",
	})
	wantStatus(t, resp, body, http.StatusCreated)

	// Chain verifies over the ciphertext.
	resp, body = doRequest(t, h.srv, "GET", "/v1/patient/p1/verify", adminToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var report verifyDTO
	decode(t, body, &report)
	if !report.Intact || report.ChainedRecords != 2 {
		t.Fatalf("verify = %+v", report)
	}

	// Hold, then sweep: withheld. PlacedAt comes from the database clock.
	resp, body = doRequest(t, h.srv, "POST", "/v1/holds", adminToken, map[string]any{
		"id": "pg-hold", "kind": "patient", "effectiveFrom": "2025-01-01T00:00:00Z",
	})
	wantStatus(t, resp, body, http.StatusCreated)
	var placed holdDTO
	decode(t, body, &placed)
	if placed.PlacedAt == "" || placed.PlacedBy.ID != adminActor.ID {
		t.Fatalf("hold = %+v", placed)
	}

	time.Sleep(50 * time.Millisecond) // age the superseded record past 1ns
	sweep := map[string]any{
		"policies": []map[string]string{{"kind": "patient", "keepFor": "1ns"}},
		"dryRun":   false,
	}
	resp, body = doRequest(t, h.srv, "POST", "/v1/retention/sweep", adminToken, sweep)
	wantStatus(t, resp, body, http.StatusOK)
	var withheld reportDTO
	decode(t, body, &withheld)
	if withheld.Kinds[0].Deleted != 0 || len(withheld.Kinds[0].Withheld) != 1 {
		t.Fatalf("hold did not withhold: %+v", withheld.Kinds)
	}

	// Release and sweep for real: the chained record leaves a tombstone and
	// the chain still verifies across the gap.
	resp, body = doRequest(t, h.srv, "POST", "/v1/holds/pg-hold/release", adminToken, map[string]any{})
	wantStatus(t, resp, body, http.StatusOK)
	// ReleasedAt is stamped by the database clock while the sweep's "now"
	// comes from this process's; give the release a moment so millisecond
	// skew between the two cannot leave the hold notionally active. This is
	// the skew retain.Execute documents, observed in the wild by this very
	// test.
	time.Sleep(250 * time.Millisecond)
	resp, body = doRequest(t, h.srv, "POST", "/v1/retention/sweep", adminToken, sweep)
	wantStatus(t, resp, body, http.StatusOK)
	var exec reportDTO
	decode(t, body, &exec)
	if !exec.Executed || exec.Kinds[0].Deleted != 1 || exec.Kinds[0].Tombstones != 1 {
		t.Fatalf("execute = %+v", exec.Kinds)
	}
	resp, body = doRequest(t, h.srv, "GET", "/v1/patient/p1/verify", adminToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	decode(t, body, &report)
	if !report.Intact || report.Tombstones != 1 || report.ChainedRecords != 1 {
		t.Fatalf("post-sweep verify = %+v", report)
	}

	// Shred the subject; the read fails loudly, structure survives.
	resp, body = doRequest(t, h.srv, "POST", "/v1/subjects/subj-p1/destroy-key", adminToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	resp, body = doRequest(t, h.srv, "GET", "/v1/patient/p1", writerToken, nil)
	wantError(t, resp, body, http.StatusGone, "shredded")
	resp, body = doRequest(t, h.srv, "GET", "/v1/patient/p1/history", writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var hist recordsResponse
	decode(t, body, &hist)
	if len(hist.Records) != 1 || hist.Records[0].DataBase64 == "" {
		t.Fatalf("post-shred history = %+v", hist.Records)
	}
	// Shredding broke no chain: the hash covers ciphertext.
	resp, body = doRequest(t, h.srv, "GET", "/v1/patient/p1/verify", adminToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	decode(t, body, &report)
	if !report.Intact {
		t.Fatalf("shredding broke the chain: %+v", report)
	}
}
