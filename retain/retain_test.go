package retain_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zkrebbekx/chronicle"
	"github.com/zkrebbekx/chronicle/retain"
)

var (
	base  = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	jan   = time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	feb   = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	alice = chronicle.Actor{ID: "u-1", Name: "Alice"}
	bob   = chronicle.Actor{ID: "u-2", Name: "Bob"}
)

const (
	employee = "employee"
	invoice  = "invoice"
	week     = 7 * 24 * time.Hour
)

// fixture is a MemStore seeded through a Log under a controlled clock, so
// that every record's supersession instant is exact.
type fixture struct {
	store *chronicle.MemStore
	log   *chronicle.Log
	clock *chronicle.FixedClock
}

func newFixture(t *testing.T, opts ...chronicle.Option) *fixture {
	t.Helper()
	f := &fixture{
		store: chronicle.NewMemStore(),
		clock: chronicle.NewFixedClock(base),
	}
	f.log = chronicle.NewLog(f.store, append([]chronicle.Option{chronicle.WithClock(f.clock)}, opts...)...)
	return f
}

// put writes and advances the clock a second, so successive writes land at
// distinct, known transaction instants.
func (f *fixture) put(t *testing.T, kind, id, data string) chronicle.Result {
	t.Helper()
	f.clock.Advance(time.Second)
	res, err := f.log.Put(context.Background(), kind, id, []byte(data), jan, time.Time{}, alice)
	if err != nil {
		t.Fatalf("Put(%s/%s) failed: %v", kind, id, err)
	}
	return res
}

// supersededVersions writes n+1 generations of one entity and returns the
// instant the last supersession happened at.
func (f *fixture) supersededVersions(t *testing.T, kind, id string, n int) time.Time {
	t.Helper()
	var last chronicle.Result
	for i := 0; i <= n; i++ {
		last = f.put(t, kind, id, `{"v":`+string(rune('0'+i))+`}`)
	}
	return last.TxAt
}

func (f *fixture) records(t *testing.T, kind string) []chronicle.Record {
	t.Helper()
	recs, _, err := f.store.Query(context.Background(), chronicle.Query{Kind: kind})
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	return recs
}

// coreOnly hides every optional capability of the wrapped store.
type coreOnly struct{ chronicle.Store }

// deleterOnly exposes deletion but not holds: the zero-argument Holds shadows
// the promoted one, so the type no longer satisfies HoldStore.
type deleterOnly struct{ *chronicle.MemStore }

func (deleterOnly) Holds() {}

// failingHolds reports holds unreadable.
type failingHolds struct{ *chronicle.MemStore }

func (s failingHolds) Holds(context.Context) ([]chronicle.Hold, error) {
	return nil, errors.New("holds unavailable")
}

// failingDelete refuses every deletion after reads succeeded.
type failingDelete struct{ *chronicle.MemStore }

func (s failingDelete) Delete(context.Context, []chronicle.RecordID) (int, error) {
	return 0, errors.New("deletion refused")
}

// failingQuery cannot be scanned.
type failingQuery struct{ *chronicle.MemStore }

func (s failingQuery) Query(context.Context, chronicle.Query) ([]chronicle.Record, chronicle.Cursor, error) {
	return nil, chronicle.NoCursor, errors.New("scan failed")
}

