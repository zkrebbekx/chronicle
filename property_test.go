package chronicle

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// rng is a deterministic xorshift64* generator. Tests use it rather than
// math/rand so that a failure is reproducible from its seed alone, with no
// dependence on the global source, the Go version's shuffling, or the order in
// which tests happen to run.
type rng struct{ s uint64 }

func newRNG(seed uint64) *rng {
	if seed == 0 {
		seed = 0x9E3779B97F4A7C15
	}
	return &rng{s: seed}
}

func (r *rng) next() uint64 {
	x := r.s
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	r.s = x
	return x * 0x2545F4914F6CDD1D
}

func (r *rng) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

// grid is the set of instants the property test draws interval bounds from. A
// small, shared grid is the point: it forces identical, adjacent, nested and
// containing intervals to arise constantly, where bounds drawn from a wide
// range would almost always be disjoint and exercise nothing.
var grid = func() []time.Time {
	out := make([]time.Time, 0, 9)
	for k := 0; k < 9; k++ {
		out = append(out, t0.AddDate(0, 0, 30*k))
	}
	return out
}()

// probes are the instants at which valid-time coverage is checked: every grid
// point, the midpoint of every gap between them, and instants far outside the
// grid in both directions so that unbounded ends are actually probed.
var probes = func() []time.Time {
	out := []time.Time{
		t0.AddDate(-50, 0, 0),
		t0.AddDate(0, 0, -1),
	}
	for i, g := range grid {
		out = append(out, g)
		if i+1 < len(grid) {
			out = append(out, g.Add(grid[i+1].Sub(g)/2))
		}
	}
	out = append(out, grid[len(grid)-1].AddDate(0, 0, 1), grid[len(grid)-1].AddDate(50, 0, 0))
	return out
}()

// randomInterval draws a valid interval, deliberately including unbounded ends
// (roughly one bound in five) and deliberately including malformed intervals,
// which the caller is expected to assert are rejected.
func (r *rng) randomInterval() Interval {
	var from, to time.Time
	if r.intn(5) != 0 {
		from = grid[r.intn(len(grid))]
	}
	if r.intn(5) != 0 {
		to = grid[r.intn(len(grid))]
	}
	return Interval{From: from, To: to}
}

// coveredProbes returns the set of probe instants covered by some current
// record for the entity.
func coveredProbes(t *testing.T, l *Log, entityID string) map[time.Time]bool {
	t.Helper()
	recs, err := l.History(context.Background(), employee, entityID, CurrentOnly())
	if err != nil {
		t.Fatalf("History failed: %v", err)
	}
	out := map[time.Time]bool{}
	for _, p := range probes {
		for _, rec := range recs {
			if rec.Valid().Contains(p) {
				out[p] = true
				break
			}
		}
	}
	return out
}

