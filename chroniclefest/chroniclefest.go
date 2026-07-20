// Package chroniclefest is an executable specification of the [chronicle.Store]
// contract.
//
// It exists because chronicle ships more than one store and intends to accept
// third-party ones. A behavioural contract that lives only in the reference
// implementation's own tests is not a contract at all: the second
// implementation gets written against whatever the first one happened to do,
// and the two drift on exactly the cases nobody thought to write down —
// half-open boundaries, cursor ties, whether a supersession is idempotent.
//
// Point [Run] at a factory and it exercises the whole surface:
//
//	func TestMemStoreConformance(t *testing.T) {
//	    chroniclefest.Run(t, func(t *testing.T) chronicle.Store {
//	        return chronicle.NewMemStore()
//	    })
//	}
//
// The suite is deliberately written to be agnostic about transaction time. A
// store may assign its own — and any store shared between processes must — so
// nothing here asserts a transaction timestamp the suite chose. Every
// assertion about the transaction axis is made against the instant
// [chronicle.Store.Apply] returned. Valid time is the caller's and is asserted
// exactly.
//
// Timestamps used by the suite are whole seconds, because a store may keep
// less than nanosecond resolution: Postgres timestamptz holds microseconds.
// A store that truncates valid times below the second is out of contract.
//
// This package imports testing and is meant to be called from a test. It is a
// normal package rather than a test file so that stores outside this module can
// use it, which is the whole point.
package chroniclefest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zkrebbekx/chronicle"
)

// Factory returns a fresh, empty store for one test. It is called many times
// over a single Run, and each store must be independent of the others.
//
// Register any teardown with t.Cleanup; the suite never closes what it is
// given, since closing is not part of the [chronicle.Store] contract.
type Factory func(t *testing.T) chronicle.Store

// Suite time constants. Whole seconds, well apart, and in the past relative to
// any plausible test run, so that a store assigning transaction time from the
// wall clock does not collide with the valid-time axis.
var (
	jan = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	feb = time.Date(2020, 2, 1, 0, 0, 0, 0, time.UTC)
	mar = time.Date(2020, 3, 1, 0, 0, 0, 0, time.UTC)
	apr = time.Date(2020, 4, 1, 0, 0, 0, 0, time.UTC)
	may = time.Date(2020, 5, 1, 0, 0, 0, 0, time.UTC)
	jun = time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC)
)

var (
	alice = chronicle.Actor{ID: "u-alice", Type: "user", Name: "Alice"}
	bob   = chronicle.Actor{ID: "u-bob", Type: "service", Name: "Bob"}
)

const (
	employee = "employee"
	invoice  = "invoice"
)

// Run executes the whole conformance suite against stores from newStore.
func Run(t *testing.T, newStore Factory) {
	t.Helper()
	t.Run("store contract", func(t *testing.T) { runStore(t, newStore) })
	t.Run("log contract", func(t *testing.T) { runLog(t, newStore) })
}

// RunStore executes only the store-level half of the suite: what Apply, Get and
// Query do, with no bitemporal reasoning layered on top.
func RunStore(t *testing.T, newStore Factory) {
	t.Helper()
	runStore(t, newStore)
}

// RunLog executes only the half that drives a [chronicle.Log] over the store,
// checking that the bitemporal engine gets the answers it expects back.
func RunLog(t *testing.T, newStore Factory) {
	t.Helper()
	runLog(t, newStore)
}

// ---------------------------------------------------------------------------
// store-level contract
// ---------------------------------------------------------------------------

