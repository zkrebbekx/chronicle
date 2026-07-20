package chroniclefest_test

import (
	"testing"
	"time"

	"github.com/zkrebbekx/chronicle"
	"github.com/zkrebbekx/chronicle/chroniclefest"
)

// fault is one deliberately broken store and the check the suite must fail on
// when it meets it.
type fault struct {
	// name says, in one sentence, what the store gets wrong.
	name string
	// newStore builds it.
	newStore chroniclefest.Factory
	// wants are substrings that must each appear in some failure's subtest path
	// or message. They are how "the suite failed" is distinguished from "the
	// suite failed for the reason the fault predicts" — without them a suite
	// that fell over on an unrelated check would look like it had caught this.
	wants []string
}

func faults() []fault {
	return []fault{
		{
			name: "closes the superseded records a second after the insertions land",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &deferredSupersede{newBase(t)}
			},
			wants: []string{
				"then the superseded record's interval closes exactly where the new one opens",
				"a gap or an overlap on the transaction axis",
			},
		},
		{
			name: "supersedes but drops the insertions, leaving a hole in valid time",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &droppedInserts{newBase(t)}
			},
			wants: []string{
				"then the new belief is what the latest instant shows",
				"then exactly one record is current",
				"then the entity's timeline is tiled without gaps or overlaps",
			},
		},
		{
			name: "inserts without closing anything, so every generation stays current",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &ignoredSupersessions{newBase(t)}
			},
			wants: []string{
				"the superseded record is still current",
				"two current records covering the same valid instant",
			},
		},
		{
			name: "applies half a stale split instead of reporting a conflict",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &noConflictDetection{newBase(t)}
			},
			wants: []string{
				"then the store reports a conflict",
				"want ErrConflict",
				"a conflicting write applies nothing",
			},
		},
		{
			name: "stamps its own instant and reports back the caller's",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &callerChosenTx{newBase(t)}
			},
			wants: []string{
				"then the transaction axis hides it",
				"Get before TxFrom",
				"TxFrom = ",
			},
		},
		{
			name: "reports the zero time from Apply",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &zeroTx{newBase(t)}
			},
			wants: []string{"a store must report the transaction instant it assigned"},
		},
		{
			name: "stamps every write with one fixed instant",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &frozenTx{newBase(t)}
			},
			wants: []string{
				"then the two instants are distinct and ordered",
				"is invisible to every as-of query",
			},
		},
		{
			name: "resolves Get uni-temporally, ignoring both axes",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &uniTemporal{newBase(t)}
			},
			wants: []string{
				"then the upper bound is exclusive",
				"the upper bound is exclusive",
			},
		},
		{
			name: "returns only current records, so prior belief is unreachable",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &hidesSuperseded{newBase(t)}
			},
			wants: []string{"no record with ID r1"},
		},
		{
			name: "orders by valid start before transaction start",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &validFirstOrder{newBase(t)}
			},
			wants: []string{
				"then the order is total and strictly monotonic",
				"are out of order or tied",
			},
		},
		{
			name: "drops the last row of every truncated page but keeps its cursor",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &pageDropsRow{newBase(t)}
			},
			wants: []string{"reproduces it exactly"},
		},
		{
			name: "mints the page cursor from the second-to-last row",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &pageRepeatsRow{newBase(t)}
			},
			wants: []string{"appeared on two pages"},
		},
		{
			name: "offers a cursor when it withheld nothing",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &alwaysCursor{newBase(t)}
			},
			wants: []string{
				"Query without a limit offered a cursor",
				"want empty when nothing was withheld",
			},
		},
		{
			name: "stores unbounded valid bounds as magic timestamps",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &sentinelValidBounds{newBase(t)}
			},
			wants: []string{
				"then unbounded ends round-trip as the zero time",
				"want unbounded at both ends as zero times",
				"an unbounded record covers all of valid time",
			},
		},
		{
			name: "drops Meta on the way out",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &mapRecords{base: newBase(t), fn: func(r chronicle.Record) chronicle.Record {
					r.Meta = nil
					return r
				}}
			},
			wants: []string{"meta = map[]; want map["},
		},
		{
			name: "drops Reason on the way out",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &mapRecords{base: newBase(t), fn: func(r chronicle.Record) chronicle.Record {
					r.Reason = ""
					return r
				}}
			},
			wants: []string{`reason = ""; want "annual review"`},
		},
		{
			name: "drops Intent on the way out",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &mapRecords{base: newBase(t), fn: func(r chronicle.Record) chronicle.Record {
					r.Intent = chronicle.IntentAssert
					return r
				}}
			},
			wants: []string{"intent = assert; want correction"},
		},
		{
			name: "truncates Data on the way out",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &mapRecords{base: newBase(t), fn: func(r chronicle.Record) chronicle.Record {
					if len(r.Data) > 1 {
						r.Data = r.Data[:1]
					}
					return r
				}}
			},
			wants: []string{"data = "},
		},
		{
			name: "hands the same record back to every caller",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &aliasesRecords{base: newBase(t)}
			},
			wants: []string{"shares mutable state with the store"},
		},
		{
			name: "reports the wrong entity on a record it returns",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &mapRecords{base: newBase(t), fn: func(r chronicle.Record) chronicle.Record {
					r.EntityID = "somebody-else"
					return r
				}}
			},
			wants: []string{"identity: got "},
		},
		{
			name: "shifts valid bounds by a minute on the way out",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &mapRecords{base: newBase(t), fn: func(r chronicle.Record) chronicle.Record {
					if !r.ValidFrom.IsZero() {
						r.ValidFrom = r.ValidFrom.Add(time.Minute)
					}
					return r
				}}
			},
			wants: []string{"valid interval = "},
		},
		{
			name: "attributes every record to the wrong actor",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &mapRecords{base: newBase(t), fn: func(r chronicle.Record) chronicle.Record {
					r.Actor = chronicle.Actor{ID: "u-nobody", Type: "user", Name: "Nobody"}
					return r
				}}
			},
			wants: []string{
				"actor = ",
				"a log must not claim someone asserted data they never sent",
			},
		},
		{
			name: "corrupts metadata values while keeping the keys",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &mapRecords{base: newBase(t), fn: func(r chronicle.Record) chronicle.Record {
					if len(r.Meta) == 0 {
						return r
					}
					meta := make(map[string]string, len(r.Meta))
					for k := range r.Meta {
						meta[k] = "tampered"
					}
					r.Meta = meta
					return r
				}}
			},
			wants: []string{"meta = map["},
		},
		{
			name: "reports a record nothing has superseded as already closed",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &mapRecords{base: newBase(t), fn: func(r chronicle.Record) chronicle.Record {
					if r.TxTo.IsZero() {
						r.TxTo = farFuture
					}
					return r
				}}
			},
			wants: []string{"a record nothing has superseded must be current"},
		},
		{
			name: "rewrites a record's transaction start when it closes it",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &mapRecords{base: newBase(t), fn: func(r chronicle.Record) chronicle.Record {
					if !r.IsCurrent() {
						r.TxFrom = r.TxTo
					}
					return r
				}}
			},
			wants: []string{"transaction time is never rewritten"},
		},
		{
			name: "treats the lower valid bound as exclusive",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &exclusiveLowerBound{newBase(t)}
			},
			wants: []string{"the lower bound is inclusive"},
		},
		{
			name: "resolves Get against the whole log rather than the entity named",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &getIgnoresIdentity{newBase(t)}
			},
			wants: []string{
				"then entity IDs do not leak between entities",
				"then entity IDs do not leak between kinds",
			},
		},
		{
			name: "returns every record twice",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &duplicatesRows{newBase(t)}
			},
			wants: []string{
				"want 17",
				"out of order or tied",
			},
		},
		{
			name: "ignores the page limit",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &ignoresLimit{newBase(t)}
			},
			wants: []string{"limit was "},
		},
		{
			name: "answers a resumed query with nothing",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &emptyAfterCursor{newBase(t)}
			},
			wants: []string{"resuming from a cursor returned nothing"},
		},
		{
			name: "rescans from the start when handed a cursor it cannot parse",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &acceptsBadCursor{newBase(t)}
			},
			wants: []string{"want ErrInvalidCursor"},
		},
		{
			name: "answers a malformed query with no rows instead of an error",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &skipsValidation{newBase(t)}
			},
			wants: []string{
				"then it is rejected before any rows are read",
				"want ErrInvalidInterval",
				"Query with an undefined intent = nil",
			},
		},
		{
			name: "reports a malformed interval without saying which one",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &bareIntervalError{newBase(t)}
			},
			wants: []string{"want an *IntervalError naming the valid axis"},
		},
		{
			name: "errors when a query is resumed from a cursor",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &resumeFails{newBase(t)}
			},
			wants: []string{"Query page "},
		},
		{
			name: "reports a conflict for a bare supersession that has nothing to do",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &strictSupersede{newBase(t)}
			},
			wants: []string{
				"repeated supersession = ",
				"supersession of an unknown ID = ",
			},
		},
		{
			name: "surfaces a duplicate key instead of keeping what it holds",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &rejectsDuplicateIDs{newBase(t)}
			},
			wants: []string{"re-inserting an existing ID = "},
		},
		{
			name: "names the wrong entity in its not-found error",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &wrongNotFoundCoordinates{newBase(t)}
			},
			wants: []string{"want kind employee entity e1"},
		},
		{
			name: "invents a record when a query matches nothing",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &phantomRow{newBase(t)}
			},
			wants: []string{
				"want none of either",
				"records after an empty write",
				"History = [phantom]; want nothing",
			},
		},
		{
			name: "materialises absent metadata as a map of its own",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &mapRecords{base: newBase(t), fn: func(r chronicle.Record) chronicle.Record {
					if r.Meta == nil {
						r.Meta = map[string]string{"_row": "1"}
					}
					return r
				}}
			},
			wants: []string{"want nil or empty"},
		},
		{
			name: "cannot apply a write that both supersedes and inserts",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &splitApplyFails{newBase(t)}
			},
			wants: []string{"Put failed", "Correct failed"},
		},
		{
			name: "cannot write at all",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &applyFails{newBase(t)}
			},
			wants: []string{
				"Apply failed",
				"Apply of an empty write = ",
				"Put failed",
			},
		},
		{
			name: "cannot read at all",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &readsFail{newBase(t)}
			},
			wants: []string{
				"Query failed",
				"History failed",
				"Timeline failed",
			},
		},
		{
			// Until this fault was injected, the suite passed this store: no
			// fixture held a record with an unbounded valid start alongside a
			// bounded one at the same transaction instant, so the two orders
			// agreed everywhere it looked.
			name: "sorts unbounded valid starts last, as a plain SQL ORDER BY does",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &nullsLastOrder{newBase(t)}
			},
			wants: []string{
				"then they sort before every bounded start at the same instant",
				"then the order is total and strictly monotonic",
				"unless its ORDER BY says NULLS FIRST",
			},
		},
		{
			// Also a hole until fault injection found it: the contract says an
			// incoming TxFrom is overwritten, and nothing inserted a record
			// carrying one.
			name: "honours the TxFrom an inserted record arrives carrying",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &honoursIncomingTxFrom{base: newBase(t)}
			},
			wants: []string{
				"then the store overwrote the transaction time it was handed",
				"lets a caller backdate what the log knew",
			},
		},
		{
			// And a third: every fixture timestamp was midnight on the first of
			// a month, so a store that rounded valid time agreed with all of
			// them by coincidence.
			name: "keeps valid time only to the day",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &truncatesValidToTheDay{newBase(t)}
			},
			wants: []string{
				"then both bounds survive to the second",
				"answers a question the caller did not ask",
			},
		},
		{
			name: "ignores a cancelled context",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &ignoresCancellation{newBase(t)}
			},
			wants: []string{
				"then Apply reports the context error",
				"then Get reports the context error",
				"then Query reports the context error",
			},
		},
	}
}