func TestSweepValidation(t *testing.T) {
	ctx := context.Background()
	store := chronicle.NewMemStore()

	t.Run("given no policy at all", func(t *testing.T) {
		t.Run("when a sweep is planned", func(t *testing.T) {
			t.Run("then it refuses — no default retention period ships", func(t *testing.T) {
				if _, err := retain.Plan(ctx, store, nil, feb); !errors.Is(err, retain.ErrNoPolicy) {
					t.Fatalf("Plan = %v; want ErrNoPolicy", err)
				}
				if _, err := retain.Execute(ctx, store, nil, feb); !errors.Is(err, retain.ErrNoPolicy) {
					t.Fatalf("Execute = %v; want ErrNoPolicy", err)
				}
			})
		})
	})

	t.Run("given malformed policies", func(t *testing.T) {
		cases := []struct {
			name     string
			policies []retain.Policy
		}{
			{"a policy with no kind", []retain.Policy{{KeepFor: week}}},
			{"a zero KeepFor", []retain.Policy{{Kind: employee}}},
			{"a negative KeepFor", []retain.Policy{{Kind: employee, KeepFor: -time.Hour}}},
			{"two policies for one kind", []retain.Policy{
				{Kind: employee, KeepFor: week},
				{Kind: employee, KeepFor: 2 * week},
			}},
		}
		for _, tc := range cases {
			t.Run("when the sweep is given "+tc.name, func(t *testing.T) {
				t.Run("then it is rejected before touching the store", func(t *testing.T) {
					if _, err := retain.Plan(ctx, store, tc.policies, feb); !errors.Is(err, retain.ErrInvalidPolicy) {
						t.Fatalf("Plan = %v; want ErrInvalidPolicy", err)
					}
				})
			})
		}
	})

	t.Run("given a store without the deletion capability", func(t *testing.T) {
		t.Run("when Execute is asked to sweep it", func(t *testing.T) {
			_, err := retain.Execute(ctx, coreOnly{store}, []retain.Policy{{Kind: employee, KeepFor: week}}, feb)
			t.Run("then it refuses with ErrNoDeleter", func(t *testing.T) {
				if !errors.Is(err, retain.ErrNoDeleter) {
					t.Fatalf("Execute = %v; want ErrNoDeleter", err)
				}
			})
		})
		t.Run("when Plan is asked the same", func(t *testing.T) {
			t.Run("then it works — planning only reads", func(t *testing.T) {
				if _, err := retain.Plan(ctx, coreOnly{store}, []retain.Policy{{Kind: employee, KeepFor: week}}, feb); err != nil {
					t.Fatalf("Plan = %v; want nil", err)
				}
			})
		})
	})
}

