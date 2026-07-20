package chronicle

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// seedRecords writes records straight to the store so that a test can
// construct pathological orderings — in particular large groups of records
// sharing a transaction instant and a valid start — that the write path's
// monotonic ratchet would otherwise never produce.
func seedRecords(t *testing.T, store *MemStore, recs []Record) {
	t.Helper()
	if err := store.Put(context.Background(), recs); err != nil {
		t.Fatalf("seeding the store failed: %v", err)
	}
}

// drainPages walks every page of a query and returns the records in order,
// asserting that no page exceeds the limit.
func drainPages(t *testing.T, l *Log, q Query) []Record {
	t.Helper()
	ctx := context.Background()
	var all []Record
	seen := map[RecordID]bool{}
	cursor := NoCursor

	for page := 0; ; page++ {
		if page > 1000 {
			t.Fatal("pagination did not terminate after 1000 pages")
		}
		pq := q
		pq.After = cursor
		recs, next, err := l.Query(ctx, pq)
		if err != nil {
			t.Fatalf("Query page %d failed: %v", page, err)
		}
		if q.Limit > 0 && len(recs) > q.Limit {
			t.Fatalf("page %d returned %d records; limit was %d", page, len(recs), q.Limit)
		}
		for _, r := range recs {
			if seen[r.ID] {
				t.Fatalf("record %s returned on more than one page", r.ID)
			}
			seen[r.ID] = true
			all = append(all, r)
		}
		if next.IsZero() {
			if len(recs) == 0 && page == 0 {
				return all
			}
			return all
		}
		if len(recs) == 0 {
			t.Fatalf("page %d was empty but returned a non-empty cursor", page)
		}
		cursor = next
	}
}

