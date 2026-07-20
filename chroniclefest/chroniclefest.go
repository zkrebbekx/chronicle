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
//	    chroniclefest.Run(t, func(t chroniclefest.T) chronicle.Store {
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
// A store that cannot preserve valid times to at least whole-second precision
// is out of contract.
//
// This package is a normal package rather than a test file so that stores
// outside this module can use it, which is the whole point.
//
// The suite reports through [T] rather than *testing.T. [Run] takes a
// *testing.T and wraps it, so the common case reads the same; [RunT] takes the
// interface directly, for harnesses that are not a Go test — including this
// module's own tests, which run the suite against deliberately broken stores
// and assert that it fails, and fails on the check that names the fault. A
// conformance suite whose failure branches have never executed is not evidence
// of anything.
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

// T is the slice of *testing.T the suite uses to report. It exists so that the
// suite can be driven by something other than the testing package: a harness
// that records failures instead of aborting, a fuzz driver, a CI tool that
// certifies a third-party store.
//
// Fatal and Fatalf must not return. *testing.T ends the goroutine; an
// implementation that cannot do that should panic and recover at its own Run
// boundary. Everything after a Fatal in the suite assumes it was not reached.
type T interface {
	// Helper marks the caller as a test helper, as [testing.T.Helper] does.
	Helper()
	// Name returns the name of the running test.
	Name() string
	// Cleanup registers a function to run when the test and its subtests
	// finish.
	Cleanup(func())
	// Errorf reports a failure and continues.
	Errorf(format string, args ...any)
	// Fatal reports a failure and does not return.
	Fatal(args ...any)
	// Fatalf reports a formatted failure and does not return.
	Fatalf(format string, args ...any)
	// Run runs f as a subtest, reporting whether it passed. A failure inside f
	// must not abort the caller.
	Run(name string, f func(T)) bool
}

// Wrap adapts a *testing.T to [T]. The only thing needing adaptation is Run,
// whose function parameter has to take the interface rather than the concrete
// type.
func Wrap(t *testing.T) T { return goT{t} }

type goT struct{ *testing.T }

func (t goT) Run(name string, f func(T)) bool {
	return t.T.Run(name, func(sub *testing.T) { f(goT{sub}) })
}

// Factory returns a fresh, empty store for one test. It is called many times
// over a single Run, and each store must be independent of the others.
//
// Register any teardown with t.Cleanup; the suite never closes what it is
// given, since closing is not part of the [chronicle.Store] contract.
type Factory func(t T) chronicle.Store

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

// offGrid is deliberately not on a day, hour or minute boundary. Every other
// constant here is midnight on the first of a month, which means a store that
// rounds valid time to the day — or to the hour, or the minute — would agree
// with all of them by coincidence. This is the one that disagrees.
var offGrid = time.Date(2020, 3, 15, 12, 34, 56, 0, time.UTC)

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
	RunT(Wrap(t), newStore)
}

// RunStore executes only the store-level half of the suite: what Apply, Get and
// Query do, with no bitemporal reasoning layered on top.
func RunStore(t *testing.T, newStore Factory) {
	t.Helper()
	runStore(Wrap(t), newStore)
}

// RunLog executes only the half that drives a [chronicle.Log] over the store,
// checking that the bitemporal engine gets the answers it expects back.
func RunLog(t *testing.T, newStore Factory) {
	t.Helper()
	runLog(Wrap(t), newStore)
}

// RunT is [Run] for a harness that is not a Go test.
func RunT(t T, newStore Factory) {
	t.Helper()
	t.Run("store contract", func(t T) { runStore(t, newStore) })
	t.Run("log contract", func(t T) { runLog(t, newStore) })
}

// RunStoreT is [RunStore] for a harness that is not a Go test.
func RunStoreT(t T, newStore Factory) {
	t.Helper()
	runStore(t, newStore)
}

// RunLogT is [RunLog] for a harness that is not a Go test.
func RunLogT(t T, newStore Factory) {
	t.Helper()
	runLog(t, newStore)
}

// ---------------------------------------------------------------------------
// store-level contract
// ---------------------------------------------------------------------------