func TestSweepEligibility(t *testing.T) {
	ctx := context.Background()
	policy := []retain.Policy{{Kind: employee, KeepFor: week}}

	t.Run("given superseded and current records of mixed ages", func(t *testing.T) {
		f := newFixture(t)
		lastTx := f.supersededVersions(t, employee, "e1", 2) // three records, two superseded
		f.supersededVersions(t, invoice, "i1", 1)            // another kind, one superseded

		t.Run("when a sweep is planned with the cutoff past everything", func(t *testing.T) {
			now := lastTx.Add(week + time.Hour)
			plan, err := retain.Plan(ctx, f.store, policy, now)
			if err != nil {
				t.Fatalf("Plan failed: %v", err)
			}
			kr := plan.Kinds[0]

			t.Run("then it would delete the superseded records and none other", func(t *testing.T) {
				if plan.Executed {
					t.Fatal("a plan reported itself executed")
				}
				if kr.Kind != employee || kr.Deleted != 2 {
					t.Fatalf("plan = %+v; want 2 employee records deleted", kr)
				}
				if !kr.Cutoff.Equal(now.Add(-week)) {
					t.Fatalf("cutoff = %s; want now minus KeepFor", kr.Cutoff)
				}
			})
			t.Run("then planning deleted nothing", func(t *testing.T) {
				if got := len(f.records(t, employee)); got != 3 {
					t.Fatalf("records after Plan = %d; want all 3", got)
				}
			})

			t.Run("when the sweep is then executed", func(t *testing.T) {
				rep, err := retain.Execute(ctx, f.store, policy, now)
				if err != nil {
					t.Fatalf("Execute failed: %v", err)
				}
				t.Run("then the report matches the plan and the records are gone", func(t *testing.T) {
					if !rep.Executed || rep.Kinds[0].Deleted != 2 {
						t.Fatalf("report = %+v; want 2 deleted, executed", rep.Kinds[0])
					}
					recs := f.records(t, employee)
					if len(recs) != 1 || !recs[0].IsCurrent() {
						t.Fatalf("survivors = %d; want just the current record", len(recs))
					}
				})
				t.Run("then the kind without a policy was untouched", func(t *testing.T) {
					if got := len(f.records(t, invoice)); got != 2 {
						t.Fatalf("invoice records = %d; want both — no policy, no sweep", got)
					}
				})
				t.Run("then a second sweep finds nothing left to do", func(t *testing.T) {
					rep, err := retain.Execute(ctx, f.store, policy, now)
					if err != nil || rep.Kinds[0].Deleted != 0 {
						t.Fatalf("re-sweep = (%+v, %v); want zero deletions", rep.Kinds[0], err)
					}
				})
			})
		})
	})

	t.Run("given a current record far older than the policy", func(t *testing.T) {
		f := newFixture(t)
		res := f.put(t, employee, "e1", `{"v":0}`) // never superseded

		t.Run("when swept decades later", func(t *testing.T) {
			rep, err := retain.Execute(ctx, f.store, policy, res.TxAt.AddDate(30, 0, 0))
			if err != nil {
				t.Fatalf("Execute failed: %v", err)
			}
			t.Run("then it survives — current belief is never retention-deleted", func(t *testing.T) {
				if rep.Kinds[0].Deleted != 0 {
					t.Fatalf("deleted = %d; want 0", rep.Kinds[0].Deleted)
				}
				if got := len(f.records(t, employee)); got != 1 {
					t.Fatalf("records = %d; want the current record intact", got)
				}
			})
		})
	})

	t.Run("given a record superseded only recently", func(t *testing.T) {
		f := newFixture(t)
		// Written long before the cutoff, superseded just now: the age that
		// matters is how long it has been dead, not how long ago it was born.
		f.put(t, employee, "e1", `{"v":0}`)
		f.clock.Advance(52 * week)
		f.clock.Advance(time.Second)
		res, err := f.log.Put(ctx, employee, "e1", []byte(`{"v":1}`), jan, time.Time{}, alice)
		if err != nil {
			t.Fatalf("Put failed: %v", err)
		}

		t.Run("when swept a day after the supersession", func(t *testing.T) {
			rep, err := retain.Execute(ctx, f.store, policy, res.TxAt.Add(24*time.Hour))
			if err != nil {
				t.Fatalf("Execute failed: %v", err)
			}
			t.Run("then the freshly dead record is kept", func(t *testing.T) {
				if rep.Kinds[0].Deleted != 0 {
					t.Fatalf("deleted = %d; want 0 — KeepFor runs from TxTo, not TxFrom", rep.Kinds[0].Deleted)
				}
			})
		})
		t.Run("when swept a week and a day after the supersession", func(t *testing.T) {
			rep, err := retain.Execute(ctx, f.store, policy, res.TxAt.Add(week+24*time.Hour))
			if err != nil {
				t.Fatalf("Execute failed: %v", err)
			}
			t.Run("then it is destroyed on schedule", func(t *testing.T) {
				if rep.Kinds[0].Deleted != 1 {
					t.Fatalf("deleted = %d; want 1", rep.Kinds[0].Deleted)
				}
			})
		})
	})

	t.Run("given a zero now", func(t *testing.T) {
		f := newFixture(t)
		f.supersededVersions(t, employee, "e1", 1)
		t.Run("when the sweep is run", func(t *testing.T) {
			rep, err := retain.Plan(ctx, f.store, policy, time.Time{})
			t.Run("then the wall clock is used and reported", func(t *testing.T) {
				if err != nil || rep.Now.IsZero() {
					t.Fatalf("Plan = (%+v, %v); want a non-zero Now", rep.Now, err)
				}
			})
		})
	})
}