func runStore(t *testing.T, newStore Factory) {
	t.Run("given an empty store", func(t *testing.T) {
		ctx := context.Background()
		s := newStore(t)

		t.Run("when a record is looked up", func(t *testing.T) {
			_, err := s.Get(ctx, chronicle.GetQuery{Kind: employee, EntityID: "e1", ValidAt: feb, TxAt: feb})
			t.Run("then the lookup reports not found", func(t *testing.T) {
				if !errors.Is(err, chronicle.ErrNotFound) {
					t.Fatalf("Get = %v; want ErrNotFound", err)
				}
			})
			t.Run("then the error carries the coordinates searched", func(t *testing.T) {
				var nf *chronicle.NotFoundError
				if !errors.As(err, &nf) {
					t.Fatalf("Get error = %v; want a *NotFoundError", err)
				}
				if nf.Kind != employee || nf.EntityID != "e1" {
					t.Fatalf("*NotFoundError = %+v; want kind %s entity e1", nf, employee)
				}
			})
		})

		t.Run("when the store is queried", func(t *testing.T) {
			recs, cursor, err := s.Query(ctx, chronicle.Query{})
			t.Run("then nothing is returned and no cursor is offered", func(t *testing.T) {
				if err != nil {
					t.Fatalf("Query failed: %v", err)
				}
				if len(recs) != 0 || !cursor.IsZero() {
					t.Fatalf("Query = %d records, cursor %q; want none of either", len(recs), cursor)
				}
			})
		})

		t.Run("when an empty write is applied", func(t *testing.T) {
			t.Run("then it succeeds and changes nothing", func(t *testing.T) {
				if _, err := s.Apply(ctx, chronicle.ApplyRequest{TxAt: feb, Plan: chronicle.StaticWrite(chronicle.Write{})}); err != nil {
					t.Fatalf("Apply of an empty write = %v; want nil", err)
				}
				recs, _, err := s.Query(ctx, chronicle.Query{})
				if err != nil {
					t.Fatalf("Query failed: %v", err)
				}
				if len(recs) != 0 {
					t.Fatalf("Query returned %d records after an empty write; want 0", len(recs))
				}
			})
		})
	})

	t.Run("given a store holding one fully populated record", func(t *testing.T) {
		ctx := context.Background()
		s := newStore(t)
		want := chronicle.Record{
			ID:        "r-full",
			Kind:      employee,
			EntityID:  "e1",
			Data:      []byte(`{"salary":50000}`),
			ValidFrom: feb,
			ValidTo:   apr,
			Actor:     alice,
			Reason:    "annual review",
			Intent:    chronicle.IntentCorrection,
			Meta:      map[string]string{"ticket": "HR-1", "source": "workday"},
		}
		tx := apply(t, s, chronicle.ApplyRequest{TxAt: jan, Plan: chronicle.StaticWrite(chronicle.Write{Insert: []chronicle.Record{want}})})
		want.TxFrom = tx

		t.Run("when it is read back", func(t *testing.T) {
			got := mustGet(t, s, chronicle.GetQuery{Kind: employee, EntityID: "e1", ValidAt: mar, TxAt: tx})
			t.Run("then every field survived the round trip", func(t *testing.T) {
				assertRecordEqual(t, *got, want)
			})
			t.Run("then it is the current belief", func(t *testing.T) {
				if !got.IsCurrent() {
					t.Fatalf("TxTo = %s; a record nothing has superseded must be current", got.TxTo)
				}
			})
		})

		t.Run("when it is looked up outside its valid interval", func(t *testing.T) {
			t.Run("then the lower bound is inclusive", func(t *testing.T) {
				if _, err := s.Get(ctx, chronicle.GetQuery{Kind: employee, EntityID: "e1", ValidAt: feb, TxAt: tx}); err != nil {
					t.Fatalf("Get at ValidFrom = %v; want the record, the lower bound is inclusive", err)
				}
			})
			t.Run("then the upper bound is exclusive", func(t *testing.T) {
				_, err := s.Get(ctx, chronicle.GetQuery{Kind: employee, EntityID: "e1", ValidAt: apr, TxAt: tx})
				if !errors.Is(err, chronicle.ErrNotFound) {
					t.Fatalf("Get at ValidTo = %v; want ErrNotFound, the upper bound is exclusive", err)
				}
			})
			t.Run("then an instant before it is not covered", func(t *testing.T) {
				_, err := s.Get(ctx, chronicle.GetQuery{Kind: employee, EntityID: "e1", ValidAt: jan, TxAt: tx})
				if !errors.Is(err, chronicle.ErrNotFound) {
					t.Fatalf("Get before ValidFrom = %v; want ErrNotFound", err)
				}
			})
		})

		t.Run("when it is looked up before it was known", func(t *testing.T) {
			t.Run("then the transaction axis hides it", func(t *testing.T) {
				_, err := s.Get(ctx, chronicle.GetQuery{
					Kind: employee, EntityID: "e1", ValidAt: mar, TxAt: tx.Add(-time.Second),
				})
				if !errors.Is(err, chronicle.ErrNotFound) {
					t.Fatalf("Get before TxFrom = %v; want ErrNotFound", err)
				}
			})
		})

		t.Run("when a different entity or kind is looked up at the same point", func(t *testing.T) {
			t.Run("then entity IDs do not leak between entities", func(t *testing.T) {
				_, err := s.Get(ctx, chronicle.GetQuery{Kind: employee, EntityID: "e2", ValidAt: mar, TxAt: tx})
				if !errors.Is(err, chronicle.ErrNotFound) {
					t.Fatalf("Get of another entity = %v; want ErrNotFound", err)
				}
			})
			t.Run("then entity IDs do not leak between kinds", func(t *testing.T) {
				_, err := s.Get(ctx, chronicle.GetQuery{Kind: invoice, EntityID: "e1", ValidAt: mar, TxAt: tx})
				if !errors.Is(err, chronicle.ErrNotFound) {
					t.Fatalf("Get under another kind = %v; want ErrNotFound", err)
				}
			})
		})

		t.Run("when the caller mutates what it read", func(t *testing.T) {
			got := mustGet(t, s, chronicle.GetQuery{Kind: employee, EntityID: "e1", ValidAt: mar, TxAt: tx})
			got.Data[0] = 'X'
			if got.Meta != nil {
				got.Meta["ticket"] = "tampered"
			}
			t.Run("then the store is unaffected", func(t *testing.T) {
				again := mustGet(t, s, chronicle.GetQuery{Kind: employee, EntityID: "e1", ValidAt: mar, TxAt: tx})
				if string(again.Data) != `{"salary":50000}` || again.Meta["ticket"] != "HR-1" {
					t.Fatal("a record handed to a caller shares mutable state with the store")
				}
			})
		})
	})

	t.Run("given a record with unbounded valid ends", func(t *testing.T) {
		s := newStore(t)
		tx := apply(t, s, chronicle.ApplyRequest{TxAt: jan, Plan: chronicle.StaticWrite(chronicle.Write{Insert: []chronicle.Record{{
			ID: "r-open", Kind: employee, EntityID: "e1", Data: []byte("v"), Actor: alice,
		}}})})

		t.Run("when it is read back", func(t *testing.T) {
			got := mustGet(t, s, chronicle.GetQuery{Kind: employee, EntityID: "e1", ValidAt: mar, TxAt: tx})
			t.Run("then unbounded ends round-trip as the zero time", func(t *testing.T) {
				if !got.ValidFrom.IsZero() || !got.ValidTo.IsZero() {
					t.Fatalf("valid interval = %s; want unbounded at both ends as zero times", got.Valid())
				}
			})
			t.Run("then it covers every instant on the valid axis", func(t *testing.T) {
				for _, at := range []time.Time{jan.AddDate(-100, 0, 0), mar, jun.AddDate(100, 0, 0)} {
					if _, err := s.Get(context.Background(), chronicle.GetQuery{
						Kind: employee, EntityID: "e1", ValidAt: at, TxAt: tx,
					}); err != nil {
						t.Fatalf("Get at %s = %v; an unbounded record covers all of valid time", at, err)
					}
				}
			})
			t.Run("then nil metadata stays nil rather than becoming an empty map", func(t *testing.T) {
				if len(got.Meta) != 0 {
					t.Fatalf("Meta = %v; want nil or empty", got.Meta)
				}
			})
		})
	})

	t.Run("given a record that is then superseded", func(t *testing.T) {
		s := newStore(t)
		first := chronicle.Record{
			ID: "r1", Kind: employee, EntityID: "e1", Data: []byte("v1"),
			ValidFrom: feb, ValidTo: apr, Actor: alice,
		}
		tx1 := apply(t, s, chronicle.ApplyRequest{TxAt: jan, Plan: chronicle.StaticWrite(chronicle.Write{Insert: []chronicle.Record{first}})})
		tx2 := apply(t, s, chronicle.ApplyRequest{TxAt: tx1.Add(time.Second), Plan: chronicle.StaticWrite(chronicle.Write{
			Supersede: []chronicle.RecordID{"r1"},
			Insert: []chronicle.Record{{
				ID: "r2", Kind: employee, EntityID: "e1", Data: []byte("v2"),
				ValidFrom: feb, ValidTo: apr, Actor: bob, Intent: chronicle.IntentCorrection,
			}}})})

		t.Run("when the transaction axis is walked", func(t *testing.T) {
			t.Run("then the two instants are distinct and ordered", func(t *testing.T) {
				if !tx2.After(tx1) {
					t.Fatalf("second Apply returned %s, not after the first's %s — a superseded "+
						"record whose transaction interval is empty is invisible to every as-of query",
						tx2, tx1)
				}
			})
			t.Run("then the old belief is still readable at the old instant", func(t *testing.T) {
				got := mustGet(t, s, chronicle.GetQuery{Kind: employee, EntityID: "e1", ValidAt: mar, TxAt: tx1})
				if string(got.Data) != "v1" {
					t.Fatalf("data at the first instant = %s; want v1 — nothing is ever destroyed", got.Data)
				}
			})
			t.Run("then the new belief is what the latest instant shows", func(t *testing.T) {
				got := mustGet(t, s, chronicle.GetQuery{Kind: employee, EntityID: "e1", ValidAt: mar, TxAt: tx2})
				if string(got.Data) != "v2" {
					t.Fatalf("data at the second instant = %s; want v2", got.Data)
				}
			})
			t.Run("then the superseded record's interval closes exactly where the new one opens", func(t *testing.T) {
				old := byID(t, s, "r1")
				if old.TxTo.IsZero() {
					t.Fatal("the superseded record is still current")
				}
				if !old.TxTo.Equal(tx2) {
					t.Fatalf("TxTo = %s; want %s — a gap or an overlap on the transaction axis "+
						"makes an as-of query return nothing or two things", old.TxTo, tx2)
				}
				if !old.TxFrom.Equal(tx1) {
					t.Fatalf("TxFrom = %s; want %s — transaction time is never rewritten", old.TxFrom, tx1)
				}
			})
			t.Run("then exactly one record is current", func(t *testing.T) {
				recs := queryAll(t, s, chronicle.Query{Kind: employee, EntityID: "e1", CurrentOnly: true})
				if len(recs) != 1 || recs[0].ID != "r2" {
					t.Fatalf("current records = %v; want just r2", ids(recs))
				}
			})
		})
	})

	t.Run("given a supersession with nothing to insert", func(t *testing.T) {
		s := newStore(t)
		tx1 := apply(t, s, chronicle.ApplyRequest{TxAt: jan, Plan: chronicle.StaticWrite(chronicle.Write{Insert: []chronicle.Record{{
			ID: "r1", Kind: employee, EntityID: "e1", Data: []byte("v"), ValidFrom: feb, Actor: alice,
		}}})})
		closed := apply(t, s, chronicle.ApplyRequest{TxAt: tx1.Add(time.Second), Plan: chronicle.StaticWrite(chronicle.Write{Supersede: []chronicle.RecordID{"r1"}})})

		t.Run("when it is repeated", func(t *testing.T) {
			t.Run("then it is idempotent", func(t *testing.T) {
				if _, err := s.Apply(context.Background(), chronicle.ApplyRequest{TxAt: closed.Add(time.Hour), Plan: chronicle.StaticWrite(chronicle.Write{
					Supersede: []chronicle.RecordID{"r1"}})}); err != nil {
					t.Fatalf("repeated supersession = %v; want nil", err)
				}
				if got := byID(t, s, "r1").TxTo; !got.Equal(closed) {
					t.Fatalf("TxTo = %s; want the original %s — a retry must not rewrite an "+
						"assigned transaction timestamp", got, closed)
				}
			})
		})

		t.Run("when it names a record that does not exist", func(t *testing.T) {
			t.Run("then it is not an error", func(t *testing.T) {
				if _, err := s.Apply(context.Background(), chronicle.ApplyRequest{TxAt: closed.Add(time.Hour), Plan: chronicle.StaticWrite(chronicle.Write{
					Supersede: []chronicle.RecordID{"no-such-record"}})}); err != nil {
					t.Fatalf("supersession of an unknown ID = %v; want nil", err)
				}
			})
		})
	})

	t.Run("given a split planned against a record someone else already closed", func(t *testing.T) {
		s := newStore(t)
		tx1 := apply(t, s, chronicle.ApplyRequest{TxAt: jan, Plan: chronicle.StaticWrite(chronicle.Write{Insert: []chronicle.Record{{
			ID: "r1", Kind: employee, EntityID: "e1", Data: []byte("v1"), ValidFrom: feb, ValidTo: apr, Actor: alice,
		}}})})
		apply(t, s, chronicle.ApplyRequest{TxAt: tx1.Add(time.Second), Plan: chronicle.StaticWrite(chronicle.Write{
			Supersede: []chronicle.RecordID{"r1"},
			Insert: []chronicle.Record{{
				ID: "r2", Kind: employee, EntityID: "e1", Data: []byte("v2"), ValidFrom: feb, ValidTo: apr, Actor: bob,
			}}})})

		t.Run("when the stale half of the split is applied", func(t *testing.T) {
			_, err := s.Apply(context.Background(), chronicle.ApplyRequest{TxAt: tx1.Add(2 * time.Second), Plan: chronicle.StaticWrite(chronicle.Write{
				Supersede: []chronicle.RecordID{"r1"},
				Insert: []chronicle.Record{{
					ID: "r3", Kind: employee, EntityID: "e1", Data: []byte("v3"), ValidFrom: feb, ValidTo: apr, Actor: alice,
				}}})})
			t.Run("then the store reports a conflict", func(t *testing.T) {
				if !errors.Is(err, chronicle.ErrConflict) {
					t.Fatalf("Apply = %v; want ErrConflict — applying half a split against a "+
						"pre-state that has moved leaves the entity's timeline overlapping", err)
				}
			})
			t.Run("then nothing was inserted", func(t *testing.T) {
				if recs := queryAll(t, s, chronicle.Query{Kind: employee, EntityID: "e1"}); len(recs) != 2 {
					t.Fatalf("records = %v; want just r1 and r2 — a conflicting write applies nothing", ids(recs))
				}
			})
			t.Run("then the entity still has exactly one current record", func(t *testing.T) {
				assertNoOverlap(t, s)
			})
		})
	})

	t.Run("given a record inserted twice under one ID", func(t *testing.T) {
		s := newStore(t)
		rec := chronicle.Record{
			ID: "r1", Kind: employee, EntityID: "e1", Data: []byte("original"), ValidFrom: feb, Actor: alice,
		}
		tx := apply(t, s, chronicle.ApplyRequest{TxAt: jan, Plan: chronicle.StaticWrite(chronicle.Write{Insert: []chronicle.Record{rec}})})

		t.Run("when the second insertion arrives", func(t *testing.T) {
			dup := rec
			dup.Data = []byte("overwritten")
			if _, err := s.Apply(context.Background(), chronicle.ApplyRequest{TxAt: tx, Plan: chronicle.StaticWrite(chronicle.Write{Insert: []chronicle.Record{dup}})}); err != nil {
				t.Fatalf("re-inserting an existing ID = %v; want nil", err)
			}
			t.Run("then the original is kept, because a log is append-only", func(t *testing.T) {
				if got := byID(t, s, "r1"); string(got.Data) != "original" {
					t.Fatalf("data = %s; want the original", got.Data)
				}
			})
			t.Run("then no second row appeared", func(t *testing.T) {
				if recs := queryAll(t, s, chronicle.Query{}); len(recs) != 1 {
					t.Fatalf("records = %v; want one", ids(recs))
				}
			})
		})
	})

	runQueryFilters(t, newStore)
	runPagination(t, newStore)
	runQueryValidation(t, newStore)
}