func TestPagination(t *testing.T) {
	ctx := context.Background()

	t.Run("given a log with many records", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		const n = 37
		for i := 0; i < n; i++ {
			clock.Advance(time.Hour)
			from := t1.AddDate(0, 0, i*2)
			mustPut(t, l, fmt.Sprintf("e-%d", i), fmt.Sprintf("v%d", i), from, from.AddDate(0, 0, 1))
		}

		full := drainPages(t, l, Query{Kind: employee})

		t.Run("when paged at every page size", func(t *testing.T) {
			for _, limit := range []int{1, 2, 3, 5, 7, 36, 37, 38, 100} {
				t.Run(fmt.Sprintf("then a limit of %d yields the same records in the same order", limit), func(t *testing.T) {
					got := drainPages(t, l, Query{Kind: employee, Limit: limit})
					if len(got) != len(full) {
						t.Fatalf("paging at %d returned %d records; want %d", limit, len(got), len(full))
					}
					for i := range got {
						if got[i].ID != full[i].ID {
							t.Fatalf("paging at %d diverged at index %d: got %s, want %s",
								limit, i, got[i].ID, full[i].ID)
						}
					}
				})
			}
		})

		t.Run("when the last page exactly fills the limit", func(t *testing.T) {
			t.Run("then no trailing empty page is required", func(t *testing.T) {
				// n is 37 plus the remainder records; find a limit that
				// divides the total exactly.
				limit := len(full)
				recs, next, err := l.Query(ctx, Query{Kind: employee, Limit: limit})
				if err != nil {
					t.Fatalf("Query failed: %v", err)
				}
				if len(recs) != limit {
					t.Fatalf("got %d records; want %d", len(recs), limit)
				}
				if !next.IsZero() {
					t.Fatal("a cursor was returned even though the result set was exhausted")
				}
			})
		})

		t.Run("when paged in descending order", func(t *testing.T) {
			t.Run("then every record appears exactly once, reversed", func(t *testing.T) {
				got := drainPages(t, l, Query{Kind: employee, Limit: 4, Descending: true})
				if len(got) != len(full) {
					t.Fatalf("descending paging returned %d records; want %d", len(got), len(full))
				}
				for i := range got {
					want := full[len(full)-1-i]
					if got[i].ID != want.ID {
						t.Fatalf("descending order diverged at %d: got %s, want %s", i, got[i].ID, want.ID)
					}
				}
			})
		})
	})

	t.Run("given many records tied on both sort timestamps", func(t *testing.T) {
		// Every record shares a transaction instant and a valid start, so the
		// record ID is the only thing separating them. This is the case that
		// breaks naive offset pagination and naive keyset pagination alike.
		store := NewMemStore()
		l := NewLog(store)

		const n = 25
		recs := make([]Record, 0, n)
		for i := 0; i < n; i++ {
			recs = append(recs, Record{
				ID:        RecordID(fmt.Sprintf("tie-%03d", i)),
				Kind:      employee,
				EntityID:  "shared",
				Data:      []byte(fmt.Sprintf(`{"i":%d}`, i)),
				ValidFrom: t1,
				ValidTo:   t3,
				TxFrom:    t2,
				Actor:     alice,
			})
		}
		seedRecords(t, store, recs)

		t.Run("when paged at a size that splits the tie group", func(t *testing.T) {
			for _, limit := range []int{1, 2, 4, 7, 24, 25, 26} {
				t.Run(fmt.Sprintf("then a limit of %d returns all %d exactly once", limit, n), func(t *testing.T) {
					got := drainPages(t, l, Query{Kind: employee, Limit: limit})
					if len(got) != n {
						t.Fatalf("paging at %d returned %d records; want %d — a tie group was "+
							"split incorrectly across a page boundary", limit, len(got), n)
					}
					for i := 1; i < len(got); i++ {
						if got[i-1].ID >= got[i].ID {
							t.Fatalf("records out of order across pages: %s then %s", got[i-1].ID, got[i].ID)
						}
					}
				})
			}
		})

		t.Run("when paged descending through the tie group", func(t *testing.T) {
			t.Run("then all records are returned exactly once", func(t *testing.T) {
				got := drainPages(t, l, Query{Kind: employee, Limit: 3, Descending: true})
				if len(got) != n {
					t.Fatalf("descending paging returned %d records; want %d", len(got), n)
				}
			})
		})
	})

	t.Run("given records with unbounded valid starts", func(t *testing.T) {
		store := NewMemStore()
		l := NewLog(store)
		recs := []Record{
			{ID: "a", Kind: employee, EntityID: "e", TxFrom: t1, ValidTo: t2, Actor: alice},
			{ID: "b", Kind: employee, EntityID: "e", TxFrom: t1, ValidFrom: t2, ValidTo: t3, Actor: alice},
			{ID: "c", Kind: employee, EntityID: "e", TxFrom: t1, ValidFrom: t3, Actor: alice},
		}
		seedRecords(t, store, recs)

		t.Run("when paged one at a time", func(t *testing.T) {
			got := drainPages(t, l, Query{Kind: employee, Limit: 1})
			t.Run("then all records are returned", func(t *testing.T) {
				if len(got) != 3 {
					t.Fatalf("got %d records; want 3 — an unbounded valid start must survive a cursor round trip", len(got))
				}
			})
			t.Run("then the unbounded start sorts first", func(t *testing.T) {
				if got[0].ID != "a" {
					t.Fatalf("first record = %s; want a, the unbounded start", got[0].ID)
				}
			})
		})
	})

	t.Run("given a cursor from a previous page", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		for i := 0; i < 5; i++ {
			clock.Advance(time.Hour)
			mustPut(t, l, fmt.Sprintf("e-%d", i), "v", t1, t3)
		}
		_, cursor, err := l.Query(ctx, Query{Kind: employee, Limit: 2})
		if err != nil {
			t.Fatalf("Query failed: %v", err)
		}

		t.Run("when it is inspected", func(t *testing.T) {
			t.Run("then it is opaque and URL-safe", func(t *testing.T) {
				if cursor.IsZero() {
					t.Fatal("no cursor was returned")
				}
				s := cursor.String()
				if strings.ContainsAny(s, "+/= &?#") {
					t.Fatalf("cursor %q is not URL-safe", s)
				}
				if strings.Contains(s, "2026") {
					t.Fatalf("cursor %q leaks its internal structure", s)
				}
			})
			t.Run("then it is stable across identical queries", func(t *testing.T) {
				_, again, err := l.Query(ctx, Query{Kind: employee, Limit: 2})
				if err != nil {
					t.Fatalf("Query failed: %v", err)
				}
				if again != cursor {
					t.Fatalf("the same query produced two different cursors: %q and %q", cursor, again)
				}
			})
		})

		t.Run("when it is corrupted", func(t *testing.T) {
			cases := []struct {
				name string
				c    Cursor
			}{
				{"not base64", Cursor("!!!not base64!!!")},
				{"unstructured payload", Cursor(base64.RawURLEncoding.EncodeToString([]byte("garbage")))},
				{"too few fields", Cursor(base64.RawURLEncoding.EncodeToString([]byte("c1\x1fa\x1fb")))},
				{"wrong version", Cursor(base64.RawURLEncoding.EncodeToString([]byte("c9\x1f\x1f\x1fid\x1f0")))},
				{"bad checksum", Cursor(base64.RawURLEncoding.EncodeToString([]byte("c1\x1f\x1f\x1fid\x1fzzz")))},
				{"truncated", cursor[:len(cursor)-4]},
			}
			for _, tc := range cases {
				t.Run("then a "+tc.name+" cursor is rejected", func(t *testing.T) {
					_, _, err := l.Query(ctx, Query{Kind: employee, Limit: 2, After: tc.c})
					if !errors.Is(err, ErrInvalidCursor) {
						t.Fatalf("Query with a %s cursor = %v; want ErrInvalidCursor", tc.name, err)
					}
					var ce *CursorError
					if !errors.As(err, &ce) || ce.Error() == "" {
						t.Fatalf("error = %v; want a *CursorError with a message", err)
					}
				})
			}
		})

		t.Run("when its timestamps are tampered with", func(t *testing.T) {
			t.Run("then the checksum rejects it", func(t *testing.T) {
				raw, err := base64.RawURLEncoding.DecodeString(string(cursor))
				if err != nil {
					t.Fatalf("decoding the cursor failed: %v", err)
				}
				parts := strings.Split(string(raw), "\x1f")
				parts[1] = t5.Format(time.RFC3339Nano)
				tampered := Cursor(base64.RawURLEncoding.EncodeToString([]byte(strings.Join(parts, "\x1f"))))
				_, _, err = l.Query(ctx, Query{Kind: employee, After: tampered})
				if !errors.Is(err, ErrInvalidCursor) {
					t.Fatalf("a tampered cursor = %v; want ErrInvalidCursor", err)
				}
			})
		})

		t.Run("when an unparseable time survives the checksum", func(t *testing.T) {
			t.Run("then decoding still rejects it", func(t *testing.T) {
				for _, bad := range []string{"not-a-time\x1f\x1fid", "\x1fnot-a-time\x1fid"} {
					payload := "c1\x1f" + bad
					c := Cursor(base64.RawURLEncoding.EncodeToString([]byte(payload + "\x1f" + checksumOf(payload))))
					if _, err := decodeCursor(c); !errors.Is(err, ErrInvalidCursor) {
						t.Fatalf("decodeCursor(%q) = %v; want ErrInvalidCursor", payload, err)
					}
				}
			})
		})
	})

	t.Run("given a cursor round trip", func(t *testing.T) {
		cases := []Record{
			{ID: "plain", TxFrom: t1, ValidFrom: t2},
			{ID: "unbounded valid start", TxFrom: t1},
			{ID: "sub-second precision", TxFrom: t1.Add(123456789 * time.Nanosecond), ValidFrom: t2.Add(987654321 * time.Nanosecond)},
			{ID: "id with punctuation -_.", TxFrom: t1, ValidFrom: t2},
		}
		for _, rec := range cases {
			t.Run("when the record is "+string(rec.ID), func(t *testing.T) {
				t.Run("then it decodes to the same key", func(t *testing.T) {
					got, err := decodeCursor(encodeCursor(rec))
					if err != nil {
						t.Fatalf("decodeCursor failed: %v", err)
					}
					if got.ID != rec.ID || !got.TxFrom.Equal(rec.TxFrom) || !got.ValidFrom.Equal(rec.ValidFrom) {
						t.Fatalf("round trip = %+v; want %s / %s / %s", got, rec.ID, rec.TxFrom, rec.ValidFrom)
					}
					if rec.ValidFrom.IsZero() && !got.ValidFrom.IsZero() {
						t.Fatal("an unbounded valid start did not survive the round trip")
					}
				})
			})
		}
	})

	t.Run("given the empty cursor", func(t *testing.T) {
		t.Run("when it is used as a starting point", func(t *testing.T) {
			t.Run("then it means the beginning rather than an error", func(t *testing.T) {
				l, _, _ := newTestLog(t)
				mustPut(t, l, "e", "v", t1, t3)
				recs, _, err := l.Query(ctx, Query{Kind: employee, After: NoCursor})
				if err != nil {
					t.Fatalf("Query with NoCursor = %v; want no error", err)
				}
				if len(recs) == 0 {
					t.Fatal("Query with NoCursor returned nothing")
				}
			})
		})
	})
}

func checksumOf(payload string) string { return checksumString(payload) }