func TestSweepHolds(t *testing.T) {
	ctx := context.Background()
	policy := []retain.Policy{{Kind: employee, KeepFor: week}}

	// seed builds two employees and an invoice, everything superseded once and
	// old enough to sweep, and returns the sweep instant.
	seed := func(t *testing.T) (*fixture, time.Time) {
		t.Helper()
		f := newFixture(t)
		f.supersededVersions(t, employee, "e1", 1)
		last := f.supersededVersions(t, employee, "e2", 1)
		return f, last.Add(week + time.Hour)
	}

	t.Run("given an active hold scoped to the kind", func(t *testing.T) {
		f, now := seed(t)
		if _, err := f.store.PlaceHold(ctx, chronicle.Hold{
			ID: "matter-1", Kind: employee, Reason: "anticipated litigation", PlacedBy: alice,
		}); err != nil {
			t.Fatalf("PlaceHold failed: %v", err)
		}

		t.Run("when the sweep runs", func(t *testing.T) {
			rep, err := retain.Execute(ctx, f.store, policy, now)
			if err != nil {
				t.Fatalf("Execute failed: %v", err)
			}
			kr := rep.Kinds[0]
			t.Run("then every eligible record is withheld, named, and attributed", func(t *testing.T) {
				if kr.Deleted != 0 {
					t.Fatalf("deleted = %d; want 0 — hold always beats retention", kr.Deleted)
				}
				if len(kr.Withheld) != 2 {
					t.Fatalf("withheld = %+v; want both eligible records", kr.Withheld)
				}
				for _, w := range kr.Withheld {
					if w.HoldID != "matter-1" {
						t.Fatalf("withholding = %+v; want it attributed to matter-1", w)
					}
				}
			})
			t.Run("then nothing was destroyed", func(t *testing.T) {
				if got := len(f.records(t, employee)); got != 4 {
					t.Fatalf("records = %d; want all 4", got)
				}
			})
		})

		t.Run("when the hold is released and the sweep re-runs", func(t *testing.T) {
			if _, err := f.store.ReleaseHold(ctx, "matter-1", bob, "matter settled"); err != nil {
				t.Fatalf("ReleaseHold failed: %v", err)
			}
			// The store stamped the release with the wall clock, and a hold is
			// active over [EffectiveFrom, ReleasedAt) — so the re-run sweeps at
			// the wall clock too, where the release has taken effect. The
			// fixture's records are long superseded either way.
			rep, err := retain.Execute(ctx, f.store, policy, time.Time{})
			if err != nil {
				t.Fatalf("Execute failed: %v", err)
			}
			t.Run("then retention resumes", func(t *testing.T) {
				if rep.Kinds[0].Deleted != 2 || len(rep.Kinds[0].Withheld) != 0 {
					t.Fatalf("report = %+v; want 2 deleted, none withheld", rep.Kinds[0])
				}
			})
		})
	})

	t.Run("given a backdated hold placed after the records were superseded", func(t *testing.T) {
		f, now := seed(t)
		// The operator asserts the duty attached a year before anyone placed
		// the hold. FRCP 37(e)'s trigger is anticipation, judged after the
		// fact; the hold must be placeable that way, and must bite.
		if _, err := f.store.PlaceHold(ctx, chronicle.Hold{
			ID:            "matter-2",
			Kind:          employee,
			EffectiveFrom: now.AddDate(-1, 0, 0),
			PlacedBy:      alice,
		}); err != nil {
			t.Fatalf("PlaceHold failed: %v", err)
		}

		t.Run("when the sweep runs", func(t *testing.T) {
			rep, err := retain.Execute(ctx, f.store, policy, now)
			if err != nil {
				t.Fatalf("Execute failed: %v", err)
			}
			t.Run("then the backdated hold withholds everything in scope", func(t *testing.T) {
				if rep.Kinds[0].Deleted != 0 || len(rep.Kinds[0].Withheld) != 2 {
					t.Fatalf("report = %+v; want everything withheld under the backdated hold", rep.Kinds[0])
				}
			})
		})
	})

	t.Run("given a hold that only takes effect in the future", func(t *testing.T) {
		f, now := seed(t)
		if _, err := f.store.PlaceHold(ctx, chronicle.Hold{
			ID: "matter-3", Kind: employee, EffectiveFrom: now.Add(time.Hour), PlacedBy: alice,
		}); err != nil {
			t.Fatalf("PlaceHold failed: %v", err)
		}
		t.Run("when the sweep runs before that", func(t *testing.T) {
			rep, err := retain.Execute(ctx, f.store, policy, now)
			if err != nil {
				t.Fatalf("Execute failed: %v", err)
			}
			t.Run("then the hold does not bite yet", func(t *testing.T) {
				if rep.Kinds[0].Deleted != 2 {
					t.Fatalf("deleted = %d; want 2 — the duty has not attached", rep.Kinds[0].Deleted)
				}
			})
		})
	})

	t.Run("given a hold scoped to one entity", func(t *testing.T) {
		f, now := seed(t)
		if _, err := f.store.PlaceHold(ctx, chronicle.Hold{
			ID: "matter-4", Kind: employee, EntityID: "e1", PlacedBy: alice,
		}); err != nil {
			t.Fatalf("PlaceHold failed: %v", err)
		}
		t.Run("when the sweep runs", func(t *testing.T) {
			rep, err := retain.Execute(ctx, f.store, policy, now)
			if err != nil {
				t.Fatalf("Execute failed: %v", err)
			}
			kr := rep.Kinds[0]
			t.Run("then only that entity is withheld", func(t *testing.T) {
				if kr.Deleted != 1 || len(kr.Withheld) != 1 {
					t.Fatalf("report = %+v; want e2 deleted and e1 withheld", kr)
				}
			})
		})
	})

	t.Run("given two active holds matching one record", func(t *testing.T) {
		f, now := seed(t)
		for _, id := range []string{"matter-first", "matter-second"} {
			if _, err := f.store.PlaceHold(ctx, chronicle.Hold{ID: id, Kind: employee, PlacedBy: alice}); err != nil {
				t.Fatalf("PlaceHold failed: %v", err)
			}
		}
		t.Run("when the sweep attributes the withholding", func(t *testing.T) {
			rep, err := retain.Execute(ctx, f.store, policy, now)
			if err != nil {
				t.Fatalf("Execute failed: %v", err)
			}
			t.Run("then the first hold in placement order is named", func(t *testing.T) {
				for _, w := range rep.Kinds[0].Withheld {
					if w.HoldID != "matter-first" {
						t.Fatalf("withholding = %+v; want the first matching hold", w)
					}
				}
			})
		})
	})

	t.Run("given a store that can delete but cannot hold", func(t *testing.T) {
		f, now := seed(t)
		t.Run("when the sweep runs over it", func(t *testing.T) {
			rep, err := retain.Execute(ctx, deleterOnly{f.store}, policy, now)
			t.Run("then it proceeds — a store without the capability cannot contain holds", func(t *testing.T) {
				if err != nil || rep.Kinds[0].Deleted != 2 {
					t.Fatalf("Execute = (%+v, %v); want a normal sweep", rep.Kinds[0], err)
				}
			})
		})
	})
}