// ---------------------------------------------------------------------------
// query filters
// ---------------------------------------------------------------------------

// filterFixture seeds a small log spanning two kinds, two actors, three intents
// and both open and closed transaction intervals, and reports the transaction
// instants each generation landed at.
type filterFixture struct {
	store    chronicle.Store
	tx1, tx2 time.Time
}

func seedFilterFixture(t *testing.T, newStore Factory) filterFixture {
	t.Helper()
	s := newStore(t)

	// Generation one: three records, one per entity.
	tx1 := apply(t, s, chronicle.ApplyRequest{TxAt: jan, Plan: chronicle.StaticWrite(chronicle.Write{Insert: []chronicle.Record{
		{ID: "a", Kind: employee, EntityID: "e1", Data: []byte("a"), ValidFrom: feb, ValidTo: apr, Actor: alice, Intent: chronicle.IntentAssert},
		{ID: "b", Kind: employee, EntityID: "e2", Data: []byte("b"), ValidFrom: mar, ValidTo: may, Actor: bob, Intent: chronicle.IntentCorrection},
		{ID: "c", Kind: invoice, EntityID: "i1", Data: []byte("c"), ValidFrom: apr, Actor: alice, Intent: chronicle.IntentRemainder},
	}})})

	// Generation two closes the invoice and replaces it, so the fixture has a
	// record with a closed transaction interval to filter on.
	tx2 := apply(t, s, chronicle.ApplyRequest{TxAt: tx1.Add(time.Second), Plan: chronicle.StaticWrite(chronicle.Write{
		Supersede: []chronicle.RecordID{"c"},
		Insert: []chronicle.Record{
			{ID: "d", Kind: invoice, EntityID: "i1", Data: []byte("d"), ValidFrom: apr, Actor: bob, Intent: chronicle.IntentAssert},
		}})})
	return filterFixture{store: s, tx1: tx1, tx2: tx2}
}

