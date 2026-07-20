package chronicle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

var alice = Actor{ID: "u-1", Type: "user", Name: "Alice"}
var bob = Actor{ID: "u-2", Type: "user", Name: "Bob"}

const employee = "employee"

func newTestLog(t *testing.T, opts ...Option) (*Log, *MemStore, *FixedClock) {
	t.Helper()
	clock := NewFixedClock(t0)
	store := NewMemStore()
	all := append([]Option{WithClock(clock)}, opts...)
	return NewLog(store, all...), store, clock
}

func mustPut(t *testing.T, l *Log, id string, data string, from, to time.Time) Result {
	t.Helper()
	res, err := l.Put(context.Background(), employee, id, []byte(data), from, to, alice)
	if err != nil {
		t.Fatalf("Put(%s, %s, %s) failed: %v", id, Between(from, to), data, err)
	}
	return res
}

func mustCorrect(t *testing.T, l *Log, id string, data string, from, to time.Time) Result {
	t.Helper()
	res, err := l.Correct(context.Background(), employee, id, []byte(data), from, to, bob)
	if err != nil {
		t.Fatalf("Correct(%s, %s, %s) failed: %v", id, Between(from, to), data, err)
	}
	return res
}

// currentSegments returns the entity's current valid-time tiling as a
// comparable summary, ordered by valid start.
func currentSegments(t *testing.T, l *Log, id string) []string {
	t.Helper()
	recs, err := l.History(context.Background(), employee, id, CurrentOnly())
	if err != nil {
		t.Fatalf("History failed: %v", err)
	}
	sort.Slice(recs, func(i, j int) bool { return compareStarts(recs[i].ValidFrom, recs[j].ValidFrom) < 0 })
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = fmt.Sprintf("%s=%s", r.Valid(), r.Data)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// assertInvariants checks everything the library promises about a store's
// contents. It is called after every mutating step in the property test and
// at the end of most unit tests.
func assertInvariants(t *testing.T, store *MemStore) {
	t.Helper()

	recs, _, err := store.Query(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}

	byEntity := map[entityKey][]Record{}
	seenID := map[RecordID]bool{}
	for _, r := range recs {
		if seenID[r.ID] {
			t.Fatalf("duplicate record ID %s", r.ID)
		}
		seenID[r.ID] = true

		// No empty or inverted valid intervals may ever be stored.
		if err := r.Valid().Validate(); err != nil {
			t.Fatalf("record %s has an invalid valid interval %s", r.ID, r.Valid())
		}
		// Transaction time is always assigned and always UTC.
		if r.TxFrom.IsZero() {
			t.Fatalf("record %s has no transaction start", r.ID)
		}
		if r.TxFrom.Location() != time.UTC {
			t.Fatalf("record %s stores transaction time in %v, not UTC", r.ID, r.TxFrom.Location())
		}
		// Every superseded record must have a strictly positive transaction
		// interval, or no as-of query could ever have observed it.
		if !r.TxTo.IsZero() && !r.TxTo.After(r.TxFrom) {
			t.Fatalf("record %s has TxTo %s not after TxFrom %s", r.ID, r.TxTo, r.TxFrom)
		}
		// Actor attribution is never absent.
		if r.Actor.ID == "" {
			t.Fatalf("record %s has no actor", r.ID)
		}
		key := entityKey{kind: r.Kind, entityID: r.EntityID}
		byEntity[key] = append(byEntity[key], r)
	}

	// The headline invariant: at any transaction instant, an entity's current
	// valid intervals must not overlap. Checking the current set is equivalent
	// to checking every instant, because the current set is what every past
	// instant's set became.
	for key, ers := range byEntity {
		var current []Record
		for _, r := range ers {
			if r.IsCurrent() {
				current = append(current, r)
			}
		}
		for i := 0; i < len(current); i++ {
			for j := i + 1; j < len(current); j++ {
				if current[i].Valid().Overlaps(current[j].Valid()) {
					t.Fatalf("entity %s/%s has overlapping current records: %s (%s) and %s (%s)",
						key.kind, key.entityID,
						current[i].ID, current[i].Valid(),
						current[j].ID, current[j].Valid())
				}
			}
		}
	}

	// The same check, applied at every distinct transaction instant that has
	// ever existed: the tiling must have been non-overlapping then too.
	instants := map[time.Time]bool{}
	for _, r := range recs {
		instants[r.TxFrom] = true
	}
	for at := range instants {
		perEntity := map[entityKey][]Record{}
		for _, r := range recs {
			if r.Tx().Contains(at) {
				k := entityKey{kind: r.Kind, entityID: r.EntityID}
				perEntity[k] = append(perEntity[k], r)
			}
		}
		for key, ers := range perEntity {
			for i := 0; i < len(ers); i++ {
				for j := i + 1; j < len(ers); j++ {
					if ers[i].Valid().Overlaps(ers[j].Valid()) {
						t.Fatalf("at tx instant %s entity %s/%s had overlapping records %s (%s) and %s (%s)",
							at, key.kind, key.entityID,
							ers[i].ID, ers[i].Valid(), ers[j].ID, ers[j].Valid())
					}
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// the bitemporal question
// ---------------------------------------------------------------------------

func TestRetroactiveCorrectionPreservesPriorBelief(t *testing.T) {
	t.Run("given a salary asserted for March and then retroactively corrected", func(t *testing.T) {
		ctx := context.Background()
		l, store, clock := newTestLog(t)

		// In March we record that Alice earns 50000 from 1 March onwards.
		clock.Set(t2)
		first := mustPut(t, l, "alice", `{"salary":50000}`, t2, time.Time{})
		originalBelief := first.TxAt

		// In April we discover the March figure was wrong: it was always
		// 60000. This is a correction, not a raise — the valid interval is the
		// same stretch of world-time.
		clock.Set(t3)
		second := mustCorrect(t, l, "alice", `{"salary":60000}`, t2, time.Time{})
		correctedBelief := second.TxAt

		t.Run("when asked what we believe now about March", func(t *testing.T) {
			t.Run("then the corrected figure is returned", func(t *testing.T) {
				rec, err := l.Get(ctx, employee, "alice", As{ValidAt: t2})
				if err != nil {
					t.Fatalf("Get failed: %v", err)
				}
				if string(rec.Data) != `{"salary":60000}` {
					t.Fatalf("current belief about March = %s; want the corrected 60000", rec.Data)
				}
			})
		})

		t.Run("when asked what we believed in March about March", func(t *testing.T) {
			t.Run("then the original figure is still returned", func(t *testing.T) {
				rec, err := l.Get(ctx, employee, "alice", As{ValidAt: t2, TxAt: originalBelief})
				if err != nil {
					t.Fatalf("Get at the original transaction instant failed: %v", err)
				}
				if string(rec.Data) != `{"salary":50000}` {
					t.Fatalf("belief as of %s = %s; want the original 50000 — a retroactive "+
						"correction must not rewrite what we appear to have known", originalBelief, rec.Data)
				}
			})

			t.Run("then that record is marked superseded at the moment of correction", func(t *testing.T) {
				rec, err := l.Get(ctx, employee, "alice", As{ValidAt: t2, TxAt: originalBelief})
				if err != nil {
					t.Fatalf("Get failed: %v", err)
				}
				if rec.IsCurrent() {
					t.Fatal("the original belief should no longer be current")
				}
				if !rec.TxTo.Equal(correctedBelief) {
					t.Fatalf("original record closed at %s; want the correction instant %s", rec.TxTo, correctedBelief)
				}
			})
		})

		t.Run("when asked what we believe now, at the current instant", func(t *testing.T) {
			t.Run("then the corrected figure is returned", func(t *testing.T) {
				rec, err := l.Get(ctx, employee, "alice", Now())
				if err != nil {
					t.Fatalf("Get failed: %v", err)
				}
				if string(rec.Data) != `{"salary":60000}` {
					t.Fatalf("Get(now) = %s; want 60000", rec.Data)
				}
			})
		})

		t.Run("when the correction is examined", func(t *testing.T) {
			t.Run("then it is flagged as a correction rather than an assertion", func(t *testing.T) {
				rec, err := l.Get(ctx, employee, "alice", Now())
				if err != nil {
					t.Fatalf("Get failed: %v", err)
				}
				if rec.Intent != IntentCorrection {
					t.Fatalf("Intent = %v; want %v — without the flag a retroactive fix is "+
						"indistinguishable from a late-arriving fact", rec.Intent, IntentCorrection)
				}
			})
			t.Run("then it is attributed to whoever made it", func(t *testing.T) {
				rec, err := l.Get(ctx, employee, "alice", Now())
				if err != nil {
					t.Fatalf("Get failed: %v", err)
				}
				if rec.Actor.ID != bob.ID {
					t.Fatalf("Actor = %v; want %v", rec.Actor, bob)
				}
			})
			t.Run("then corrections are findable in history by intent", func(t *testing.T) {
				recs, err := l.History(ctx, employee, "alice", WithIntent(IntentCorrection))
				if err != nil {
					t.Fatalf("History failed: %v", err)
				}
				if len(recs) != 1 || string(recs[0].Data) != `{"salary":60000}` {
					t.Fatalf("corrections in history = %d records; want exactly the one correction", len(recs))
				}
			})
		})

		t.Run("when the whole history is read", func(t *testing.T) {
			t.Run("then nothing was destroyed", func(t *testing.T) {
				recs, err := l.History(ctx, employee, "alice")
				if err != nil {
					t.Fatalf("History failed: %v", err)
				}
				if len(recs) != 2 {
					t.Fatalf("history has %d records; want 2 — supersession must not delete", len(recs))
				}
			})
		})

		assertInvariants(t, store)
	})
}

// ---------------------------------------------------------------------------
// the overlap taxonomy, exercised through the write path
// ---------------------------------------------------------------------------

func TestPutOverlapCases(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name string
		// existing is written first, then incoming.
		existing, incoming Interval
		// want is the resulting current tiling, ordered by valid start.
		want []string
	}{
		{
			name:     "identical intervals",
			existing: Between(t1, t3),
			incoming: Between(t1, t3),
			want:     []string{"[2026-02-01T00:00:00Z, 2026-04-01T00:00:00Z)=new"},
		},
		{
			name:     "incoming nested strictly inside existing",
			existing: Between(t0, t5),
			incoming: Between(t2, t3),
			want: []string{
				"[2026-01-01T00:00:00Z, 2026-03-01T00:00:00Z)=old",
				"[2026-03-01T00:00:00Z, 2026-04-01T00:00:00Z)=new",
				"[2026-04-01T00:00:00Z, 2026-06-01T00:00:00Z)=old",
			},
		},
		{
			name:     "incoming contains existing",
			existing: Between(t2, t3),
			incoming: Between(t0, t5),
			want:     []string{"[2026-01-01T00:00:00Z, 2026-06-01T00:00:00Z)=new"},
		},
		{
			name:     "left overlap: incoming starts before and ends inside",
			existing: Between(t2, t5),
			incoming: Between(t0, t3),
			want: []string{
				"[2026-01-01T00:00:00Z, 2026-04-01T00:00:00Z)=new",
				"[2026-04-01T00:00:00Z, 2026-06-01T00:00:00Z)=old",
			},
		},
		{
			name:     "right overlap: incoming starts inside and ends after",
			existing: Between(t0, t3),
			incoming: Between(t2, t5),
			want: []string{
				"[2026-01-01T00:00:00Z, 2026-03-01T00:00:00Z)=old",
				"[2026-03-01T00:00:00Z, 2026-06-01T00:00:00Z)=new",
			},
		},
		{
			name:     "adjacent, incoming immediately after: no supersession",
			existing: Between(t0, t2),
			incoming: Between(t2, t4),
			want: []string{
				"[2026-01-01T00:00:00Z, 2026-03-01T00:00:00Z)=old",
				"[2026-03-01T00:00:00Z, 2026-05-01T00:00:00Z)=new",
			},
		},
		{
			name:     "adjacent, incoming immediately before: no supersession",
			existing: Between(t2, t4),
			incoming: Between(t0, t2),
			want: []string{
				"[2026-01-01T00:00:00Z, 2026-03-01T00:00:00Z)=new",
				"[2026-03-01T00:00:00Z, 2026-05-01T00:00:00Z)=old",
			},
		},
		{
			name:     "disjoint with a gap",
			existing: Between(t0, t1),
			incoming: Between(t3, t4),
			want: []string{
				"[2026-01-01T00:00:00Z, 2026-02-01T00:00:00Z)=old",
				"[2026-04-01T00:00:00Z, 2026-05-01T00:00:00Z)=new",
			},
		},
		{
			name:     "existing unbounded, incoming bounded inside it",
			existing: Since(t0),
			incoming: Between(t2, t3),
			want: []string{
				"[2026-01-01T00:00:00Z, 2026-03-01T00:00:00Z)=old",
				"[2026-03-01T00:00:00Z, 2026-04-01T00:00:00Z)=new",
				"[2026-04-01T00:00:00Z, ∞)=old",
			},
		},
		{
			name:     "existing unbounded, incoming bounded and starting earlier",
			existing: Since(t2),
			incoming: Between(t0, t3),
			want: []string{
				"[2026-01-01T00:00:00Z, 2026-04-01T00:00:00Z)=new",
				"[2026-04-01T00:00:00Z, ∞)=old",
			},
		},
		{
			name:     "existing bounded, incoming unbounded from inside it",
			existing: Between(t0, t3),
			incoming: Since(t2),
			want: []string{
				"[2026-01-01T00:00:00Z, 2026-03-01T00:00:00Z)=old",
				"[2026-03-01T00:00:00Z, ∞)=new",
			},
		},
		{
			name:     "existing bounded, incoming unbounded from before it",
			existing: Between(t2, t3),
			incoming: Since(t0),
			want:     []string{"[2026-01-01T00:00:00Z, ∞)=new"},
		},
		{
			name:     "both unbounded, same start",
			existing: Since(t1),
			incoming: Since(t1),
			want:     []string{"[2026-02-01T00:00:00Z, ∞)=new"},
		},
		{
			name:     "both unbounded, incoming starts later",
			existing: Since(t1),
			incoming: Since(t3),
			want: []string{
				"[2026-02-01T00:00:00Z, 2026-04-01T00:00:00Z)=old",
				"[2026-04-01T00:00:00Z, ∞)=new",
			},
		},
		{
			name:     "both unbounded, incoming starts earlier",
			existing: Since(t3),
			incoming: Since(t1),
			want:     []string{"[2026-02-01T00:00:00Z, ∞)=new"},
		},
		{
			name:     "existing unbounded at the start, incoming bounded inside",
			existing: Until(t5),
			incoming: Between(t1, t3),
			want: []string{
				"[-∞, 2026-02-01T00:00:00Z)=old",
				"[2026-02-01T00:00:00Z, 2026-04-01T00:00:00Z)=new",
				"[2026-04-01T00:00:00Z, 2026-06-01T00:00:00Z)=old",
			},
		},
		{
			name:     "existing covers all of time, incoming bounded inside",
			existing: Always(),
			incoming: Between(t1, t3),
			want: []string{
				"[-∞, 2026-02-01T00:00:00Z)=old",
				"[2026-02-01T00:00:00Z, 2026-04-01T00:00:00Z)=new",
				"[2026-04-01T00:00:00Z, ∞)=old",
			},
		},
		{
			name:     "incoming covers all of time",
			existing: Between(t1, t3),
			incoming: Always(),
			want:     []string{"[-∞, ∞)=new"},
		},
		{
			name:     "existing unbounded at the start, incoming unbounded at the start too",
			existing: Until(t5),
			incoming: Until(t2),
			want: []string{
				"[-∞, 2026-03-01T00:00:00Z)=new",
				"[2026-03-01T00:00:00Z, 2026-06-01T00:00:00Z)=old",
			},
		},
	}

	for _, tc := range cases {
		t.Run("given an existing record and an incoming write that are "+tc.name, func(t *testing.T) {
			l, store, _ := newTestLog(t)
			if _, err := l.PutInterval(ctx, employee, "e", []byte("old"), tc.existing, alice); err != nil {
				t.Fatalf("writing the existing record failed: %v", err)
			}
			if _, err := l.PutInterval(ctx, employee, "e", []byte("new"), tc.incoming, bob); err != nil {
				t.Fatalf("writing the incoming record failed: %v", err)
			}

			t.Run("when the current tiling is read", func(t *testing.T) {
				got := currentSegments(t, l, "e")
				t.Run("then the intervals are split as expected", func(t *testing.T) {
					if !equalStrings(got, tc.want) {
						t.Fatalf("current tiling:\n got %v\nwant %v", got, tc.want)
					}
				})
				t.Run("then no current intervals overlap", func(t *testing.T) {
					assertInvariants(t, store)
				})
			})
		})
	}
}

func TestPutRemainderAttribution(t *testing.T) {
	t.Run("given an existing record split by a later writer", func(t *testing.T) {
		ctx := context.Background()
		l, _, _ := newTestLog(t)

		if _, err := l.Put(ctx, employee, "e", []byte(`{"v":1}`), t0, time.Time{}, alice, WithReason("hired"), WithMetaValue("src", "hr")); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
		res, err := l.Put(ctx, employee, "e", []byte(`{"v":2}`), t2, t3, bob)
		if err != nil {
			t.Fatalf("Put failed: %v", err)
		}

		t.Run("when the remainders are examined", func(t *testing.T) {
			var remainders []Record
			for _, r := range res.Written {
				if r.Intent == IntentRemainder {
					remainders = append(remainders, r)
				}
			}

			t.Run("then both uncovered parts were preserved", func(t *testing.T) {
				if len(remainders) != 2 {
					t.Fatalf("got %d remainders; want 2", len(remainders))
				}
			})
			t.Run("then they carry the superseded record's data", func(t *testing.T) {
				for _, r := range remainders {
					if string(r.Data) != `{"v":1}` {
						t.Fatalf("remainder data = %s; want the superseded record's data", r.Data)
					}
				}
			})
			t.Run("then they are attributed to the original author, not the splitter", func(t *testing.T) {
				for _, r := range remainders {
					if r.Actor.ID != alice.ID {
						t.Fatalf("remainder attributed to %s; want the original author %s — "+
							"stamping the splitter on it would claim they asserted data they never sent",
							r.Actor.ID, alice.ID)
					}
				}
			})
			t.Run("then they carry the original reason and metadata", func(t *testing.T) {
				for _, r := range remainders {
					if r.Reason != "hired" || r.Meta["src"] != "hr" {
						t.Fatalf("remainder lost the original reason/metadata: %q %v", r.Reason, r.Meta)
					}
				}
			})
			t.Run("then the splitter is still identifiable at the same transaction instant", func(t *testing.T) {
				recs, err := l.History(ctx, employee, "e", InTxRange(Between(res.TxAt, res.TxAt.Add(time.Nanosecond))), WithIntent(IntentAssert))
				if err != nil {
					t.Fatalf("History failed: %v", err)
				}
				var found bool
				for _, r := range recs {
					if r.TxFrom.Equal(res.TxAt) && r.Actor.ID == bob.ID {
						found = true
					}
				}
				if !found {
					t.Fatal("could not identify the actor who caused the split from the transaction instant")
				}
			})
		})
	})
}

func TestPutSupersessionIsNonDestructive(t *testing.T) {
	t.Run("given a record superseded by a later write", func(t *testing.T) {
		ctx := context.Background()
		l, store, clock := newTestLog(t)

		clock.Set(t1)
		first := mustPut(t, l, "e", "v1", t0, time.Time{})
		clock.Set(t3)
		second := mustPut(t, l, "e", "v2", t2, time.Time{})

		t.Run("when the superseded record is inspected", func(t *testing.T) {
			t.Run("then it still exists", func(t *testing.T) {
				if store.Len() < 2 {
					t.Fatalf("store holds %d records; supersession must not delete", store.Len())
				}
			})
			t.Run("then its transaction interval was closed at the superseding write", func(t *testing.T) {
				recs, err := l.History(ctx, employee, "e")
				if err != nil {
					t.Fatalf("History failed: %v", err)
				}
				var closed int
				for _, r := range recs {
					if r.ID == first.Record.ID {
						if r.IsCurrent() {
							t.Fatal("the superseded record is still current")
						}
						if !r.TxTo.Equal(second.TxAt) {
							t.Fatalf("TxTo = %s; want the superseding write's instant %s", r.TxTo, second.TxAt)
						}
						closed++
					}
				}
				if closed != 1 {
					t.Fatalf("found the original record %d times; want once", closed)
				}
			})
			t.Run("then its transaction interval is strictly positive", func(t *testing.T) {
				assertInvariants(t, store)
			})
		})

		t.Run("when the supersession is reported to the writer", func(t *testing.T) {
			t.Run("then the result names the record it closed", func(t *testing.T) {
				if len(second.Superseded) != 1 || second.Superseded[0] != first.Record.ID {
					t.Fatalf("Superseded = %v; want [%s]", second.Superseded, first.Record.ID)
				}
			})
		})
	})
}

// ---------------------------------------------------------------------------
// validation at the API boundary
// ---------------------------------------------------------------------------

func TestWriteValidation(t *testing.T) {
	ctx := context.Background()

	t.Run("given a write with no actor", func(t *testing.T) {
		l, _, _ := newTestLog(t)
		t.Run("when it is attempted", func(t *testing.T) {
			t.Run("then it fails with ErrMissingActor", func(t *testing.T) {
				_, err := l.Put(ctx, employee, "e", []byte("x"), t0, t1, Actor{})
				if !errors.Is(err, ErrMissingActor) {
					t.Fatalf("Put with no actor = %v; want ErrMissingActor", err)
				}
			})
			t.Run("then an actor with only a display name is still rejected", func(t *testing.T) {
				_, err := l.Put(ctx, employee, "e", []byte("x"), t0, t1, Actor{Name: "system"})
				if !errors.Is(err, ErrMissingActor) {
					t.Fatalf("Put with a nameless-but-unidentified actor = %v; want ErrMissingActor", err)
				}
			})
			t.Run("then Correct rejects it too", func(t *testing.T) {
				_, err := l.Correct(ctx, employee, "e", []byte("x"), t0, t1, Actor{})
				if !errors.Is(err, ErrMissingActor) {
					t.Fatalf("Correct with no actor = %v; want ErrMissingActor", err)
				}
			})
			t.Run("then nothing was written", func(t *testing.T) {
				recs, err := l.History(ctx, employee, "e")
				if err != nil {
					t.Fatalf("History failed: %v", err)
				}
				if len(recs) != 0 {
					t.Fatalf("a rejected write left %d records behind", len(recs))
				}
			})
		})
	})

	t.Run("given a write with a malformed valid interval", func(t *testing.T) {
		l, _, _ := newTestLog(t)
		cases := []struct {
			name     string
			from, to time.Time
		}{
			{"empty (zero width)", t1, t1},
			{"inverted", t3, t1},
			{"inverted by a nanosecond", t1.Add(time.Nanosecond), t1},
		}
		for _, tc := range cases {
			t.Run("when the interval is "+tc.name, func(t *testing.T) {
				t.Run("then it fails with ErrInvalidInterval", func(t *testing.T) {
					_, err := l.Put(ctx, employee, "e", []byte("x"), tc.from, tc.to, alice)
					if !errors.Is(err, ErrInvalidInterval) {
						t.Fatalf("Put(%s) = %v; want ErrInvalidInterval", Between(tc.from, tc.to), err)
					}
					var ie *IntervalError
					if !errors.As(err, &ie) || ie.Field != "valid" {
						t.Fatalf("error = %v; want an *IntervalError naming the valid axis", err)
					}
				})
			})
		}
	})

	t.Run("given a write with no kind", func(t *testing.T) {
		l, _, _ := newTestLog(t)
		t.Run("when it is attempted", func(t *testing.T) {
			t.Run("then it fails with ErrUnknownKind", func(t *testing.T) {
				_, err := l.Put(ctx, "", "e", []byte("x"), t0, t1, alice)
				if !errors.Is(err, ErrUnknownKind) {
					t.Fatalf("Put with no kind = %v; want ErrUnknownKind", err)
				}
			})
		})
	})

	t.Run("given a log restricted to a set of kinds", func(t *testing.T) {
		l, _, _ := newTestLog(t, WithKinds(employee, "invoice"))
		t.Run("when a registered kind is written", func(t *testing.T) {
			t.Run("then it succeeds", func(t *testing.T) {
				if _, err := l.Put(ctx, "invoice", "i-1", []byte("x"), t0, t1, alice); err != nil {
					t.Fatalf("Put with a registered kind failed: %v", err)
				}
			})
		})
		t.Run("when an unregistered kind is written", func(t *testing.T) {
			t.Run("then it fails with ErrUnknownKind naming the kind", func(t *testing.T) {
				_, err := l.Put(ctx, "porcupine", "p-1", []byte("x"), t0, t1, alice)
				if !errors.Is(err, ErrUnknownKind) {
					t.Fatalf("Put with an unregistered kind = %v; want ErrUnknownKind", err)
				}
				var ke *KindError
				if !errors.As(err, &ke) || ke.Kind != "porcupine" {
					t.Fatalf("error = %v; want a *KindError naming porcupine", err)
				}
			})
		})
		t.Run("when an unregistered kind is read", func(t *testing.T) {
			t.Run("then every read path rejects it", func(t *testing.T) {
				if _, err := l.Get(ctx, "porcupine", "p-1", Now()); !errors.Is(err, ErrUnknownKind) {
					t.Fatalf("Get = %v; want ErrUnknownKind", err)
				}
				if _, err := l.History(ctx, "porcupine", "p-1"); !errors.Is(err, ErrUnknownKind) {
					t.Fatalf("History = %v; want ErrUnknownKind", err)
				}
				if _, err := l.Timeline(ctx, "porcupine", "p-1", Now()); !errors.Is(err, ErrUnknownKind) {
					t.Fatalf("Timeline = %v; want ErrUnknownKind", err)
				}
				if _, err := l.Diff(ctx, "porcupine", "p-1", Now(), Now()); !errors.Is(err, ErrUnknownKind) {
					t.Fatalf("Diff = %v; want ErrUnknownKind", err)
				}
				if _, _, err := l.Query(ctx, Query{Kind: "porcupine"}); !errors.Is(err, ErrUnknownKind) {
					t.Fatalf("Query = %v; want ErrUnknownKind", err)
				}
			})
		})
		t.Run("when an empty kind is passed to WithKinds", func(t *testing.T) {
			t.Run("then it is ignored rather than registered", func(t *testing.T) {
				l2, _, _ := newTestLog(t, WithKinds("", "thing"))
				if _, err := l2.Put(ctx, "", "e", []byte("x"), t0, t1, alice); !errors.Is(err, ErrUnknownKind) {
					t.Fatalf("empty kind = %v; want ErrUnknownKind", err)
				}
			})
		})
	})

	t.Run("given a write with no entity ID", func(t *testing.T) {
		l, _, _ := newTestLog(t)
		t.Run("when it is attempted", func(t *testing.T) {
			t.Run("then it fails rather than writing to a phantom entity", func(t *testing.T) {
				_, err := l.Put(ctx, employee, "", []byte("x"), t0, t1, alice)
				if !errors.Is(err, ErrMissingEntityID) {
					t.Fatalf("Put with no entity ID = %v; want ErrMissingEntityID", err)
				}
			})
			t.Run("then the read paths reject it too", func(t *testing.T) {
				if _, err := l.Get(ctx, employee, "", Now()); !errors.Is(err, ErrMissingEntityID) {
					t.Fatalf("Get = %v; want ErrMissingEntityID", err)
				}
				if _, err := l.History(ctx, employee, ""); !errors.Is(err, ErrMissingEntityID) {
					t.Fatalf("History = %v; want ErrMissingEntityID", err)
				}
				if _, err := l.Timeline(ctx, employee, "", Now()); !errors.Is(err, ErrMissingEntityID) {
					t.Fatalf("Timeline = %v; want ErrMissingEntityID", err)
				}
				if _, err := l.Diff(ctx, employee, "", Now(), Now()); !errors.Is(err, ErrMissingEntityID) {
					t.Fatalf("Diff = %v; want ErrMissingEntityID", err)
				}
			})
		})
	})

	t.Run("given a cancelled context", func(t *testing.T) {
		l, _, _ := newTestLog(t)
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()

		t.Run("when any operation is attempted", func(t *testing.T) {
			t.Run("then it reports the context error", func(t *testing.T) {
				if _, err := l.Put(cancelled, employee, "e", []byte("x"), t0, t1, alice); !errors.Is(err, context.Canceled) {
					t.Fatalf("Put = %v; want context.Canceled", err)
				}
				if _, err := l.Correct(cancelled, employee, "e", []byte("x"), t0, t1, alice); !errors.Is(err, context.Canceled) {
					t.Fatalf("Correct = %v; want context.Canceled", err)
				}
				if _, err := l.Get(cancelled, employee, "e", Now()); !errors.Is(err, context.Canceled) {
					t.Fatalf("Get = %v; want context.Canceled", err)
				}
				if _, err := l.History(cancelled, employee, "e"); !errors.Is(err, context.Canceled) {
					t.Fatalf("History = %v; want context.Canceled", err)
				}
				if _, err := l.Timeline(cancelled, employee, "e", Now()); !errors.Is(err, context.Canceled) {
					t.Fatalf("Timeline = %v; want context.Canceled", err)
				}
				if _, err := l.Diff(cancelled, employee, "e", Now(), Now()); !errors.Is(err, context.Canceled) {
					t.Fatalf("Diff = %v; want context.Canceled", err)
				}
				if _, _, err := l.Query(cancelled, Query{}); !errors.Is(err, context.Canceled) {
					t.Fatalf("Query = %v; want context.Canceled", err)
				}
			})
		})
	})

	t.Run("given a nil store", func(t *testing.T) {
		t.Run("when a log is constructed over it", func(t *testing.T) {
			t.Run("then it panics rather than degrading silently", func(t *testing.T) {
				defer func() {
					if recover() == nil {
						t.Fatal("NewLog(nil) did not panic")
					}
				}()
				NewLog(nil)
			})
		})
	})
}

// ---------------------------------------------------------------------------
// transaction time
// ---------------------------------------------------------------------------

func TestTransactionTime(t *testing.T) {
	ctx := context.Background()

	t.Run("given a clock frozen at a single instant", func(t *testing.T) {
		l, store, _ := newTestLog(t)

		var results []Result
		for i := 0; i < 5; i++ {
			results = append(results, mustPut(t, l, "e", fmt.Sprintf("v%d", i), t1, time.Time{}))
		}

		t.Run("when several writes land in the same nanosecond", func(t *testing.T) {
			t.Run("then transaction time still advances strictly", func(t *testing.T) {
				for i := 1; i < len(results); i++ {
					if !results[i].TxAt.After(results[i-1].TxAt) {
						t.Fatalf("write %d got tx %s, not after write %d's %s — the ratchet failed",
							i, results[i].TxAt, i-1, results[i-1].TxAt)
					}
				}
			})
			t.Run("then it advances by exactly one nanosecond per write", func(t *testing.T) {
				for i := 1; i < len(results); i++ {
					if d := results[i].TxAt.Sub(results[i-1].TxAt); d != time.Nanosecond {
						t.Fatalf("tx advanced by %v between writes; want 1ns", d)
					}
				}
			})
			t.Run("then every superseded record has a non-empty transaction interval", func(t *testing.T) {
				assertInvariants(t, store)
			})
			t.Run("then each intermediate belief is separately observable", func(t *testing.T) {
				for i, res := range results {
					rec, err := l.Get(ctx, employee, "e", As{ValidAt: t1, TxAt: res.TxAt})
					if err != nil {
						t.Fatalf("Get at tx %s failed: %v", res.TxAt, err)
					}
					if want := fmt.Sprintf("v%d", i); string(rec.Data) != want {
						t.Fatalf("belief at tx %s = %s; want %s", res.TxAt, rec.Data, want)
					}
				}
			})
			t.Run("then the current read sees the last write", func(t *testing.T) {
				// ValidAt is pinned because the frozen clock sits before the
				// records' valid start; TxAt is left to default, which is what
				// this assertion is about.
				rec, err := l.Get(ctx, employee, "e", As{ValidAt: t1})
				if err != nil {
					t.Fatalf("Get(now) failed: %v", err)
				}
				if string(rec.Data) != "v4" {
					t.Fatalf("Get(now) = %s; want v4 — a read must not sit behind the ratchet", rec.Data)
				}
			})
		})
	})

	t.Run("given a clock that goes backwards", func(t *testing.T) {
		l, store, clock := newTestLog(t)
		clock.Set(t3)
		first := mustPut(t, l, "e", "v1", t1, time.Time{})
		clock.Set(t0) // the clock jumps back a quarter
		second := mustPut(t, l, "e", "v2", t1, time.Time{})

		t.Run("when the second write is made", func(t *testing.T) {
			t.Run("then transaction time still moves forward", func(t *testing.T) {
				if !second.TxAt.After(first.TxAt) {
					t.Fatalf("tx went backwards: %s then %s — an injected clock must not be able to rewind the axis",
						first.TxAt, second.TxAt)
				}
			})
			t.Run("then the invariants still hold", func(t *testing.T) {
				assertInvariants(t, store)
			})
		})
	})

	t.Run("given a write with times in a non-UTC zone", func(t *testing.T) {
		l, _, _ := newTestLog(t)
		zone := time.FixedZone("test", -7*3600)
		res := mustPut(t, l, "e", "v", t1.In(zone), t3.In(zone))

		t.Run("when the record is read back", func(t *testing.T) {
			t.Run("then all four timestamps are stored in UTC", func(t *testing.T) {
				r := res.Record
				for name, tv := range map[string]time.Time{"ValidFrom": r.ValidFrom, "ValidTo": r.ValidTo, "TxFrom": r.TxFrom} {
					if tv.Location() != time.UTC {
						t.Fatalf("%s stored in %v; want UTC", name, tv.Location())
					}
				}
			})
			t.Run("then the instants are unchanged", func(t *testing.T) {
				if !res.Record.ValidFrom.Equal(t1) || !res.Record.ValidTo.Equal(t3) {
					t.Fatalf("normalising to UTC changed the instants: %s", res.Record.Valid())
				}
			})
		})
	})

	t.Run("given a log using the default clock", func(t *testing.T) {
		l := NewLog(NewMemStore())
		t.Run("when a write is made", func(t *testing.T) {
			before := time.Now().UTC()
			res, err := l.Put(ctx, employee, "e", []byte("x"), t0, t1, alice)
			if err != nil {
				t.Fatalf("Put failed: %v", err)
			}
			after := time.Now().UTC()
			t.Run("then transaction time comes from the wall clock in UTC", func(t *testing.T) {
				if res.TxAt.Before(before) || res.TxAt.After(after.Add(time.Second)) {
					t.Fatalf("TxAt %s is outside [%s, %s]", res.TxAt, before, after)
				}
				if res.TxAt.Location() != time.UTC {
					t.Fatalf("TxAt in %v; want UTC", res.TxAt.Location())
				}
			})
		})
	})

	t.Run("given options that are nil", func(t *testing.T) {
		t.Run("when a log is constructed", func(t *testing.T) {
			t.Run("then the defaults survive", func(t *testing.T) {
				l := NewLog(NewMemStore(), WithClock(nil), WithCodec(nil))
				if l.clock == nil || l.codec == nil {
					t.Fatal("a nil option overwrote a default")
				}
				if l.Codec().Name() != "json" {
					t.Fatalf("default codec = %q; want json", l.Codec().Name())
				}
			})
		})
	})
}

func TestFixedClock(t *testing.T) {
	t.Run("given a fixed clock", func(t *testing.T) {
		c := NewFixedClock(t1)
		t.Run("when it is read", func(t *testing.T) {
			t.Run("then it reports its instant", func(t *testing.T) {
				if !c.Now().Equal(t1) {
					t.Fatalf("Now() = %s; want %s", c.Now(), t1)
				}
			})
		})
		t.Run("when it is advanced", func(t *testing.T) {
			t.Run("then it reports the later instant", func(t *testing.T) {
				got := c.Advance(24 * time.Hour)
				if !got.Equal(t1.Add(24*time.Hour)) || !c.Now().Equal(got) {
					t.Fatalf("Advance = %s; want %s", got, t1.Add(24*time.Hour))
				}
			})
		})
		t.Run("when it is set", func(t *testing.T) {
			t.Run("then it reports the new instant", func(t *testing.T) {
				c.Set(t4)
				if !c.Now().Equal(t4) {
					t.Fatalf("Now() = %s; want %s", c.Now(), t4)
				}
			})
		})
	})

	t.Run("given a ClockFunc", func(t *testing.T) {
		t.Run("when it is used as a Clock", func(t *testing.T) {
			t.Run("then it reports what the function returns", func(t *testing.T) {
				var c Clock = ClockFunc(func() time.Time { return t2 })
				if !c.Now().Equal(t2) {
					t.Fatalf("Now() = %s; want %s", c.Now(), t2)
				}
			})
		})
	})
}

// ---------------------------------------------------------------------------
// reads
// ---------------------------------------------------------------------------

func TestGet(t *testing.T) {
	ctx := context.Background()

	t.Run("given an entity with an unbounded current record", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		clock.Set(t1)
		mustPut(t, l, "e", "v1", t1, time.Time{})

		t.Run("when read at an instant inside the interval", func(t *testing.T) {
			t.Run("then the record is returned", func(t *testing.T) {
				rec, err := l.Get(ctx, employee, "e", As{ValidAt: t5})
				if err != nil {
					t.Fatalf("Get failed: %v", err)
				}
				if string(rec.Data) != "v1" {
					t.Fatalf("Get = %s; want v1", rec.Data)
				}
			})
			t.Run("then an unbounded end really is unbounded", func(t *testing.T) {
				far := t5.AddDate(500, 0, 0)
				if _, err := l.Get(ctx, employee, "e", As{ValidAt: far}); err != nil {
					t.Fatalf("Get far in the future failed: %v — a zero ValidTo must mean unbounded", err)
				}
			})
		})

		t.Run("when read before the interval began", func(t *testing.T) {
			t.Run("then it reports not found", func(t *testing.T) {
				_, err := l.Get(ctx, employee, "e", As{ValidAt: t0})
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("Get before the record = %v; want ErrNotFound", err)
				}
				var nfe *NotFoundError
				if !errors.As(err, &nfe) || nfe.EntityID != "e" {
					t.Fatalf("error = %v; want a *NotFoundError naming the entity", err)
				}
				if nfe.Error() == "" {
					t.Fatal("NotFoundError.Error() is empty")
				}
			})
		})

		t.Run("when read before it was written on the transaction axis", func(t *testing.T) {
			t.Run("then it reports not found, because we did not know yet", func(t *testing.T) {
				_, err := l.Get(ctx, employee, "e", As{ValidAt: t5, TxAt: t0})
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("Get before the write = %v; want ErrNotFound", err)
				}
			})
		})

		t.Run("when an unknown entity is read", func(t *testing.T) {
			t.Run("then it reports not found", func(t *testing.T) {
				_, err := l.Get(ctx, employee, "nobody", Now())
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("Get of an unknown entity = %v; want ErrNotFound", err)
				}
			})
		})
	})

	t.Run("given a record with an unbounded valid start", func(t *testing.T) {
		l, _, _ := newTestLog(t)
		if _, err := l.PutInterval(ctx, employee, "e", []byte("always"), Until(t3), alice); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
		t.Run("when read at an arbitrarily early instant", func(t *testing.T) {
			t.Run("then the record is returned", func(t *testing.T) {
				rec, err := l.Get(ctx, employee, "e", As{ValidAt: time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)})
				if err != nil {
					t.Fatalf("Get = %v; a zero ValidFrom must mean unbounded", err)
				}
				if string(rec.Data) != "always" {
					t.Fatalf("Get = %s; want always", rec.Data)
				}
			})
		})
	})

	t.Run("given a record returned to the caller", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		clock.Set(t2)
		mustPut(t, l, "e", `{"v":1}`, t1, time.Time{})

		t.Run("when the caller mutates it", func(t *testing.T) {
			rec, err := l.Get(ctx, employee, "e", Now())
			if err != nil {
				t.Fatalf("Get failed: %v", err)
			}
			rec.Data[0] = 'X'
			rec.Meta = map[string]string{"tampered": "yes"}

			t.Run("then the log is unaffected", func(t *testing.T) {
				again, err := l.Get(ctx, employee, "e", Now())
				if err != nil {
					t.Fatalf("Get failed: %v", err)
				}
				if string(again.Data) != `{"v":1}` {
					t.Fatalf("stored data was mutated through a returned record: %s", again.Data)
				}
			})
		})
	})

	t.Run("given data the caller still holds a reference to", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		clock.Set(t2)
		data := []byte(`{"v":1}`)
		meta := map[string]string{"k": "v"}
		if _, err := l.Put(ctx, employee, "e", data, t1, time.Time{}, alice, WithMeta(meta)); err != nil {
			t.Fatalf("Put failed: %v", err)
		}

		t.Run("when the caller mutates it after the write", func(t *testing.T) {
			data[2] = 'X'
			meta["k"] = "tampered"

			t.Run("then the stored record is unaffected", func(t *testing.T) {
				rec, err := l.Get(ctx, employee, "e", Now())
				if err != nil {
					t.Fatalf("Get failed: %v", err)
				}
				if string(rec.Data) != `{"v":1}` {
					t.Fatalf("stored data = %s; the log must copy on write", rec.Data)
				}
				if rec.Meta["k"] != "v" {
					t.Fatalf("stored meta = %v; the log must copy metadata on write", rec.Meta)
				}
			})
		})
	})
}