func TestSweepArchiveHook(t *testing.T) {
	ctx := context.Background()
	policy := []retain.Policy{{Kind: employee, KeepFor: week}}

	t.Run("given an archive hook", func(t *testing.T) {
		f := newFixture(t)
		last := f.supersededVersions(t, employee, "e1", 2)
		now := last.Add(week + time.Hour)

		var archived []chronicle.RecordID
		hook := func(_ context.Context, doomed []chronicle.Record) error {
			for _, r := range doomed {
				archived = append(archived, r.ID)
			}
			return nil
		}

		t.Run("when the sweep is planned", func(t *testing.T) {
			if _, err := retain.Plan(ctx, f.store, policy, now); err != nil {
				t.Fatalf("Plan failed: %v", err)
			}
			t.Run("then the hook is not called — a dry run archives nothing", func(t *testing.T) {
				if len(archived) != 0 {
					t.Fatalf("archived = %v during a plan", archived)
				}
			})
		})

		t.Run("when the sweep executes", func(t *testing.T) {
			rep, err := retain.Execute(ctx, f.store, policy, now, retain.WithArchive(hook))
			if err != nil {
				t.Fatalf("Execute failed: %v", err)
			}
			t.Run("then every destroyed record went through the hook first", func(t *testing.T) {
				if len(archived) != rep.Kinds[0].Deleted {
					t.Fatalf("archived %d, deleted %d; the hook must see every doomed record",
						len(archived), rep.Kinds[0].Deleted)
				}
			})
		})
	})

	t.Run("given an archive hook that fails", func(t *testing.T) {
		f := newFixture(t)
		last := f.supersededVersions(t, employee, "e1", 2)
		now := last.Add(week + time.Hour)

		t.Run("when the sweep executes", func(t *testing.T) {
			_, err := retain.Execute(ctx, f.store, policy, now,
				retain.WithArchive(func(context.Context, []chronicle.Record) error {
					return errors.New("archive storage full")
				}))
			t.Run("then the sweep aborts and nothing was destroyed", func(t *testing.T) {
				if err == nil {
					t.Fatal("Execute succeeded past a failing archive hook")
				}
				if got := len(f.records(t, employee)); got != 3 {
					t.Fatalf("records = %d; want all 3 — no archive, no deletion", got)
				}
			})
		})
	})

	t.Run("given a batch size smaller than the eligible set", func(t *testing.T) {
		f := newFixture(t)
		last := f.supersededVersions(t, employee, "e1", 5) // five superseded
		now := last.Add(week + time.Hour)

		var batches [][]chronicle.RecordID
		hook := func(_ context.Context, doomed []chronicle.Record) error {
			ids := make([]chronicle.RecordID, len(doomed))
			for i, r := range doomed {
				ids[i] = r.ID
			}
			batches = append(batches, ids)
			return nil
		}

		t.Run("when the sweep executes at batch size two", func(t *testing.T) {
			rep, err := retain.Execute(ctx, f.store, policy, now,
				retain.WithArchive(hook), retain.WithBatchSize(2))
			if err != nil {
				t.Fatalf("Execute failed: %v", err)
			}
			t.Run("then everything is destroyed across several bounded batches", func(t *testing.T) {
				if rep.Kinds[0].Deleted != 5 {
					t.Fatalf("deleted = %d; want 5", rep.Kinds[0].Deleted)
				}
				if len(batches) < 2 {
					t.Fatalf("batches = %d; want the work split", len(batches))
				}
				for _, b := range batches {
					if len(b) > 2 {
						t.Fatalf("batch of %d exceeded the size", len(b))
					}
				}
				if got := len(f.records(t, employee)); got != 1 {
					t.Fatalf("records = %d; want just the current one", got)
				}
			})
		})
	})
}