func runQueryFilters(t *testing.T, newStore Factory) {
	t.Run("given a log spanning several kinds, actors and intents", func(t *testing.T) {
		f := seedFilterFixture(t, newStore)
		before := f.tx1.Add(-time.Second)

		cases := []struct {
			name string
			q    chronicle.Query
			want []chronicle.RecordID
		}{
			{"unfiltered", chronicle.Query{}, []chronicle.RecordID{"a", "b", "c", "d"}},
			{"by kind", chronicle.Query{Kind: employee}, []chronicle.RecordID{"a", "b"}},
			{"by kind and entity", chronicle.Query{Kind: employee, EntityID: "e2"}, []chronicle.RecordID{"b"}},
			{"by actor", chronicle.Query{ActorID: alice.ID}, []chronicle.RecordID{"a", "c"}},
			{"by actor and kind together", chronicle.Query{ActorID: bob.ID, Kind: invoice}, []chronicle.RecordID{"d"}},
			{"by intent assert", chronicle.Query{Intent: chronicle.IntentAssert, HasIntent: true}, []chronicle.RecordID{"a", "d"}},
			{"by intent correction", chronicle.Query{Intent: chronicle.IntentCorrection, HasIntent: true}, []chronicle.RecordID{"b"}},
			{"by intent remainder", chronicle.Query{Intent: chronicle.IntentRemainder, HasIntent: true}, []chronicle.RecordID{"c"}},
			{"current only", chronicle.Query{CurrentOnly: true}, []chronicle.RecordID{"a", "b", "d"}},
			{"by valid instant", chronicle.Query{ValidAt: mar}, []chronicle.RecordID{"a", "b"}},
			{"by valid instant on an exclusive upper bound", chronicle.Query{ValidAt: apr}, []chronicle.RecordID{"b", "c", "d"}},
			{"by valid range", chronicle.Query{Valid: chronicle.Between(apr, may)}, []chronicle.RecordID{"b", "c", "d"}},
			{"by open-ended valid range", chronicle.Query{Valid: chronicle.Since(may)}, []chronicle.RecordID{"c", "d"}},
			{"by unbounded-start valid range", chronicle.Query{Valid: chronicle.Until(mar)}, []chronicle.RecordID{"a"}},
			{"by transaction instant before anything was known", chronicle.Query{TxAt: before}, nil},
			{"by kind matching nothing", chronicle.Query{Kind: "nonexistent"}, nil},
			{"by entity matching nothing", chronicle.Query{Kind: employee, EntityID: "nope"}, nil},
			{"by actor matching nothing", chronicle.Query{ActorID: "nobody"}, nil},
		}

		for _, tc := range cases {
			t.Run("when filtered "+tc.name, func(t *testing.T) {
				t.Run("then exactly the expected records come back", func(t *testing.T) {
					got := queryAll(t, f.store, tc.q)
					assertIDs(t, got, tc.want)
				})
			})
		}

		// The transaction-axis filters are written separately because their
		// operands are instants the store chose rather than the suite.
		t.Run("when filtered by the first transaction instant", func(t *testing.T) {
			t.Run("then only the first generation is visible", func(t *testing.T) {
				assertIDs(t, queryAll(t, f.store, chronicle.Query{TxAt: f.tx1}), []chronicle.RecordID{"a", "b", "c"})
			})
		})
		t.Run("when filtered by the second transaction instant", func(t *testing.T) {
			t.Run("then the replaced record has dropped out and its replacement is in", func(t *testing.T) {
				assertIDs(t, queryAll(t, f.store, chronicle.Query{TxAt: f.tx2}), []chronicle.RecordID{"a", "b", "d"})
			})
		})
		t.Run("when filtered by a transaction range covering only the first generation", func(t *testing.T) {
			t.Run("then the second generation is excluded", func(t *testing.T) {
				assertIDs(t, queryAll(t, f.store, chronicle.Query{Tx: chronicle.Between(before, f.tx2)}),
					[]chronicle.RecordID{"a", "b", "c"})
			})
		})
		t.Run("when filtered by a transaction range starting at the second generation", func(t *testing.T) {
			t.Run("then the record closed at that instant is excluded", func(t *testing.T) {
				assertIDs(t, queryAll(t, f.store, chronicle.Query{Tx: chronicle.Since(f.tx2)}),
					[]chronicle.RecordID{"a", "b", "d"})
			})
		})
		t.Run("when filtered on both axes at once", func(t *testing.T) {
			t.Run("then the filters intersect rather than accumulate", func(t *testing.T) {
				assertIDs(t, queryAll(t, f.store, chronicle.Query{
					Kind: employee, ValidAt: mar, TxAt: f.tx2, ActorID: alice.ID,
				}), []chronicle.RecordID{"a"})
			})
		})
	})
}