func TestTimeline(t *testing.T) {
	ctx := context.Background()

	t.Run("given an entity whose timeline was retiled by a correction", func(t *testing.T) {
		l, _, clock := newTestLog(t)

		clock.Set(t1)
		before := mustPut(t, l, "e", "v1", t1, time.Time{}).TxAt
		clock.Set(t4)
		after := mustCorrect(t, l, "e", "v2", t2, t3).TxAt

		t.Run("when read at the earlier belief instant", func(t *testing.T) {
			recs, err := l.Timeline(ctx, employee, "e", As{TxAt: before})
			if err != nil {
				t.Fatalf("Timeline failed: %v", err)
			}
			t.Run("then it shows the original single segment", func(t *testing.T) {
				if len(recs) != 1 || string(recs[0].Data) != "v1" {
					t.Fatalf("timeline at %s = %d records; want the original single segment", before, len(recs))
				}
			})
		})

		t.Run("when read at the later belief instant", func(t *testing.T) {
			recs, err := l.Timeline(ctx, employee, "e", As{TxAt: after})
			if err != nil {
				t.Fatalf("Timeline failed: %v", err)
			}
			t.Run("then it shows the retiled segments in valid-time order", func(t *testing.T) {
				want := []string{"v1", "v2", "v1"}
				if len(recs) != len(want) {
					t.Fatalf("timeline has %d segments; want %d", len(recs), len(want))
				}
				for i, r := range recs {
					if string(r.Data) != want[i] {
						t.Fatalf("segment %d = %s; want %s", i, r.Data, want[i])
					}
				}
			})
			t.Run("then the segments are contiguous and non-overlapping", func(t *testing.T) {
				for i := 1; i < len(recs); i++ {
					if !recs[i-1].ValidTo.Equal(recs[i].ValidFrom) {
						t.Fatalf("segment %d ends at %s but %d starts at %s — the tiling has a hole",
							i-1, recs[i-1].ValidTo, i, recs[i].ValidFrom)
					}
				}
			})
		})

		t.Run("when read with no transaction instant", func(t *testing.T) {
			t.Run("then it defaults to current belief", func(t *testing.T) {
				recs, err := l.Timeline(ctx, employee, "e", Now())
				if err != nil {
					t.Fatalf("Timeline failed: %v", err)
				}
				if len(recs) != 3 {
					t.Fatalf("timeline at now has %d segments; want 3", len(recs))
				}
			})
		})

		t.Run("when a valid instant is also supplied", func(t *testing.T) {
			t.Run("then it is ignored rather than narrowing the result", func(t *testing.T) {
				recs, err := l.Timeline(ctx, employee, "e", As{ValidAt: t2, TxAt: after})
				if err != nil {
					t.Fatalf("Timeline failed: %v", err)
				}
				if len(recs) != 3 {
					t.Fatalf("timeline with a ValidAt = %d segments; want all 3", len(recs))
				}
			})
		})

		t.Run("when an unknown entity is read", func(t *testing.T) {
			t.Run("then the timeline is empty rather than an error", func(t *testing.T) {
				recs, err := l.Timeline(ctx, employee, "nobody", Now())
				if err != nil {
					t.Fatalf("Timeline of an unknown entity = %v; want no error", err)
				}
				if len(recs) != 0 {
					t.Fatalf("timeline of an unknown entity has %d records; want 0", len(recs))
				}
			})
		})
	})
}