// TestPropertyInvariantsHoldOverRandomSequences drives long randomised
// sequences of writes at a small set of entities and asserts, after every
// single step, everything the library promises.
//
// The seeds are a fixed table rather than a time-derived value, so a failure
// here reproduces exactly.
func TestPropertyInvariantsHoldOverRandomSequences(t *testing.T) {
	seeds := []uint64{
		1, 2, 3, 7, 42, 1337, 0xDEADBEEF, 0xC0FFEE, 0x5EED,
		0x9E3779B97F4A7C15, 0xFFFFFFFFFFFFFFFF, 0x0123456789ABCDEF,
	}
	const steps = 220
	entities := []string{"a", "b", "c"}

	for _, seed := range seeds {
		t.Run(fmt.Sprintf("given a randomised write sequence from seed %#x", seed), func(t *testing.T) {
			ctx := context.Background()
			r := newRNG(seed)
			clock := NewFixedClock(t0)
			store := NewMemStore()
			l := NewLog(store, WithClock(clock))

			// everCovered records, per entity, which probe instants have at
			// some point been covered by a current record. Nothing may ever
			// leave this set: a write may retile an entity's valid timeline
			// but must never punch a hole in it.
			everCovered := map[string]map[time.Time]bool{}
			for _, e := range entities {
				everCovered[e] = map[time.Time]bool{}
			}

			var writes, rejected int

			for step := 0; step < steps; step++ {
				entity := entities[r.intn(len(entities))]
				iv := r.randomInterval()
				actor := Actor{ID: fmt.Sprintf("actor-%d", r.intn(3))}
				data := []byte(fmt.Sprintf(`{"step":%d}`, step))

				// Advance the clock sometimes but not always, so that runs of
				// writes share a wall-clock instant and the ratchet is
				// exercised rather than bypassed.
				if r.intn(3) == 0 {
					clock.Advance(time.Duration(1+r.intn(48)) * time.Hour)
				}

				var err error
				if r.intn(4) == 0 {
					_, err = l.CorrectInterval(ctx, employee, entity, data, iv, actor)
				} else {
					_, err = l.PutInterval(ctx, employee, entity, data, iv, actor)
				}

				if wantErr := iv.Validate() != nil; wantErr {
					rejected++
					if !errors.Is(err, ErrInvalidInterval) {
						t.Fatalf("step %d: writing malformed interval %s = %v; want ErrInvalidInterval", step, iv, err)
					}
					// A rejected write must have changed nothing, so the
					// coverage check below still applies unchanged.
				} else {
					writes++
					if err != nil {
						t.Fatalf("step %d: writing %s failed: %v", step, iv, err)
					}
					for _, p := range probes {
						if iv.Contains(p) {
							everCovered[entity][p] = true
						}
					}
				}

				assertInvariants(t, store)

				for _, e := range entities {
					covered := coveredProbes(t, l, e)
					for p := range everCovered[e] {
						if !covered[p] {
							t.Fatalf("step %d: entity %s lost coverage at %s — a write punched a hole "+
								"in a previously covered valid timeline", step, e, p)
						}
					}
				}
			}

			t.Run("when the sequence has run", func(t *testing.T) {
				t.Run("then it exercised both accepted and rejected writes", func(t *testing.T) {
					if writes == 0 {
						t.Fatal("the sequence accepted no writes at all")
					}
					if rejected == 0 {
						t.Fatal("the sequence generated no malformed intervals, so rejection went untested")
					}
				})
				t.Run("then history was never destroyed", func(t *testing.T) {
					if store.Len() < writes {
						t.Fatalf("store holds %d records after %d accepted writes; supersession must not delete",
							store.Len(), writes)
					}
				})
				t.Run("then every past belief instant is still queryable", func(t *testing.T) {
					recs, _, err := store.Query(ctx, Query{})
					if err != nil {
						t.Fatalf("Query failed: %v", err)
					}
					for _, rec := range recs {
						got, err := l.Get(ctx, rec.Kind, rec.EntityID, As{
							ValidAt: pickInstantIn(rec.Valid()),
							TxAt:    rec.TxFrom,
						})
						if err != nil {
							t.Fatalf("record %s (%s, tx %s) is not retrievable at its own coordinates: %v",
								rec.ID, rec.Valid(), rec.Tx(), err)
						}
						if got.ID != rec.ID {
							t.Fatalf("querying at record %s's own coordinates returned %s instead", rec.ID, got.ID)
						}
					}
				})
			})
		})
	}
}

// pickInstantIn returns an instant strictly inside the interval, which the
// interval's own validity guarantees exists.
func pickInstantIn(iv Interval) time.Time {
	switch {
	case !iv.From.IsZero():
		return iv.From
	case !iv.To.IsZero():
		return iv.To.Add(-time.Nanosecond)
	default:
		return t0
	}
}

// FuzzPutSequence drives the write path from fuzzer-supplied bytes and asserts
// the same invariants. It complements the seeded property test rather than
// replacing it: the fuzzer explores shapes the grid generator would not, but
// cannot be relied on to reproduce.
func FuzzPutSequence(f *testing.F) {
	f.Add([]byte{0})
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8})
	f.Add([]byte{0, 0, 0, 0, 255, 255, 255, 255})
	f.Add([]byte{3, 1, 4, 1, 5, 9, 2, 6, 5, 3, 5, 8, 9, 7, 9})
	f.Add([]byte{8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8})

	f.Fuzz(func(t *testing.T, in []byte) {
		if len(in) == 0 {
			return
		}
		ctx := context.Background()
		clock := NewFixedClock(t0)
		store := NewMemStore()
		l := NewLog(store, WithClock(clock))

		// Four bytes per step: entity, from index, to index, flags.
		for i := 0; i+3 < len(in) && i < 400; i += 4 {
			entity := fmt.Sprintf("e-%d", int(in[i])%3)

			var from, to time.Time
			if fi := int(in[i+1]); fi < len(grid)*2 {
				if fi < len(grid) {
					from = grid[fi]
				}
			}
			if ti := int(in[i+2]); ti < len(grid)*2 {
				if ti < len(grid) {
					to = grid[ti]
				}
			}

			flags := in[i+3]
			if flags&1 != 0 {
				clock.Advance(time.Duration(1+int(flags>>1)) * time.Hour)
			}

			iv := Interval{From: from, To: to}
			var err error
			if flags&2 != 0 {
				_, err = l.CorrectInterval(ctx, employee, entity, []byte(`{}`), iv, alice)
			} else {
				_, err = l.PutInterval(ctx, employee, entity, []byte(`{}`), iv, alice)
			}

			if wantErr := iv.Validate() != nil; wantErr {
				if !errors.Is(err, ErrInvalidInterval) {
					t.Fatalf("malformed interval %s = %v; want ErrInvalidInterval", iv, err)
				}
				continue
			}
			if err != nil {
				t.Fatalf("writing %s failed: %v", iv, err)
			}

			assertInvariants(t, store)
		}
	})
}