// ---------------------------------------------------------------------------
// ordering and keyset pagination
// ---------------------------------------------------------------------------

func runPagination(t *testing.T, newStore Factory) {
	t.Run("given many records, most of them sharing a transaction instant", func(t *testing.T) {
		s := newStore(t)

		// Twelve records land in one Apply and so share a transaction instant.
		// That is the hard case for pagination: the leading sort key ties for
		// every row and only the valid start and the record ID separate them.
		//
		// One entity each, because these are all current records and a store
		// is entitled — required, in the Postgres adapter's case — to refuse
		// two current records covering the same valid instant for one entity.
		// Only the valid starts repeat, which is what the tie needs.
		var batch []chronicle.Record
		for i := 0; i < 12; i++ {
			batch = append(batch, chronicle.Record{
				ID:        chronicle.RecordID(fmt.Sprintf("g1-%02d", i)),
				Kind:      employee,
				EntityID:  fmt.Sprintf("e%02d", i),
				Data:      []byte("v"),
				ValidFrom: feb.AddDate(0, i%3, 0),
				Actor:     alice,
			})
		}
		tx1 := apply(t, s, chronicle.ApplyRequest{TxAt: jan, Plan: chronicle.StaticWrite(chronicle.Write{Insert: batch})})

		// A second generation at a later instant, so the leading key varies too.
		var second []chronicle.Record
		for i := 0; i < 5; i++ {
			second = append(second, chronicle.Record{
				ID:        chronicle.RecordID(fmt.Sprintf("g2-%02d", i)),
				Kind:      invoice,
				EntityID:  fmt.Sprintf("i%d", i),
				Data:      []byte("v"),
				ValidFrom: mar,
				Actor:     bob,
			})
		}
		apply(t, s, chronicle.ApplyRequest{TxAt: tx1.Add(time.Second), Plan: chronicle.StaticWrite(chronicle.Write{Insert: second})})

		for _, desc := range []bool{false, true} {
			dir := "ascending"
			if desc {
				dir = "descending"
			}

			t.Run("when scanned "+dir+" without a limit", func(t *testing.T) {
				full := queryAll(t, s, chronicle.Query{Descending: desc})
				t.Run("then every record is returned once", func(t *testing.T) {
					if len(full) != 17 {
						t.Fatalf("got %d records; want 17", len(full))
					}
				})
				t.Run("then the order is total and strictly monotonic", func(t *testing.T) {
					assertOrdered(t, full, desc)
				})

				for _, limit := range []int{1, 2, 5, 16, 17, 18} {
					t.Run(fmt.Sprintf("then paging at limit %d reproduces it exactly", limit), func(t *testing.T) {
						paged := drainPages(t, s, chronicle.Query{Descending: desc, Limit: limit}, limit)
						assertIDs(t, paged, ids(full))
					})
				}
			})

			t.Run("when a filtered scan is paged "+dir, func(t *testing.T) {
				q := chronicle.Query{Kind: employee, Descending: desc}
				full := queryAll(t, s, q)
				t.Run("then the filter survives cursor resumption", func(t *testing.T) {
					q.Limit = 3
					assertIDs(t, drainPages(t, s, q, 3), ids(full))
				})
			})
		}

		t.Run("when a page is requested that exactly exhausts the result set", func(t *testing.T) {
			t.Run("then no cursor is offered, so callers need no trailing empty page", func(t *testing.T) {
				_, cursor, err := s.Query(context.Background(), chronicle.Query{Limit: 17})
				if err != nil {
					t.Fatalf("Query failed: %v", err)
				}
				if !cursor.IsZero() {
					t.Fatalf("cursor = %q; want empty when nothing was withheld", cursor)
				}
			})
		})

		t.Run("when a cursor from an ascending scan is reused", func(t *testing.T) {
			page, cursor, err := s.Query(context.Background(), chronicle.Query{Limit: 4})
			if err != nil {
				t.Fatalf("Query failed: %v", err)
			}
			t.Run("then it resumes strictly after the last record of the page", func(t *testing.T) {
				next, _, err := s.Query(context.Background(), chronicle.Query{Limit: 4, After: cursor})
				if err != nil {
					t.Fatalf("Query failed: %v", err)
				}
				if len(next) == 0 {
					t.Fatal("resuming from a cursor returned nothing")
				}
				if next[0].ID == page[len(page)-1].ID {
					t.Fatal("resuming from a cursor repeated the last record of the previous page")
				}
			})
		})
	})
}