func TestHistory(t *testing.T) {
	ctx := context.Background()

	t.Run("given an entity with several versions and actors", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		clock.Set(t1)
		mustPut(t, l, "e", "v1", t1, time.Time{})
		clock.Set(t2)
		mustCorrect(t, l, "e", "v2", t1, time.Time{})
		clock.Set(t3)
		mustPut(t, l, "e", "v3", t3, time.Time{})

		t.Run("when the full history is read", func(t *testing.T) {
			recs, err := l.History(ctx, employee, "e")
			if err != nil {
				t.Fatalf("History failed: %v", err)
			}
			t.Run("then superseded versions are included", func(t *testing.T) {
				if len(recs) != 4 {
					t.Fatalf("history has %d records; want 4 (three writes plus one remainder)", len(recs))
				}
			})
			t.Run("then it is ordered by transaction time", func(t *testing.T) {
				for i := 1; i < len(recs); i++ {
					if recs[i].TxFrom.Before(recs[i-1].TxFrom) {
						t.Fatal("history is not ordered by transaction time")
					}
				}
			})
		})

		t.Run("when filtered to current belief only", func(t *testing.T) {
			t.Run("then superseded versions are excluded", func(t *testing.T) {
				recs, err := l.History(ctx, employee, "e", CurrentOnly())
				if err != nil {
					t.Fatalf("History failed: %v", err)
				}
				for _, r := range recs {
					if !r.IsCurrent() {
						t.Fatalf("record %s is superseded but was returned by CurrentOnly", r.ID)
					}
				}
			})
		})

		t.Run("when filtered by actor", func(t *testing.T) {
			t.Run("then only that actor's writes are returned", func(t *testing.T) {
				recs, err := l.History(ctx, employee, "e", ByActor(bob.ID))
				if err != nil {
					t.Fatalf("History failed: %v", err)
				}
				if len(recs) == 0 {
					t.Fatal("filtering by actor returned nothing")
				}
				for _, r := range recs {
					if r.Actor.ID != bob.ID {
						t.Fatalf("record attributed to %s leaked past the actor filter", r.Actor.ID)
					}
				}
			})
		})

		t.Run("when filtered by valid range", func(t *testing.T) {
			t.Run("then only overlapping records are returned", func(t *testing.T) {
				recs, err := l.History(ctx, employee, "e", InValidRange(Between(t4, t5)))
				if err != nil {
					t.Fatalf("History failed: %v", err)
				}
				for _, r := range recs {
					if !r.Valid().Overlaps(Between(t4, t5)) {
						t.Fatalf("record %s (%s) does not overlap the filter", r.ID, r.Valid())
					}
				}
			})
		})

		t.Run("when filtered by transaction range", func(t *testing.T) {
			t.Run("then only beliefs held in that window are returned", func(t *testing.T) {
				recs, err := l.History(ctx, employee, "e", InTxRange(Between(t1, t2)))
				if err != nil {
					t.Fatalf("History failed: %v", err)
				}
				for _, r := range recs {
					if !r.Tx().Overlaps(Between(t1, t2)) {
						t.Fatalf("record %s (tx %s) does not overlap the filter", r.ID, r.Tx())
					}
				}
			})
		})

		t.Run("when limited", func(t *testing.T) {
			t.Run("then at most that many records are returned", func(t *testing.T) {
				recs, err := l.History(ctx, employee, "e", Limit(2))
				if err != nil {
					t.Fatalf("History failed: %v", err)
				}
				if len(recs) != 2 {
					t.Fatalf("Limit(2) returned %d records", len(recs))
				}
			})
		})

		t.Run("when reversed", func(t *testing.T) {
			t.Run("then the newest belief comes first", func(t *testing.T) {
				recs, err := l.History(ctx, employee, "e", Descending())
				if err != nil {
					t.Fatalf("History failed: %v", err)
				}
				for i := 1; i < len(recs); i++ {
					if recs[i].TxFrom.After(recs[i-1].TxFrom) {
						t.Fatal("Descending did not reverse the order")
					}
				}
			})
		})

		t.Run("when an option tries to change the entity", func(t *testing.T) {
			t.Run("then the entity argument still wins", func(t *testing.T) {
				recs, err := l.History(ctx, employee, "e", func(q *Query) { q.EntityID = "someone-else" })
				if err != nil {
					t.Fatalf("History failed: %v", err)
				}
				for _, r := range recs {
					if r.EntityID != "e" {
						t.Fatalf("history for %q leaked into a request for %q", r.EntityID, "e")
					}
				}
			})
		})

		t.Run("when an unknown entity is read", func(t *testing.T) {
			t.Run("then the history is empty rather than an error", func(t *testing.T) {
				recs, err := l.History(ctx, employee, "nobody")
				if err != nil {
					t.Fatalf("History of an unknown entity = %v; want no error", err)
				}
				if len(recs) != 0 {
					t.Fatalf("got %d records; want 0", len(recs))
				}
			})
		})
	})
}