func TestSweepTombstones(t *testing.T) {
	ctx := context.Background()
	policy := []retain.Policy{{Kind: employee, KeepFor: week}}

	t.Run("given a chained entity with sweepable history", func(t *testing.T) {
		f := newFixture(t, chronicle.WithChaining())
		last := f.supersededVersions(t, employee, "e1", 2)
		now := last.Add(week + time.Hour)

		t.Run("when the sweep is planned and executed", func(t *testing.T) {
			plan, err := retain.Plan(ctx, f.store, policy, now)
			if err != nil {
				t.Fatalf("Plan failed: %v", err)
			}
			rep, err := retain.Execute(ctx, f.store, policy, now)
			if err != nil {
				t.Fatalf("Execute failed: %v", err)
			}
			t.Run("then both report the tombstones the deletion leaves", func(t *testing.T) {
				if plan.Kinds[0].Tombstones != 2 || rep.Kinds[0].Tombstones != 2 {
					t.Fatalf("tombstones plan=%d execute=%d; want 2 and 2",
						plan.Kinds[0].Tombstones, rep.Kinds[0].Tombstones)
				}
			})
			t.Run("then the store holds them and the chain still verifies", func(t *testing.T) {
				ts, err := f.store.Tombstones(ctx, employee, "e1")
				if err != nil || len(ts) != 2 {
					t.Fatalf("Tombstones = (%d, %v); want 2", len(ts), err)
				}
				verified, err := chronicle.NewLog(f.store).Verify(ctx, employee, "e1")
				if err != nil {
					t.Fatalf("Verify failed: %v", err)
				}
				if !verified.Intact() || verified.Tombstones != 2 {
					t.Fatalf("Verify = %+v; want an intact chain across the gap", verified)
				}
			})
		})
	})
}

func TestSweepFailurePaths(t *testing.T) {
	ctx := context.Background()
	policy := []retain.Policy{{Kind: employee, KeepFor: week}}

	seed := func(t *testing.T) (*fixture, time.Time) {
		t.Helper()
		f := newFixture(t)
		last := f.supersededVersions(t, employee, "e1", 1)
		return f, last.Add(week + time.Hour)
	}

	t.Run("given stores that fail partway", func(t *testing.T) {
		t.Run("when the scan fails", func(t *testing.T) {
			f, now := seed(t)
			_, err := retain.Execute(ctx, failingQuery{f.store}, policy, now)
			t.Run("then the sweep reports it", func(t *testing.T) {
				if err == nil {
					t.Fatal("Execute succeeded over an unscannable store")
				}
			})
		})
		t.Run("when the holds cannot be read", func(t *testing.T) {
			f, now := seed(t)
			_, err := retain.Execute(ctx, failingHolds{f.store}, policy, now)
			t.Run("then the sweep refuses to guess and stops", func(t *testing.T) {
				if err == nil {
					t.Fatal("Execute swept without being able to check holds")
				}
				if got := len(f.records(t, employee)); got != 2 {
					t.Fatalf("records = %d; want everything intact", got)
				}
			})
		})
		t.Run("when deletion fails", func(t *testing.T) {
			f, now := seed(t)
			rep, err := retain.Execute(ctx, failingDelete{f.store}, policy, now)
			t.Run("then the error surfaces with the partial report", func(t *testing.T) {
				if err == nil {
					t.Fatal("Execute swallowed a deletion failure")
				}
				if len(rep.Kinds) == 0 {
					t.Fatal("no partial report came back")
				}
			})
		})
	})
}