func runStore(t T, newStore Factory) {
	t.Run("given an empty store", func(t T) {
		ctx := context.Background()
		s := newStore(t)

		t.Run("when a record is looked up", func(t T) {
			_, err := s.Get(ctx, chronicle.GetQuery{Kind: employee, EntityID: "e1", ValidAt: feb, TxAt: feb})
			t.Run("then the lookup reports not found", func(t T) {
				if !errors.Is(err, chronicle.ErrNotFound) {
					t.Fatalf("Get = %v; want ErrNotFound", err)
				}
			})
			t.Run("then the error carries the coordinates searched", func(t T) {
				var nf *chronicle.NotFoundError
				if !errors.As(err, &nf) {
					t.Fatalf("Get error = %v; want a *NotFoundError", err)
				}
				if nf.Kind != employee || nf.EntityID != "e1" {
					t.Fatalf("*NotFoundError = %+v; want kind %s entity e1", nf, employee)
				}
			})
		})

		t.Run("when the store is queried", func(t T) {
			recs, cursor, err := s.Query(ctx, chronicle.Query{})
			t.Run("then nothing is returned and no cursor is offered", func(t T) {
				if err != nil {
					t.Fatalf("Query failed: %v", err)
				}
				if len(recs) != 0 || !cursor.IsZero() {
					t.Fatalf("Query = %d records, cursor %q; want none of either", len(recs), cursor)
				}
			})
		})

		t.Run("when an empty write is applied", func(t T) {
			t.Run("then it succeeds and changes nothing", func(t T) {
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

	t.Run("given a store holding one fully populated record", func(t T) {
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

		t.Run("when it is read back", func(t T) {
			got := mustGet(t, s, chronicle.GetQuery{Kind: employee, EntityID: "e1", ValidAt: mar, TxAt: tx})
			t.Run("then every field survived the round trip", func(t T) {
				assertRecordEqual(t, *got, want)
			})
			t.Run("then it is the current belief", func(t T) {
				if !got.IsCurrent() {
					t.Fatalf("TxTo = %s; a record nothing has superseded must be current", got.TxTo)
				}
			})
		})

		t.Run("when it is looked up outside its valid interval", func(t T) {
			t.Run("then the lower bound is inclusive", func(t T) {
				if _, err := s.Get(ctx, chronicle.GetQuery{Kind: employee, EntityID: "e1", ValidAt: feb, TxAt: tx}); err != nil {
					t.Fatalf("Get at ValidFrom = %v; want the record, the lower bound is inclusive", err)
				}
			})
			t.Run("then the upper bound is exclusive", func(t T) {
				_, err := s.Get(ctx, chronicle.GetQuery{Kind: employee, EntityID: "e1", ValidAt: apr, TxAt: tx})
				if !errors.Is(err, chronicle.ErrNotFound) {
					t.Fatalf("Get at ValidTo = %v; want ErrNotFound, the upper bound is exclusive", err)
				}
			})
			t.Run("then an instant before it is not covered", func(t T) {
				_, err := s.Get(ctx, chronicle.GetQuery{Kind: employee, EntityID: "e1", ValidAt: jan, TxAt: tx})
				if !errors.Is(err, chronicle.ErrNotFound) {
					t.Fatalf("Get before ValidFrom = %v; want ErrNotFound", err)
				}
			})
		})

		t.Run("when it is looked up before it was known", func(t T) {
			t.Run("then the transaction axis hides it", func(t T) {
				_, err := s.Get(ctx, chronicle.GetQuery{
					Kind: employee, EntityID: "e1", ValidAt: mar, TxAt: tx.Add(-time.Second),
				})
				if !errors.Is(err, chronicle.ErrNotFound) {
					t.Fatalf("Get before TxFrom = %v; want ErrNotFound", err)
				}
			})
		})

		t.Run("when a different entity or kind is looked up at the same point", func(t T) {
			t.Run("then entity IDs do not leak between entities", func(t T) {
				_, err := s.Get(ctx, chronicle.GetQuery{Kind: employee, EntityID: "e2", ValidAt: mar, TxAt: tx})
				if !errors.Is(err, chronicle.ErrNotFound) {
					t.Fatalf("Get of another entity = %v; want ErrNotFound", err)
				}
			})
			t.Run("then entity IDs do not leak between kinds", func(t T) {
				_, err := s.Get(ctx, chronicle.GetQuery{Kind: invoice, EntityID: "e1", ValidAt: mar, TxAt: tx})
				if !errors.Is(err, chronicle.ErrNotFound) {
					t.Fatalf("Get under another kind = %v; want ErrNotFound", err)
				}
			})
		})

		t.Run("when the caller mutates what it read", func(t T) {
			got := mustGet(t, s, chronicle.GetQuery{Kind: employee, EntityID: "e1", ValidAt: mar, TxAt: tx})
			got.Data[0] = 'X'
			if got.Meta != nil {
				got.Meta["ticket"] = "tampered"
			}
			t.Run("then the store is unaffected", func(t T) {
				again := mustGet(t, s, chronicle.GetQuery{Kind: employee, EntityID: "e1", ValidAt: mar, TxAt: tx})
				if string(again.Data) != `{"salary":50000}` || again.Meta["ticket"] != "HR-1" {
					t.Fatal("a record handed to a caller shares mutable state with the store")
				}
			})
		})
	})

	t.Run("given a record whose valid bounds are not on a day boundary", func(t T) {
		ctx := context.Background()
		s := newStore(t)
		from, to := offGrid, offGrid.Add(61*time.Second)
		tx := apply(t, s, chronicle.ApplyRequest{TxAt: jan, Plan: chronicle.StaticWrite(chronicle.Write{Insert: []chronicle.Record{{
			ID: "r-sec", Kind: employee, EntityID: "e1", Data: []byte("v"),
			ValidFrom: from, ValidTo: to, Actor: alice,
		}}})})

		t.Run("when it is read back", func(t T) {
			t.Run("then both bounds survive to the second", func(t T) {
				got := byID(t, s, "r-sec")
				if !got.ValidFrom.Equal(from) || !got.ValidTo.Equal(to) {
					t.Fatalf("valid interval = %s; want %s — a store that rounds valid time answers "+
						"a question the caller did not ask, and does it silently",
						got.Valid(), chronicle.Between(from, to))
				}
			})
			t.Run("then the second before the lower bound is not covered", func(t T) {
				_, err := s.Get(ctx, chronicle.GetQuery{
					Kind: employee, EntityID: "e1", ValidAt: from.Add(-time.Second), TxAt: tx,
				})
				if !errors.Is(err, chronicle.ErrNotFound) {
					t.Fatalf("Get one second before ValidFrom = %v; want ErrNotFound", err)
				}
			})
			t.Run("then the second before the upper bound is covered", func(t T) {
				if _, err := s.Get(ctx, chronicle.GetQuery{
					Kind: employee, EntityID: "e1", ValidAt: to.Add(-time.Second), TxAt: tx,
				}); err != nil {
					t.Fatalf("Get one second before ValidTo = %v; want the record", err)
				}
			})
		})
	})

	t.Run("given a record inserted carrying a transaction time of its own", func(t T) {
		ctx := context.Background()
		s := newStore(t)
		// A caller who could choose this could make the log appear to have
		// known something before it did, which is the one thing the
		// transaction axis exists to rule out.
		claimed := jan.AddDate(-10, 0, 0)
		tx := apply(t, s, chronicle.ApplyRequest{TxAt: jan, Plan: chronicle.StaticWrite(chronicle.Write{Insert: []chronicle.Record{{
			ID: "r-claim", Kind: employee, EntityID: "e1", Data: []byte("v"),
			ValidFrom: feb, Actor: alice, TxFrom: claimed,
		}}})})

		t.Run("when it is read back", func(t T) {
			t.Run("then the store overwrote the transaction time it was handed", func(t T) {
				got := byID(t, s, "r-claim")
				if !got.TxFrom.Equal(tx) {
					t.Fatalf("TxFrom = %s; want the instant Apply returned, %s — a store that honours "+
						"an incoming TxFrom lets a caller backdate what the log knew", got.TxFrom, tx)
				}
			})
			t.Run("then it is invisible at the instant the caller claimed", func(t T) {
				_, err := s.Get(ctx, chronicle.GetQuery{
					Kind: employee, EntityID: "e1", ValidAt: mar, TxAt: claimed,
				})
				if !errors.Is(err, chronicle.ErrNotFound) {
					t.Fatalf("Get at the claimed TxFrom = %v; want ErrNotFound", err)
				}
			})
		})
	})

	t.Run("given a record with unbounded valid ends", func(t T) {
		s := newStore(t)
		tx := apply(t, s, chronicle.ApplyRequest{TxAt: jan, Plan: chronicle.StaticWrite(chronicle.Write{Insert: []chronicle.Record{{
			ID: "r-open", Kind: employee, EntityID: "e1", Data: []byte("v"), Actor: alice,
		}}})})

		t.Run("when it is read back", func(t T) {
			got := mustGet(t, s, chronicle.GetQuery{Kind: employee, EntityID: "e1", ValidAt: mar, TxAt: tx})
			t.Run("then unbounded ends round-trip as the zero time", func(t T) {
				if !got.ValidFrom.IsZero() || !got.ValidTo.IsZero() {
					t.Fatalf("valid interval = %s; want unbounded at both ends as zero times", got.Valid())
				}
			})
			t.Run("then it covers every instant on the valid axis", func(t T) {
				for _, at := range []time.Time{jan.AddDate(-100, 0, 0), mar, jun.AddDate(100, 0, 0)} {
					if _, err := s.Get(context.Background(), chronicle.GetQuery{
						Kind: employee, EntityID: "e1", ValidAt: at, TxAt: tx,
					}); err != nil {
						t.Fatalf("Get at %s = %v; an unbounded record covers all of valid time", at, err)
					}
				}
			})
			t.Run("then nil metadata stays nil rather than becoming an empty map", func(t T) {
				if len(got.Meta) != 0 {
					t.Fatalf("Meta = %v; want nil or empty", got.Meta)
				}
			})
		})
	})

	t.Run("given a record that is then superseded", func(t T) {
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

		t.Run("when the transaction axis is walked", func(t T) {
			t.Run("then the two instants are distinct and ordered", func(t T) {
				if !tx2.After(tx1) {
					t.Fatalf("second Apply returned %s, not after the first's %s — a superseded "+
						"record whose transaction interval is empty is invisible to every as-of query",
						tx2, tx1)
				}
			})
			t.Run("then the old belief is still readable at the old instant", func(t T) {
				got := mustGet(t, s, chronicle.GetQuery{Kind: employee, EntityID: "e1", ValidAt: mar, TxAt: tx1})
				if string(got.Data) != "v1" {
					t.Fatalf("data at the first instant = %s; want v1 — nothing is ever destroyed", got.Data)
				}
			})
			t.Run("then the new belief is what the latest instant shows", func(t T) {
				got := mustGet(t, s, chronicle.GetQuery{Kind: employee, EntityID: "e1", ValidAt: mar, TxAt: tx2})
				if string(got.Data) != "v2" {
					t.Fatalf("data at the second instant = %s; want v2", got.Data)
				}
			})
			t.Run("then the superseded record's interval closes exactly where the new one opens", func(t T) {
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
			t.Run("then exactly one record is current", func(t T) {
				recs := queryAll(t, s, chronicle.Query{Kind: employee, EntityID: "e1", CurrentOnly: true})
				if len(recs) != 1 || recs[0].ID != "r2" {
					t.Fatalf("current records = %v; want just r2", ids(recs))
				}
			})
		})
	})

	t.Run("given a supersession with nothing to insert", func(t T) {
		s := newStore(t)
		tx1 := apply(t, s, chronicle.ApplyRequest{TxAt: jan, Plan: chronicle.StaticWrite(chronicle.Write{Insert: []chronicle.Record{{
			ID: "r1", Kind: employee, EntityID: "e1", Data: []byte("v"), ValidFrom: feb, Actor: alice,
		}}})})
		closed := apply(t, s, chronicle.ApplyRequest{TxAt: tx1.Add(time.Second), Plan: chronicle.StaticWrite(chronicle.Write{Supersede: []chronicle.RecordID{"r1"}})})

		t.Run("when it is repeated", func(t T) {
			t.Run("then it is idempotent", func(t T) {
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

		t.Run("when it names a record that does not exist", func(t T) {
			t.Run("then it is not an error", func(t T) {
				if _, err := s.Apply(context.Background(), chronicle.ApplyRequest{TxAt: closed.Add(time.Hour), Plan: chronicle.StaticWrite(chronicle.Write{
					Supersede: []chronicle.RecordID{"no-such-record"}})}); err != nil {
					t.Fatalf("supersession of an unknown ID = %v; want nil", err)
				}
			})
		})
	})

	t.Run("given a split planned against a record someone else already closed", func(t T) {
		s := newStore(t)
		tx1 := apply(t, s, chronicle.ApplyRequest{TxAt: jan, Plan: chronicle.StaticWrite(chronicle.Write{Insert: []chronicle.Record{{
			ID: "r1", Kind: employee, EntityID: "e1", Data: []byte("v1"), ValidFrom: feb, ValidTo: apr, Actor: alice,
		}}})})
		apply(t, s, chronicle.ApplyRequest{TxAt: tx1.Add(time.Second), Plan: chronicle.StaticWrite(chronicle.Write{
			Supersede: []chronicle.RecordID{"r1"},
			Insert: []chronicle.Record{{
				ID: "r2", Kind: employee, EntityID: "e1", Data: []byte("v2"), ValidFrom: feb, ValidTo: apr, Actor: bob,
			}}})})

		t.Run("when the stale half of the split is applied", func(t T) {
			_, err := s.Apply(context.Background(), chronicle.ApplyRequest{TxAt: tx1.Add(2 * time.Second), Plan: chronicle.StaticWrite(chronicle.Write{
				Supersede: []chronicle.RecordID{"r1"},
				Insert: []chronicle.Record{{
					ID: "r3", Kind: employee, EntityID: "e1", Data: []byte("v3"), ValidFrom: feb, ValidTo: apr, Actor: alice,
				}}})})
			t.Run("then the store reports a conflict", func(t T) {
				if !errors.Is(err, chronicle.ErrConflict) {
					t.Fatalf("Apply = %v; want ErrConflict — applying half a split against a "+
						"pre-state that has moved leaves the entity's timeline overlapping", err)
				}
			})
			t.Run("then nothing was inserted", func(t T) {
				if recs := queryAll(t, s, chronicle.Query{Kind: employee, EntityID: "e1"}); len(recs) != 2 {
					t.Fatalf("records = %v; want just r1 and r2 — a conflicting write applies nothing", ids(recs))
				}
			})
			t.Run("then the entity still has exactly one current record", func(t T) {
				assertNoOverlap(t, s)
			})
		})
	})

	t.Run("given a record inserted twice under one ID", func(t T) {
		s := newStore(t)
		rec := chronicle.Record{
			ID: "r1", Kind: employee, EntityID: "e1", Data: []byte("original"), ValidFrom: feb, Actor: alice,
		}
		tx := apply(t, s, chronicle.ApplyRequest{TxAt: jan, Plan: chronicle.StaticWrite(chronicle.Write{Insert: []chronicle.Record{rec}})})

		t.Run("when the second insertion arrives", func(t T) {
			dup := rec
			dup.Data = []byte("overwritten")
			if _, err := s.Apply(context.Background(), chronicle.ApplyRequest{TxAt: tx, Plan: chronicle.StaticWrite(chronicle.Write{Insert: []chronicle.Record{dup}})}); err != nil {
				t.Fatalf("re-inserting an existing ID = %v; want nil", err)
			}
			t.Run("then the original is kept, because a log is append-only", func(t T) {
				if got := byID(t, s, "r1"); string(got.Data) != "original" {
					t.Fatalf("data = %s; want the original", got.Data)
				}
			})
			t.Run("then no second row appeared", func(t T) {
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

func seedFilterFixture(t T, newStore Factory) filterFixture {
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

func runQueryFilters(t T, newStore Factory) {
	t.Run("given a log spanning several kinds, actors and intents", func(t T) {
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
			t.Run("when filtered "+tc.name, func(t T) {
				t.Run("then exactly the expected records come back", func(t T) {
					got := queryAll(t, f.store, tc.q)
					assertIDs(t, got, tc.want)
				})
			})
		}

		// The transaction-axis filters are written separately because their
		// operands are instants the store chose rather than the suite.
		t.Run("when filtered by the first transaction instant", func(t T) {
			t.Run("then only the first generation is visible", func(t T) {
				assertIDs(t, queryAll(t, f.store, chronicle.Query{TxAt: f.tx1}), []chronicle.RecordID{"a", "b", "c"})
			})
		})
		t.Run("when filtered by the second transaction instant", func(t T) {
			t.Run("then the replaced record has dropped out and its replacement is in", func(t T) {
				assertIDs(t, queryAll(t, f.store, chronicle.Query{TxAt: f.tx2}), []chronicle.RecordID{"a", "b", "d"})
			})
		})
		t.Run("when filtered by a transaction range covering only the first generation", func(t T) {
			t.Run("then the second generation is excluded", func(t T) {
				assertIDs(t, queryAll(t, f.store, chronicle.Query{Tx: chronicle.Between(before, f.tx2)}),
					[]chronicle.RecordID{"a", "b", "c"})
			})
		})
		t.Run("when filtered by a transaction range starting at the second generation", func(t T) {
			t.Run("then the record closed at that instant is excluded", func(t T) {
				assertIDs(t, queryAll(t, f.store, chronicle.Query{Tx: chronicle.Since(f.tx2)}),
					[]chronicle.RecordID{"a", "b", "d"})
			})
		})
		t.Run("when filtered on both axes at once", func(t T) {
			t.Run("then the filters intersect rather than accumulate", func(t T) {
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

func runPagination(t T, newStore Factory) {
	t.Run("given many records, most of them sharing a transaction instant", func(t T) {
		s := newStore(t)

		// Twelve records land in one Apply and so share a transaction instant.
		// That is the hard case for pagination: the leading sort key ties for
		// every row and only the valid start and the record ID separate them.
		//
		// One entity each, because these are all current records and a store
		// is entitled — required, in the Postgres adapter's case — to refuse
		// two current records covering the same valid instant for one entity.
		// Only the valid starts repeat, which is what the tie needs.
		//
		// Every fourth record's valid start is unbounded. An unbounded start
		// sorts before every bounded one, and a SQL store that represents it as
		// NULL gets the opposite from a plain ORDER BY, since NULLS LAST is the
		// default for an ascending sort. Without these rows the two orders agree
		// on this fixture and the bug is invisible.
		var batch []chronicle.Record
		for i := 0; i < 12; i++ {
			var validFrom time.Time
			if i%4 != 3 {
				validFrom = feb.AddDate(0, i%3, 0)
			}
			batch = append(batch, chronicle.Record{
				ID:        chronicle.RecordID(fmt.Sprintf("g1-%02d", i)),
				Kind:      employee,
				EntityID:  fmt.Sprintf("e%02d", i),
				Data:      []byte("v"),
				ValidFrom: validFrom,
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

			t.Run("when scanned "+dir+" without a limit", func(t T) {
				full := queryAll(t, s, chronicle.Query{Descending: desc})
				t.Run("then every record is returned once", func(t T) {
					if len(full) != 17 {
						t.Fatalf("got %d records; want 17", len(full))
					}
				})
				t.Run("then the order is total and strictly monotonic", func(t T) {
					assertOrdered(t, full, desc)
				})

				for _, limit := range []int{1, 2, 5, 16, 17, 18} {
					t.Run(fmt.Sprintf("then paging at limit %d reproduces it exactly", limit), func(t T) {
						paged := drainPages(t, s, chronicle.Query{Descending: desc, Limit: limit}, limit)
						assertIDs(t, paged, ids(full))
					})
				}
			})

			t.Run("when a filtered scan is paged "+dir, func(t T) {
				q := chronicle.Query{Kind: employee, Descending: desc}
				full := queryAll(t, s, q)
				t.Run("then the filter survives cursor resumption", func(t T) {
					q.Limit = 3
					assertIDs(t, drainPages(t, s, q, 3), ids(full))
				})
			})
		}

		t.Run("when the unbounded valid starts are located within the order", func(t T) {
			t.Run("then they sort before every bounded start at the same instant", func(t T) {
				var bounded chronicle.RecordID
				for _, r := range queryAll(t, s, chronicle.Query{Kind: employee}) {
					if !r.ValidFrom.IsZero() {
						bounded = r.ID
						continue
					}
					if bounded != "" {
						t.Fatalf("%s has an unbounded valid start but sorts after %s, which does not: "+
							"unbounded is the earliest start there is, and a store holding it as NULL "+
							"gets this backwards unless its ORDER BY says NULLS FIRST", r.ID, bounded)
					}
				}
			})
		})

		t.Run("when a page is requested that exactly exhausts the result set", func(t T) {
			t.Run("then no cursor is offered, so callers need no trailing empty page", func(t T) {
				_, cursor, err := s.Query(context.Background(), chronicle.Query{Limit: 17})
				if err != nil {
					t.Fatalf("Query failed: %v", err)
				}
				if !cursor.IsZero() {
					t.Fatalf("cursor = %q; want empty when nothing was withheld", cursor)
				}
			})
		})

		t.Run("when a cursor from an ascending scan is reused", func(t T) {
			page, cursor, err := s.Query(context.Background(), chronicle.Query{Limit: 4})
			if err != nil {
				t.Fatalf("Query failed: %v", err)
			}
			t.Run("then it resumes strictly after the last record of the page", func(t T) {
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

func runQueryValidation(t T, newStore Factory) {
	t.Run("given a malformed query", func(t T) {
		s := newStore(t)
		ctx := context.Background()

		t.Run("when its valid range is inverted", func(t T) {
			t.Run("then it is rejected before any rows are read", func(t T) {
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

		t.Run("when its transaction range is inverted", func(t T) {
			t.Run("then it is rejected and names the transaction axis", func(t T) {
				_, _, err := s.Query(ctx, chronicle.Query{Tx: chronicle.Between(apr, feb)})
				var ie *chronicle.IntervalError
				if !errors.As(err, &ie) || ie.Field != "transaction" {
					t.Fatalf("error = %v; want an *IntervalError naming the transaction axis", err)
				}
			})
		})

		t.Run("when its intent is not one chronicle defines", func(t T) {
			t.Run("then it is rejected rather than matching nothing", func(t T) {
				_, _, err := s.Query(ctx, chronicle.Query{Intent: chronicle.Intent(200), HasIntent: true})
				if err == nil {
					t.Fatal("Query with an undefined intent = nil; want an error")
				}
			})
		})

		t.Run("when its cursor is not one this library minted", func(t T) {
			for _, bad := range []chronicle.Cursor{"!!!not base64!!!", "YWJj", "Y29ycnVwdA"} {
				t.Run("then "+string(bad)+" is rejected as an invalid cursor", func(t T) {
					_, _, err := s.Query(ctx, chronicle.Query{After: bad})
					if !errors.Is(err, chronicle.ErrInvalidCursor) {
						t.Fatalf("Query with cursor %q = %v; want ErrInvalidCursor", bad, err)
					}
				})
			}
		})
	})

	t.Run("given a cancelled context", func(t T) {
		s := newStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		t.Run("when the store is used", func(t T) {
			t.Run("then Apply reports the context error", func(t T) {
				if _, err := s.Apply(ctx, chronicle.ApplyRequest{Plan: chronicle.StaticWrite(chronicle.Write{})}); !errors.Is(err, context.Canceled) {
					t.Fatalf("Apply = %v; want context.Canceled", err)
				}
			})
			t.Run("then Get reports the context error", func(t T) {
				_, err := s.Get(ctx, chronicle.GetQuery{Kind: employee, EntityID: "e1"})
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Get = %v; want context.Canceled", err)
				}
			})
			t.Run("then Query reports the context error", func(t T) {
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

func runLog(t T, newStore Factory) {
	t.Run("given a log over the store", func(t T) {
		ctx := context.Background()

		t.Run("when a fact is asserted and then contradicted over part of its range", func(t T) {
			s := newStore(t)
			l := chronicle.NewLog(s)

			if _, err := l.Put(ctx, employee, "e1", []byte(`{"grade":"L3"}`), feb, time.Time{}, alice); err != nil {
				t.Fatalf("Put failed: %v", err)
			}
			second, err := l.Put(ctx, employee, "e1", []byte(`{"grade":"L4"}`), apr, jun, bob)
			if err != nil {
				t.Fatalf("Put failed: %v", err)
			}

			t.Run("then the entity's timeline is tiled without gaps or overlaps", func(t T) {
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
			t.Run("then the remainders are attributed to the original author", func(t T) {
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
			t.Run("then the store's non-overlap invariant holds", func(t T) {
				assertNoOverlap(t, s)
			})
		})

		t.Run("when a write carries metadata no store can hold", func(t T) {
			// A NUL byte inside a metadata key or value is unrepresentable in
			// PostgreSQL jsonb, so the library rejects it at the write boundary
			// for every store alike — were it left to the store, the same write
			// would succeed in memory and fail on Postgres with a driver error.
			s := newStore(t)
			l := chronicle.NewLog(s)

			for _, tc := range []struct {
				name string
				opt  chronicle.WriteOption
			}{
				{"a NUL byte in a key", chronicle.WithMetaValue("bad\x00key", "v")},
				{"a NUL byte in a value", chronicle.WithMetaValue("k", "bad\x00value")},
			} {
				t.Run("then "+tc.name+" is rejected identically for every store", func(t T) {
					_, err := l.Put(ctx, employee, "e-nul", []byte(`{"a":1}`), feb, time.Time{}, alice, tc.opt)
					if !errors.Is(err, chronicle.ErrInvalidMeta) {
						t.Fatalf("Put = %v; want ErrInvalidMeta", err)
					}
					recs, _, qerr := s.Query(ctx, chronicle.Query{Kind: employee, EntityID: "e-nul"})
					if qerr != nil {
						t.Fatalf("Query failed: %v", qerr)
					}
					if len(recs) != 0 {
						t.Fatalf("the rejected write left %d records behind; want none", len(recs))
					}
				})
			}
		})

		t.Run("when a belief is corrected retroactively", func(t T) {
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

			t.Run("then what we believe now is the correction", func(t T) {
				got, err := l.Get(ctx, employee, "e1", chronicle.As{ValidAt: mar, TxAt: corrected.TxAt})
				if err != nil {
					t.Fatalf("Get failed: %v", err)
				}
				if string(got.Data) != `{"salary":55000}` {
					t.Fatalf("current belief = %s; want the correction", got.Data)
				}
			})
			t.Run("then what we believed before the correction is unchanged", func(t T) {
				got, err := l.Get(ctx, employee, "e1", chronicle.As{ValidAt: mar, TxAt: first.TxAt})
				if err != nil {
					t.Fatalf("Get failed: %v", err)
				}
				if string(got.Data) != `{"salary":50000}` {
					t.Fatalf("prior belief = %s; want the original — this is the question "+
						"uni-temporal systems answer wrongly", got.Data)
				}
			})
			t.Run("then the correction is auditable as one", func(t T) {
				recs, err := l.History(ctx, employee, "e1", chronicle.WithIntent(chronicle.IntentCorrection))
				if err != nil {
					t.Fatalf("History failed: %v", err)
				}
				if len(recs) != 1 || recs[0].Reason != "payroll error" || recs[0].Actor.ID != bob.ID {
					t.Fatalf("corrections = %+v; want one, by %s, reasoned", recs, bob.ID)
				}
			})
			t.Run("then the change is visible field by field", func(t T) {
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

		t.Run("when an entity accumulates many overlapping writes", func(t T) {
			s := newStore(t)
			l := chronicle.NewLog(s)
			for i := 0; i < 12; i++ {
				from := feb.AddDate(0, i%5, 0)
				to := from.AddDate(0, 2, 0)
				if _, err := l.Put(ctx, employee, "e1", []byte(fmt.Sprintf(`{"n":%d}`, i)), from, to, alice); err != nil {
					t.Fatalf("Put %d failed: %v", i, err)
				}
			}
			t.Run("then no two current records ever cover the same instant", func(t T) {
				assertNoOverlap(t, s)
			})
			t.Run("then history is dense on the transaction axis", func(t T) {
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

		t.Run("when an entity has no history at all", func(t T) {
			s := newStore(t)
			l := chronicle.NewLog(s)
			t.Run("then Get reports not found", func(t T) {
				if _, err := l.Get(ctx, employee, "ghost", chronicle.Now()); !errors.Is(err, chronicle.ErrNotFound) {
					t.Fatalf("Get = %v; want ErrNotFound", err)
				}
			})
			t.Run("then History reports emptiness rather than failure", func(t T) {
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
func apply(t T, s chronicle.Store, req chronicle.ApplyRequest) time.Time {
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

func mustGet(t T, s chronicle.Store, q chronicle.GetQuery) *chronicle.Record {
	t.Helper()
	rec, err := s.Get(context.Background(), q)
	if err != nil {
		t.Fatalf("Get(%+v) failed: %v", q, err)
	}
	return rec
}

// byID finds one record by ID through the query surface, since [chronicle.Store]
// has no lookup by ID.
func byID(t T, s chronicle.Store, id chronicle.RecordID) chronicle.Record {
	t.Helper()
	for _, r := range queryAll(t, s, chronicle.Query{}) {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no record with ID %s", id)
	return chronicle.Record{}
}

func queryAll(t T, s chronicle.Store, q chronicle.Query) []chronicle.Record {
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
func drainPages(t T, s chronicle.Store, q chronicle.Query, limit int) []chronicle.Record {
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

func assertIDs(t T, got []chronicle.Record, want []chronicle.RecordID) {
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
func assertOrdered(t T, recs []chronicle.Record, descending bool) {
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
func assertNoOverlap(t T, s chronicle.Store) {
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

func assertRecordEqual(t T, got, want chronicle.Record) {
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
func segments(t T, l *chronicle.Log, kind, entityID string, txAt time.Time) []string {
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