// ---------------------------------------------------------------------------
// concurrency
// ---------------------------------------------------------------------------

func TestConcurrentWrites(t *testing.T) {
	t.Run("given many goroutines writing and reading the same log", func(t *testing.T) {
		ctx := context.Background()
		store := NewMemStore()
		l := NewLog(store)

		const writers, entities, perWriter = 8, 4, 40

		var wg sync.WaitGroup
		errCh := make(chan error, writers*perWriter)

		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < perWriter; i++ {
					id := fmt.Sprintf("e-%d", i%entities)
					from := t0.AddDate(0, 0, (w*perWriter+i)%90)
					to := from.AddDate(0, 0, 1+((w+i)%30))
					_, err := l.Put(ctx, employee, id,
						[]byte(fmt.Sprintf(`{"w":%d,"i":%d}`, w, i)),
						from, to,
						Actor{ID: fmt.Sprintf("w-%d", w)})
					if err != nil {
						errCh <- err
						return
					}
				}
			}(w)
		}

		// Concurrent readers, so that the race detector sees reads racing
		// writes rather than only writes racing writes.
		for r := 0; r < 4; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < perWriter; i++ {
					id := fmt.Sprintf("e-%d", i%entities)
					if _, err := l.History(ctx, employee, id, CurrentOnly()); err != nil {
						errCh <- err
						return
					}
					if _, _, err := l.Query(ctx, Query{Kind: employee, Limit: 10}); err != nil {
						errCh <- err
						return
					}
					if _, err := l.Get(ctx, employee, id, Now()); err != nil && !errors.Is(err, ErrNotFound) {
						errCh <- err
						return
					}
				}
			}()
		}

		wg.Wait()
		close(errCh)

		t.Run("when they have all finished", func(t *testing.T) {
			t.Run("then no operation failed", func(t *testing.T) {
				for err := range errCh {
					t.Fatalf("concurrent operation failed: %v", err)
				}
			})
			t.Run("then every invariant still holds", func(t *testing.T) {
				assertInvariants(t, store)
			})
			t.Run("then no two writes shared a transaction instant", func(t *testing.T) {
				// A write is one caller record (IntentAssert or IntentCorrection)
				// plus its remainders, all stamped with one instant. Two writes
				// sharing an instant would therefore show up as an instant
				// carrying two non-remainder records — which is the actual claim,
				// where the old check of record-ID uniqueness held by
				// construction and proved nothing.
				recs, _, err := store.Query(ctx, Query{})
				if err != nil {
					t.Fatalf("Query failed: %v", err)
				}
				callerRecords := map[int64]int{}
				for _, r := range recs {
					if r.Intent == IntentRemainder {
						continue
					}
					callerRecords[r.TxFrom.UnixNano()]++
				}
				for instant, n := range callerRecords {
					if n != 1 {
						t.Fatalf("transaction instant %s carries %d non-remainder records; want "+
							"exactly 1 — two writes shared an instant, so one superseded record "+
							"has an empty transaction interval no as-of query can see",
							time.Unix(0, instant).UTC(), n)
					}
				}
				if len(callerRecords) != writers*perWriter {
					t.Fatalf("%d distinct write instants for %d writes; every write must get "+
						"its own", len(callerRecords), writers*perWriter)
				}
			})
		})
	})
}
