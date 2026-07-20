package api

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHoldLifecycle(t *testing.T) {
	h := newHarness(t, false)

	// Place, with a backdated effective instant — the shape FRCP 37(e)
	// requires — and scope to one kind.
	resp, body := h.request("POST", "/v1/holds", adminToken, map[string]any{
		"id":            "matter-042",
		"kind":          "employee",
		"effectiveFrom": "2025-11-01T00:00:00Z",
		"reason":        "anticipated litigation",
	})
	wantStatus(t, resp, body, http.StatusCreated)
	var placed holdDTO
	decode(t, body, &placed)
	if placed.PlacedBy.ID != adminActor.ID {
		t.Fatalf("placedBy = %q, want the admin token's actor %q", placed.PlacedBy.ID, adminActor.ID)
	}
	if placed.PlacedAt == "" {
		t.Fatal("placedAt not stamped by the store")
	}
	if placed.EffectiveFrom != "2025-11-01T00:00:00Z" {
		t.Fatalf("effectiveFrom = %q, want the backdated instant preserved", placed.EffectiveFrom)
	}
	if placed.ReleasedAt != "" || placed.ReleasedBy != nil {
		t.Fatalf("fresh hold carries release fields: %+v", placed)
	}

	// Duplicate ID: holds are not upsertable.
	resp, body = h.request("POST", "/v1/holds", adminToken, map[string]any{"id": "matter-042"})
	wantError(t, resp, body, http.StatusConflict, "hold_exists")

	// Missing ID: the library's typed rejection.
	resp, body = h.request("POST", "/v1/holds", adminToken, map[string]any{"kind": "employee"})
	wantError(t, resp, body, http.StatusBadRequest, "missing_hold_id")

	// Bad effectiveFrom: parse error.
	resp, body = h.request("POST", "/v1/holds", adminToken,
		map[string]any{"id": "matter-043", "effectiveFrom": "soon"})
	wantError(t, resp, body, http.StatusBadRequest, "invalid_argument")

	// List shows it.
	resp, body = h.request("GET", "/v1/holds", adminToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var list map[string][]holdDTO
	decode(t, body, &list)
	if len(list["holds"]) != 1 || list["holds"][0].ID != "matter-042" {
		t.Fatalf("holds list = %+v, want the one placed hold", list)
	}

	// Release: attribution stamped from the token, reason recorded, row kept.
	resp, body = h.request("POST", "/v1/holds/matter-042/release", adminToken,
		map[string]any{"reason": "matter settled"})
	wantStatus(t, resp, body, http.StatusOK)
	var released holdDTO
	decode(t, body, &released)
	if released.ReleasedBy == nil || released.ReleasedBy.ID != adminActor.ID {
		t.Fatalf("releasedBy = %+v, want the admin actor", released.ReleasedBy)
	}
	if released.ReleasedAt == "" || released.ReleaseReason != "matter settled" {
		t.Fatalf("release fields = %+v", released)
	}

	// A second release is a 409, not a quiet rewrite of the attribution.
	resp, body = h.request("POST", "/v1/holds/matter-042/release", adminToken, map[string]any{})
	wantError(t, resp, body, http.StatusConflict, "hold_released")

	// Releasing a hold that never existed is 404.
	resp, body = h.request("POST", "/v1/holds/matter-999/release", adminToken, map[string]any{})
	wantError(t, resp, body, http.StatusNotFound, "not_found")

	// The released hold survives in the list — the lifecycle is the value.
	resp, body = h.request("GET", "/v1/holds", adminToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	decode(t, body, &list)
	if len(list["holds"]) != 1 || list["holds"][0].ReleasedAt == "" {
		t.Fatalf("released hold missing from list: %+v", list)
	}
}

func TestRetentionSweep(t *testing.T) {
	h := newHarness(t, false)

	// Two versions: the first becomes superseded and thus sweep-eligible.
	resp, body := h.request("POST", "/v1/employee/vic/records", writerToken,
		putBody(`{"v":1}`, "2026-01-01T00:00:00Z"))
	wantStatus(t, resp, body, http.StatusCreated)
	resp, body = h.request("POST", "/v1/employee/vic/records", writerToken,
		putBody(`{"v":2}`, "2026-01-01T00:00:00Z"))
	wantStatus(t, resp, body, http.StatusCreated)

	// An empty policy list is refused: no default retention on purpose.
	resp, body = h.request("POST", "/v1/retention/sweep", adminToken,
		map[string]any{"policies": []any{}})
	wantError(t, resp, body, http.StatusBadRequest, "no_policy")

	// So is a malformed duration.
	resp, body = h.request("POST", "/v1/retention/sweep", adminToken,
		map[string]any{"policies": []map[string]string{{"kind": "employee", "keepFor": "forever"}}})
	wantError(t, resp, body, http.StatusBadRequest, "invalid_policy")

	// And a non-positive one — the library's ErrInvalidPolicy.
	resp, body = h.request("POST", "/v1/retention/sweep", adminToken,
		map[string]any{"policies": []map[string]string{{"kind": "employee", "keepFor": "-1h"}}})
	wantError(t, resp, body, http.StatusBadRequest, "invalid_policy")

	// Give the superseded record a moment of age past the tiny KeepFor.
	time.Sleep(20 * time.Millisecond)
	policies := map[string]any{
		"policies": []map[string]string{{"kind": "employee", "keepFor": "1ns"}},
		"dryRun":   true,
	}

	// Dry run: reports what would be destroyed, destroys nothing.
	resp, body = h.request("POST", "/v1/retention/sweep", adminToken, policies)
	wantStatus(t, resp, body, http.StatusOK)
	var plan reportDTO
	decode(t, body, &plan)
	if plan.Executed {
		t.Fatal("dry run reported executed=true")
	}
	if len(plan.Kinds) != 1 || plan.Kinds[0].Deleted != 1 {
		t.Fatalf("dry run = %+v, want the one superseded record eligible", plan)
	}
	if plan.Kinds[0].Cutoff == "" || plan.Now == "" {
		t.Fatalf("report must state its arithmetic: %+v", plan)
	}
	resp, body = h.request("GET", "/v1/employee/vic/history", writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var hist recordsResponse
	decode(t, body, &hist)
	if len(hist.Records) != 2 {
		t.Fatalf("dry run destroyed something: %d records left", len(hist.Records))
	}

	// A hold over the kind withholds the eligible record, and says which
	// hold saved it.
	resp, body = h.request("POST", "/v1/holds", adminToken,
		map[string]any{"id": "hold-vic", "kind": "employee"})
	wantStatus(t, resp, body, http.StatusCreated)
	resp, body = h.request("POST", "/v1/retention/sweep", adminToken, policies)
	wantStatus(t, resp, body, http.StatusOK)
	var withHold reportDTO
	decode(t, body, &withHold)
	if len(withHold.Kinds) != 1 || withHold.Kinds[0].Deleted != 0 ||
		len(withHold.Kinds[0].Withheld) != 1 || withHold.Kinds[0].Withheld[0].HoldID != "hold-vic" {
		t.Fatalf("hold did not withhold: %+v", withHold.Kinds)
	}

	// Release the hold; execute for real.
	resp, body = h.request("POST", "/v1/holds/hold-vic/release", adminToken, map[string]any{})
	wantStatus(t, resp, body, http.StatusOK)
	policies["dryRun"] = false
	resp, body = h.request("POST", "/v1/retention/sweep", adminToken, policies)
	wantStatus(t, resp, body, http.StatusOK)
	var exec reportDTO
	decode(t, body, &exec)
	if !exec.Executed || exec.Kinds[0].Deleted != 1 {
		t.Fatalf("execute = %+v, want one record destroyed", exec)
	}

	// The superseded record is gone; current belief is untouched.
	resp, body = h.request("GET", "/v1/employee/vic/history", writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	decode(t, body, &hist)
	if len(hist.Records) != 1 {
		t.Fatalf("history after sweep = %d records, want 1", len(hist.Records))
	}
	resp, body = h.request("GET", "/v1/employee/vic", writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var current recordDTO
	decode(t, body, &current)
	if !strings.Contains(string(current.Data), `"v":2`) {
		t.Fatalf("current belief after sweep = %s", current.Data)
	}
}

func TestCryptoShredding(t *testing.T) {
	h := newHarness(t, false)

	// A record written for a subject reads back as plaintext while the key
	// lives.
	resp, body := h.request("POST", "/v1/patient/p-7/records", writerToken, map[string]any{
		"data":      map[string]any{"diagnosis": "sensitive"},
		"validFrom": "2026-01-01T00:00:00Z",
		"subject":   "subject-p7",
	})
	wantStatus(t, resp, body, http.StatusCreated)
	resp, body = h.request("GET", "/v1/patient/p-7", writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var rec recordDTO
	decode(t, body, &rec)
	if !strings.Contains(string(rec.Data), "sensitive") {
		t.Fatalf("pre-shred read = %s, want plaintext", rec.Data)
	}

	// History returns the record as stored: ciphertext, so dataBase64.
	resp, body = h.request("GET", "/v1/patient/p-7/history", writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var hist recordsResponse
	decode(t, body, &hist)
	if len(hist.Records) != 1 || hist.Records[0].DataBase64 == "" || hist.Records[0].Data != nil {
		t.Fatalf("history of encrypted record = %+v, want dataBase64 only", hist.Records)
	}

	// Destroy the key. The response carries the honest hedge, verbatim
	// concern: mechanism described, legal characterization left open.
	resp, body = h.request("POST", "/v1/subjects/subject-p7/destroy-key", adminToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var destroyed map[string]any
	decode(t, body, &destroyed)
	if destroyed["destroyed"] != true || destroyed["subject"] != "subject-p7" {
		t.Fatalf("destroy response = %s", body)
	}
	note, _ := destroyed["note"].(string)
	if !strings.Contains(note, "Art. 17") || !strings.Contains(note, "no compliance claim") {
		t.Fatalf("destroy response note lost the compliance hedge: %q", note)
	}

	// Idempotent.
	resp, body = h.request("POST", "/v1/subjects/subject-p7/destroy-key", adminToken, nil)
	wantStatus(t, resp, body, http.StatusOK)

	// Reads of the shredded value fail loudly, never returning ciphertext
	// where plaintext was asked for.
	resp, body = h.request("GET", "/v1/patient/p-7", writerToken, nil)
	wantError(t, resp, body, http.StatusGone, "shredded")

	// The structure survives: history still lists the record.
	resp, body = h.request("GET", "/v1/patient/p-7/history", writerToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	decode(t, body, &hist)
	if len(hist.Records) != 1 {
		t.Fatalf("shredding removed the record from history: %+v", hist.Records)
	}

	// New writes under the destroyed subject are refused: destruction is
	// terminal, and a quietly re-minted key would undo the erasure.
	resp, body = h.request("POST", "/v1/patient/p-7/records", writerToken, map[string]any{
		"data":      map[string]any{"diagnosis": "new"},
		"validFrom": "2026-02-01T00:00:00Z",
		"subject":   "subject-p7",
	})
	wantError(t, resp, body, http.StatusGone, "key_destroyed")
}

func TestChainVerification(t *testing.T) {
	h := newHarness(t, true) // chaining on

	resp, body := h.request("POST", "/v1/device/d-1/records", writerToken,
		putBody(`{"fw":"1.0"}`, "2026-01-01T00:00:00Z"))
	wantStatus(t, resp, body, http.StatusCreated)
	resp, body = h.request("POST", "/v1/device/d-1/corrections", writerToken,
		putBody(`{"fw":"1.1"}`, "2026-01-01T00:00:00Z"))
	wantStatus(t, resp, body, http.StatusCreated)

	// Verify: intact, covering both records.
	resp, body = h.request("GET", "/v1/device/d-1/verify", adminToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var report verifyDTO
	decode(t, body, &report)
	if !report.Intact || report.ChainedRecords != 2 || report.Divergence != nil {
		t.Fatalf("verify = %+v, want an intact 2-record chain", report)
	}
	if report.Head == "" {
		t.Fatal("intact chain reported no head")
	}

	// The chain head endpoint returns the same value, hex-encoded.
	resp, body = h.request("GET", "/v1/device/d-1/chain-head", adminToken, nil)
	wantStatus(t, resp, body, http.StatusOK)
	var head map[string]string
	decode(t, body, &head)
	if head["head"] != report.Head {
		t.Fatalf("chain-head = %q, verify head = %q", head["head"], report.Head)
	}

	// An entity with no chain is 404 no_chain — never a passed verification.
	resp, body = h.request("GET", "/v1/device/ghost/verify", adminToken, nil)
	wantError(t, resp, body, http.StatusNotFound, "no_chain")
	resp, body = h.request("GET", "/v1/device/ghost/chain-head", adminToken, nil)
	wantError(t, resp, body, http.StatusNotFound, "no_chain")
}

// TestChainVerifyWithoutChaining: a server running with chaining off still
// answers verify — an auditor reading what writers chained — but entities
// written unchained report no_chain.
func TestChainVerifyWithoutChaining(t *testing.T) {
	h := newHarness(t, false)
	resp, body := h.request("POST", "/v1/device/d-2/records", writerToken,
		putBody(`{"fw":"1.0"}`, "2026-01-01T00:00:00Z"))
	wantStatus(t, resp, body, http.StatusCreated)
	resp, body = h.request("GET", "/v1/device/d-2/verify", adminToken, nil)
	wantError(t, resp, body, http.StatusNotFound, "no_chain")
}