// ---------------------------------------------------------------------------
// query validation
// ---------------------------------------------------------------------------

func runQueryValidation(t *testing.T, newStore Factory) {
	t.Run("given a malformed query", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		t.Run("when its valid range is inverted", func(t *testing.T) {
			t.Run("then it is rejected before any rows are read", func(t *testing.T) {
				_, _, err := s.Query(ctx, chronicle.Query{Valid: chronicle.Between(apr, feb)})
				if !errors.Is(err, chronicle.ErrInvalidInterval) {
					t.Fatalf("Query = %v; want ErrInvalidInterval", err)
				}
				var ie *chronicle.IntervalError
				if !errors.As(err, &ie) || ie.Field != "valid" {
					t.Fatalf("error = %v; want an *IntervalError naming the valid axis", err)
				}
			})
		})

		t.Run("when its transaction range is inverted", func(t *testing.T) {
			t.Run("then it is rejected and names the transaction axis", func(t *testing.T) {
				_, _, err := s.Query(ctx, chronicle.Query{Tx: chronicle.Between(apr, feb)})
				var ie *chronicle.IntervalError
				if !errors.As(err, &ie) || ie.Field != "transaction" {
					t.Fatalf("error = %v; want an *IntervalError naming the transaction axis", err)
				}
			})
		})

		t.Run("when its intent is not one chronicle defines", func(t *testing.T) {
			t.Run("then it is rejected rather than matching nothing", func(t *testing.T) {
				_, _, err := s.Query(ctx, chronicle.Query{Intent: chronicle.Intent(200), HasIntent: true})
				if err == nil {
					t.Fatal("Query with an undefined intent = nil; want an error")
				}
			})
		})

		t.Run("when its cursor is not one this library minted", func(t *testing.T) {
			for _, bad := range []chronicle.Cursor{"!!!not base64!!!", "YWJj", "Y29ycnVwdA"} {
				t.Run("then "+string(bad)+" is rejected as an invalid cursor", func(t *testing.T) {
					_, _, err := s.Query(ctx, chronicle.Query{After: bad})
					if !errors.Is(err, chronicle.ErrInvalidCursor) {
						t.Fatalf("Query with cursor %q = %v; want ErrInvalidCursor", bad, err)
					}
				})
			}
		})
	})

	t.Run("given a cancelled context", func(t *testing.T) {
		s := newStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		t.Run("when the store is used", func(t *testing.T) {
			t.Run("then Apply reports the context error", func(t *testing.T) {
				if _, err := s.Apply(ctx, chronicle.ApplyRequest{Plan: chronicle.StaticWrite(chronicle.Write{})}); !errors.Is(err, context.Canceled) {
					t.Fatalf("Apply = %v; want context.Canceled", err)
				}
			})
			t.Run("then Get reports the context error", func(t *testing.T) {
				_, err := s.Get(ctx, chronicle.GetQuery{Kind: employee, EntityID: "e1"})
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Get = %v; want context.Canceled", err)
				}
			})
			t.Run("then Query reports the context error", func(t *testing.T) {
				if _, _, err := s.Query(ctx, chronicle.Query{}); !errors.Is(err, context.Canceled) {
					t.Fatalf("Query = %v; want context.Canceled", err)
				}
			})
		})
	})
}

// ---------------------------------------------------------------------------
// log-level contract
// ---------------------------------------------------------------------------