// TestSuiteCatchesFaults is the test that gives the conformance suite its
// standing. Every case is a store broken in exactly one nameable way, and the
// assertion is that the suite notices, and notices on the check that describes
// that fault rather than on some unrelated one that happened to fall over
// first.
//
// A case that fails here means the suite has a hole: some invariant it appears
// to check, it does not.
func TestSuiteCatchesFaults(t *testing.T) {
	t.Run("given a store broken in exactly one way", func(t *testing.T) {
		for _, f := range faults() {
			t.Run("when the store "+f.name, func(t *testing.T) {
				t.Parallel()
				rec := run(f.newStore)

				t.Run("then the conformance suite fails", func(t *testing.T) {
					if len(rec.failures()) == 0 {
						t.Fatal("the suite passed a store that violates the contract; " +
							"the check that should have caught this is missing or vacuous")
					}
				})
				for _, want := range f.wants {
					t.Run("then it fails on "+want, func(t *testing.T) {
						if !rec.matched(want) {
							t.Fatalf("no failure mentions %q, so the suite failed for some other "+
								"reason than the injected fault.\n%s", want, rec.summary())
						}
					})
				}
			})
		}
	})
}

// TestSuiteHalvesCatchFaults checks that the two halves of the suite are
// individually load-bearing. A store author running only RunStoreT because the
// log half is not relevant yet must still be told about a broken store, and the
// log half must not be quietly passing on assertions the store half made.
func TestSuiteHalvesCatchFaults(t *testing.T) {
	t.Run("given a store that never closes what it supersedes", func(t *testing.T) {
		broken := func(t chroniclefest.T) chronicle.Store {
			return &ignoredSupersessions{newBase(t)}
		}

		t.Run("when only the store half runs", func(t *testing.T) {
			rec := runWith(func(t chroniclefest.T) { chroniclefest.RunStoreT(t, broken) })
			t.Run("then it still catches the fault", func(t *testing.T) {
				if !rec.matched("the superseded record is still current") {
					t.Fatalf("the store half missed it:\n%s", rec.summary())
				}
			})
		})

		t.Run("when only the log half runs", func(t *testing.T) {
			rec := runWith(func(t chroniclefest.T) { chroniclefest.RunLogT(t, broken) })
			t.Run("then it still catches the fault", func(t *testing.T) {
				if !rec.matched("two current records covering the same valid instant") {
					t.Fatalf("the log half missed it:\n%s", rec.summary())
				}
			})
		})
	})
}

// TestSuitePassesTheReferenceStore proves the harness is not trivially failing
// everything: the same recorder, driving the same suite, reports nothing
// against a conforming store.
func TestSuitePassesTheReferenceStore(t *testing.T) {
	t.Run("given the reference store", func(t *testing.T) {
		t.Run("when the suite runs under the recording harness", func(t *testing.T) {
			rec := run(func(t chroniclefest.T) chronicle.Store { return newBase(t) })
			t.Run("then it reports no failures", func(t *testing.T) {
				if n := len(rec.failures()); n != 0 {
					t.Fatalf("the recording harness failed a conforming store:\n%s", rec.summary())
				}
			})
		})
	})
}