func runLog(t *testing.T, newStore Factory) {
	t.Run("given a log over the store", func(t *testing.T) {
		ctx := context.Background()

		t.Run("when a fact is asserted and then contradicted over part of its range", func(t *testing.T) {
			s := newStore(t)
			l := chronicle.NewLog(s)

			if _, err := l.Put(ctx, employee, "e1", []byte(`{"grade":"L3"}`), feb, time.Time{}, alice); err != nil {
				t.Fatalf("Put failed: %v", err)
			}
			second, err := l.Put(ctx, employee, "e1", []byte(`{"grade":"L4"}`), apr, jun, bob)
			if err != nil {
				t.Fatalf("Put failed: %v", err)
			}

			t.Run("then the entity's timeline is tiled without gaps or overlaps", func(t *testing.T) {
				want := []string{
					"[2020-02-01T00:00:00Z, 2020-04-01T00:00:00Z)={\"grade\":\"L3\"}",
					"[2020-04-01T00:00:00Z, 2020-06-01T00:00:00Z)={\"grade\":\"L4\"}",
					"[2020-06-01T00:00:00Z, ∞)={\"grade\":\"L3\"}",
				}
				got := segments(t, l, employee, "e1", second.TxAt)
				if strings.Join(got, "\n") != strings.Join(want, "\n") {
					t.Fatalf("timeline:\n got %v\nwant %v", got, want)
				}
			})
			t.Run("then the remainders are attributed to the original author", func(t *testing.T) {
				recs, err := l.History(ctx, employee, "e1", chronicle.WithIntent(chronicle.IntentRemainder))
				if err != nil {
					t.Fatalf("History failed: %v", err)
				}
				if len(recs) == 0 {
					t.Fatal("splitting an interval produced no remainder records")
				}
				for _, r := range recs {
					if r.Actor.ID != alice.ID {
						t.Fatalf("remainder %s is attributed to %s; want the superseded record's "+
							"author %s — a log must not claim someone asserted data they never sent",
							r.ID, r.Actor.ID, alice.ID)
					}
				}
			})
			t.Run("then the store's non-overlap invariant holds", func(t *testing.T) {
				assertNoOverlap(t, s)
			})
		})

		t.Run("when a belief is corrected retroactively", func(t *testing.T) {
			s := newStore(t)
			l := chronicle.NewLog(s)

			first, err := l.Put(ctx, employee, "e1", []byte(`{"salary":50000}`), feb, time.Time{}, alice)
			if err != nil {
				t.Fatalf("Put failed: %v", err)
			}
			corrected, err := l.Correct(ctx, employee, "e1", []byte(`{"salary":55000}`), feb, time.Time{}, bob,
				chronicle.WithReason("payroll error"))
			if err != nil {
				t.Fatalf("Correct failed: %v", err)
			}

			t.Run("then what we believe now is the correction", func(t *testing.T) {
				got, err := l.Get(ctx, employee, "e1", chronicle.As{ValidAt: mar, TxAt: corrected.TxAt})
				if err != nil {
					t.Fatalf("Get failed: %v", err)
				}
				if string(got.Data) != `{"salary":55000}` {
					t.Fatalf("current belief = %s; want the correction", got.Data)
				}
			})
			t.Run("then what we believed before the correction is unchanged", func(t *testing.T) {
				got, err := l.Get(ctx, employee, "e1", chronicle.As{ValidAt: mar, TxAt: first.TxAt})
				if err != nil {
					t.Fatalf("Get failed: %v", err)
				}
				if string(got.Data) != `{"salary":50000}` {
					t.Fatalf("prior belief = %s; want the original — this is the question "+
						"uni-temporal systems answer wrongly", got.Data)
				}
			})
			t.Run("then the correction is auditable as one", func(t *testing.T) {
				recs, err := l.History(ctx, employee, "e1", chronicle.WithIntent(chronicle.IntentCorrection))
				if err != nil {
					t.Fatalf("History failed: %v", err)
				}
				if len(recs) != 1 || recs[0].Reason != "payroll error" || recs[0].Actor.ID != bob.ID {
					t.Fatalf("corrections = %+v; want one, by %s, reasoned", recs, bob.ID)
				}
			})
			t.Run("then the change is visible field by field", func(t *testing.T) {
				delta, err := l.Diff(ctx, employee, "e1",
					chronicle.As{ValidAt: mar, TxAt: first.TxAt},
					chronicle.As{ValidAt: mar, TxAt: corrected.TxAt})
				if err != nil {
					t.Fatalf("Diff failed: %v", err)
				}
				if len(delta.Changes) != 1 || delta.Changes[0].Path != "/salary" {
					t.Fatalf("changes = %+v; want one at /salary", delta.Changes)
				}
			})
		})

		t.Run("when an entity accumulates many overlapping writes", func(t *testing.T) {
			s := newStore(t)
			l := chronicle.NewLog(s)
			for i := 0; i < 12; i++ {
				from := feb.AddDate(0, i%5, 0)
				to := from.AddDate(0, 2, 0)
				if _, err := l.Put(ctx, employee, "e1", []byte(fmt.Sprintf(`{"n":%d}`, i)), from, to, alice); err != nil {
					t.Fatalf("Put %d failed: %v", i, err)
				}
			}
			t.Run("then no two current records ever cover the same instant", func(t *testing.T) {
				assertNoOverlap(t, s)
			})
			t.Run("then history is dense on the transaction axis", func(t *testing.T) {
				recs, err := l.History(ctx, employee, "e1")
				if err != nil {
					t.Fatalf("History failed: %v", err)
				}
				for _, r := range recs {
					if !r.IsCurrent() && !r.TxTo.After(r.TxFrom) {
						t.Fatalf("record %s has an empty transaction interval [%s, %s) and so is "+
							"invisible to every as-of query", r.ID, r.TxFrom, r.TxTo)
					}
				}
			})
		})

		t.Run("when an entity has no history at all", func(t *testing.T) {
			s := newStore(t)
			l := chronicle.NewLog(s)
			t.Run("then Get reports not found", func(t *testing.T) {
				if _, err := l.Get(ctx, employee, "ghost", chronicle.Now()); !errors.Is(err, chronicle.ErrNotFound) {
					t.Fatalf("Get = %v; want ErrNotFound", err)
				}
			})
			t.Run("then History reports emptiness rather than failure", func(t *testing.T) {
				recs, err := l.History(ctx, employee, "ghost")
				if err != nil {
					t.Fatalf("History = %v; an entity with no history has an empty history", err)
				}
				if len(recs) != 0 {
					t.Fatalf("History = %v; want nothing", ids(recs))
				}
			})
		})
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// apply runs a write and returns the transaction instant the store assigned,
// which is the only transaction timestamp the suite is entitled to assume.
func apply(t *testing.T, s chronicle.Store, req chronicle.ApplyRequest) time.Time {
	t.Helper()
	tx, err := s.Apply(context.Background(), req)
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	if tx.IsZero() {
		t.Fatal("Apply returned the zero time; a store must report the transaction instant it assigned")
	}
	return tx.UTC()
}

func mustGet(t *testing.T, s chronicle.Store, q chronicle.GetQuery) *chronicle.Record {
	t.Helper()
	rec, err := s.Get(context.Background(), q)
	if err != nil {
		t.Fatalf("Get(%+v) failed: %v", q, err)
	}
	return rec
}

// byID finds one record by ID through the query surface, since [chronicle.Store]
// has no lookup by ID.
func byID(t *testing.T, s chronicle.Store, id chronicle.RecordID) chronicle.Record {
	t.Helper()
	for _, r := range queryAll(t, s, chronicle.Query{}) {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no record with ID %s", id)
	return chronicle.Record{}
}

func queryAll(t *testing.T, s chronicle.Store, q chronicle.Query) []chronicle.Record {
	t.Helper()
	recs, cursor, err := s.Query(context.Background(), q)
	if err != nil {
		t.Fatalf("Query(%+v) failed: %v", q, err)
	}
	if !cursor.IsZero() {
		t.Fatalf("Query without a limit offered a cursor %q; want none", cursor)
	}
	return recs
}

// drainPages walks every page of a paged query and returns the concatenation,
// checking as it goes that no page exceeds the limit and that the walk
// terminates on an empty cursor rather than an empty page.
func drainPages(t *testing.T, s chronicle.Store, q chronicle.Query, limit int) []chronicle.Record {
	t.Helper()
	var out []chronicle.Record
	seen := make(map[chronicle.RecordID]struct{})
	for page := 0; ; page++ {
		if page > 1000 {
			t.Fatal("pagination did not terminate")
		}
		recs, cursor, err := s.Query(context.Background(), q)
		if err != nil {
			t.Fatalf("Query page %d failed: %v", page, err)
		}
		if len(recs) > limit {
			t.Fatalf("page %d returned %d records; limit was %d", page, len(recs), limit)
		}
		for _, r := range recs {
			if _, dup := seen[r.ID]; dup {
				t.Fatalf("record %s appeared on two pages", r.ID)
			}
			seen[r.ID] = struct{}{}
			out = append(out, r)
		}
		if cursor.IsZero() {
			return out
		}
		if len(recs) == 0 {
			t.Fatalf("page %d was empty but offered a cursor %q", page, cursor)
		}
		q.After = cursor
	}
}

func ids(recs []chronicle.Record) []chronicle.RecordID {
	out := make([]chronicle.RecordID, len(recs))
	for i, r := range recs {
		out[i] = r.ID
	}
	return out
}

func assertIDs(t *testing.T, got []chronicle.Record, want []chronicle.RecordID) {
	t.Helper()
	gotIDs := ids(got)
	if len(gotIDs) != len(want) {
		t.Fatalf("records = %v; want %v", gotIDs, want)
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Fatalf("records = %v; want %v", gotIDs, want)
		}
	}
}

// assertOrdered checks chronicle's total order — transaction start, then valid
// start, then record ID — and that no two records compare equal, which is what
// makes keyset pagination exact.
func assertOrdered(t *testing.T, recs []chronicle.Record, descending bool) {
	t.Helper()
	for i := 1; i < len(recs); i++ {
		prev, cur := recs[i-1], recs[i]
		c := compare(prev, cur)
		if descending {
			c = -c
		}
		if c >= 0 {
			t.Fatalf("records %d and %d are out of order or tied: (%s, %s, %s) then (%s, %s, %s)",
				i-1, i, prev.TxFrom, prev.ValidFrom, prev.ID, cur.TxFrom, cur.ValidFrom, cur.ID)
		}
	}
}

// compare mirrors chronicle's unexported total order. The duplication is
// deliberate: the suite is checking that a store implements that order, so
// borrowing the implementation under test would make the check vacuous.
func compare(a, b chronicle.Record) int {
	if c := a.TxFrom.Compare(b.TxFrom); c != 0 {
		return c
	}
	switch {
	case a.ValidFrom.IsZero() && !b.ValidFrom.IsZero():
		return -1
	case !a.ValidFrom.IsZero() && b.ValidFrom.IsZero():
		return 1
	default:
		if c := a.ValidFrom.Compare(b.ValidFrom); c != 0 {
			return c
		}
	}
	return strings.Compare(string(a.ID), string(b.ID))
}

// assertNoOverlap checks the library's headline invariant directly against the
// store: no two current records for one entity may cover the same valid
// instant.
func assertNoOverlap(t *testing.T, s chronicle.Store) {
	t.Helper()
	byEntity := make(map[chronicle.EntityRef][]chronicle.Record)
	for _, r := range queryAll(t, s, chronicle.Query{CurrentOnly: true}) {
		ref := chronicle.EntityRef{Kind: r.Kind, EntityID: r.EntityID}
		byEntity[ref] = append(byEntity[ref], r)
	}
	for ref, recs := range byEntity {
		for i := range recs {
			for j := i + 1; j < len(recs); j++ {
				if recs[i].Valid().Overlaps(recs[j].Valid()) {
					t.Fatalf("%s/%s has two current records covering the same valid instant: "+
						"%s %s and %s %s", ref.Kind, ref.EntityID,
						recs[i].ID, recs[i].Valid(), recs[j].ID, recs[j].Valid())
				}
			}
		}
	}
}

func assertRecordEqual(t *testing.T, got, want chronicle.Record) {
	t.Helper()
	if got.ID != want.ID || got.Kind != want.Kind || got.EntityID != want.EntityID {
		t.Fatalf("identity: got %s/%s/%s; want %s/%s/%s",
			got.ID, got.Kind, got.EntityID, want.ID, want.Kind, want.EntityID)
	}
	if string(got.Data) != string(want.Data) {
		t.Fatalf("data = %q; want %q", got.Data, want.Data)
	}
	if !got.ValidFrom.Equal(want.ValidFrom) || !got.ValidTo.Equal(want.ValidTo) {
		t.Fatalf("valid interval = %s; want %s", got.Valid(), want.Valid())
	}
	if !got.TxFrom.Equal(want.TxFrom) {
		t.Fatalf("TxFrom = %s; want %s", got.TxFrom, want.TxFrom)
	}
	if got.Actor != want.Actor {
		t.Fatalf("actor = %+v; want %+v", got.Actor, want.Actor)
	}
	if got.Reason != want.Reason {
		t.Fatalf("reason = %q; want %q", got.Reason, want.Reason)
	}
	if got.Intent != want.Intent {
		t.Fatalf("intent = %s; want %s", got.Intent, want.Intent)
	}
	if len(got.Meta) != len(want.Meta) {
		t.Fatalf("meta = %v; want %v", got.Meta, want.Meta)
	}
	for k, v := range want.Meta {
		if got.Meta[k] != v {
			t.Fatalf("meta = %v; want %v", got.Meta, want.Meta)
		}
	}
}

// segments renders an entity's current valid-time tiling at one transaction
// instant, which is the most legible form in which to assert that a write
// split an interval the way it should have.
func segments(t *testing.T, l *chronicle.Log, kind, entityID string, txAt time.Time) []string {
	t.Helper()
	recs, err := l.Timeline(context.Background(), kind, entityID, chronicle.As{TxAt: txAt})
	if err != nil {
		t.Fatalf("Timeline failed: %v", err)
	}
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Valid().String()+"="+string(r.Data))
	}
	return out
}
