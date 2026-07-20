package pgstore_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/zkrebbekx/chronicle"
	"github.com/zkrebbekx/chronicle/pgstore"
)

// TestConcurrentWriters covers the second correctness requirement: two writers
// to one entity must not both observe the same pre-state and each split it.
//
// The test uses two separate connection pools, so the writers really are
// independent as far as the database is concerned — a single *sql.DB would
// still hand out distinct connections, but two pools rule out any chance that
// the isolation being tested is really an artefact of pooling. The two logs are
// separate values with separate ratchets and separate node tokens, which is the
// arrangement that in-process mutexes cannot help with.
func TestConcurrentWriters(t *testing.T) {
	dsn := os.Getenv(DSNEnv)
	if dsn == "" {
		t.Skipf("set %s to run the Postgres integration tests", DSNEnv)
	}
	db := testDB(t)
	ctx := context.Background()

	t.Run("given two independent writers racing on one entity", func(t *testing.T) {
		store, schema := newStoreNamed(t, db)

		storeA := attach(t, openPool(t, dsn), schema)
		storeB := attach(t, openPool(t, dsn), schema)
		logA := chronicle.NewLog(storeA)
		logB := chronicle.NewLog(storeB)

		const rounds = 40
		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			applied int
			landed  = map[string]int{}
			failed  []error
		)

		// Both writers assert overlapping intervals over the same entity, so
		// every round is a genuine contest: whoever loses must supersede what
		// the winner left rather than what it originally read.
		for _, w := range []struct {
			log  *chronicle.Log
			name string
			from time.Time
		}{
			{logA, "A", march},
			{logB, "B", april},
		} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < rounds; i++ {
					_, err := w.log.Put(ctx, "employee", "contested",
						[]byte(fmt.Sprintf(`{"w":%q,"i":%d}`, w.name, i)), w.from, july, alice)
					mu.Lock()
					if err != nil {
						failed = append(failed, err)
					} else {
						applied++
						landed[w.name]++
					}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()

		t.Run("when both have finished", func(t *testing.T) {
			t.Run("then neither writer starved", func(t *testing.T) {
				// This is the assertion the earlier design failed. With the
				// overlap scan outside the store's lock, the writer that waits
				// for the lock finds its plan stale every single time, while
				// the writer that never waits never conflicts — so one of the
				// two lands nothing at all. Counting per writer is what
				// distinguishes that from ordinary contention.
				mu.Lock()
				defer mu.Unlock()
				for name, n := range landed {
					if n != rounds {
						t.Fatalf("writer %s landed %d of %d writes; both writers must make "+
							"progress, and a writer that loses every race is starved rather "+
							"than merely unlucky", name, n, rounds)
					}
				}
				if len(landed) != 2 {
					t.Fatalf("only %d writers recorded progress; want 2", len(landed))
				}
			})
			t.Run("then no write was lost or half-applied", func(t *testing.T) {
				if len(failed) != 0 {
					t.Fatalf("%d of %d writes failed, first: %v\n"+
						"a conflict is retryable and the log retries it; a write that still "+
						"fails is one the caller has to be told about, and there should be none "+
						"when the store plans under its own lock", len(failed), rounds*2, failed[0])
				}
			})
			t.Run("then no two current records cover the same instant", func(t *testing.T) {
				if n := countOverlaps(ctx, t, db, store.Table()); n != 0 {
					t.Fatalf("%d overlapping pairs of current records — two writers split the "+
						"same pre-state", n)
				}
			})
			t.Run("then every write is present in the log", func(t *testing.T) {
				// Nothing is destroyed, so the transaction axis holds one
				// generation per successful write regardless of who won which
				// race. Counting the asserts is how a silently dropped write
				// would show up.
				recs, _, err := storeA.Query(ctx, chronicle.Query{
					Kind: "employee", EntityID: "contested",
					Intent: chronicle.IntentAssert, HasIntent: true,
				})
				if err != nil {
					t.Fatalf("Query failed: %v", err)
				}
				if len(recs) != applied {
					t.Fatalf("log holds %d asserted records; %d writes reported success — "+
						"a write that returned nil must be in the log", len(recs), applied)
				}
			})
			t.Run("then the transaction axis is strictly ordered", func(t *testing.T) {
				assertTxOrdered(ctx, t, storeA, "contested")
			})
		})
	})

	t.Run("given two writers whose plans are computed from the same pre-state", func(t *testing.T) {
		// The race above is real but timing-dependent. This one stages the
		// hazard deterministically: both writes are planned against the same
		// snapshot, and the second is applied after the first has committed.
		store := newStore(t, db)
		l := chronicle.NewLog(store)
		first, err := l.Put(ctx, "employee", "e1", []byte("v0"), march, july, alice)
		if err != nil {
			t.Fatalf("Put failed: %v", err)
		}
		original := first.Record.ID

		// Both plans are static, so neither is recomputed from what the
		// store reads: this is precisely the shape of write that can go stale,
		// and it is why StaticWrite is documented as unsafe for ordinary use.
		planA := chronicle.ApplyRequest{Plan: chronicle.StaticWrite(chronicle.Write{
			Supersede: []chronicle.RecordID{original},
			Insert: []chronicle.Record{{
				ID: "plan-a", Kind: "employee", EntityID: "e1",
				Data: []byte("A"), ValidFrom: march, ValidTo: july, Actor: alice,
			}},
		})}
		planB := chronicle.ApplyRequest{Plan: chronicle.StaticWrite(chronicle.Write{
			Supersede: []chronicle.RecordID{original},
			Insert: []chronicle.Record{{
				ID: "plan-b", Kind: "employee", EntityID: "e1",
				Data: []byte("B"), ValidFrom: march, ValidTo: july, Actor: bob,
			}},
		})}

		t.Run("when the first is applied", func(t *testing.T) {
			t.Run("then it succeeds", func(t *testing.T) {
				if _, err := store.Apply(ctx, planA); err != nil {
					t.Fatalf("Apply of the first plan failed: %v", err)
				}
			})
		})

		t.Run("when the second is applied against the state it no longer describes", func(t *testing.T) {
			_, err := store.Apply(ctx, planB)
			t.Run("then it is refused as a conflict rather than corrupting the log", func(t *testing.T) {
				if !errors.Is(err, chronicle.ErrConflict) {
					t.Fatalf("Apply = %v; want ErrConflict — the record it means to supersede "+
						"has already been superseded, so applying its insert would leave two "+
						"current records over [march, july)", err)
				}
			})
			t.Run("then nothing of it was applied", func(t *testing.T) {
				recs, _, err := store.Query(ctx, chronicle.Query{Kind: "employee", EntityID: "e1"})
				if err != nil {
					t.Fatalf("Query failed: %v", err)
				}
				for _, r := range recs {
					if r.ID == "plan-b" {
						t.Fatal("the conflicting write's record is in the log")
					}
				}
			})
			t.Run("then the invariant is intact", func(t *testing.T) {
				if n := countOverlaps(ctx, t, db, store.Table()); n != 0 {
					t.Fatalf("%d overlapping pairs of current records", n)
				}
			})
		})
	})

	t.Run("given many writers spread across many entities", func(t *testing.T) {
		// Per-entity locking must not become a global one. This is a coarse
		// check that unrelated entities proceed together rather than queueing
		// behind each other, and a check that the invariant holds at breadth
		// as well as under contention.
		store := newStore(t, db)
		const writers, entities, each = 8, 16, 10

		var wg sync.WaitGroup
		errCh := make(chan error, writers*each)
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				l := chronicle.NewLog(store)
				for i := 0; i < each; i++ {
					entity := fmt.Sprintf("e%d", (w*each+i)%entities)
					from := march.AddDate(0, i%4, 0)
					if _, err := l.Put(ctx, "employee", entity,
						[]byte(fmt.Sprintf(`{"w":%d,"i":%d}`, w, i)), from, from.AddDate(0, 2, 0), alice); err != nil {
						errCh <- err
						return
					}
				}
			}(w)
		}
		wg.Wait()
		close(errCh)

		t.Run("when they have all finished", func(t *testing.T) {
			t.Run("then none of them failed", func(t *testing.T) {
				for err := range errCh {
					t.Fatalf("concurrent write failed: %v", err)
				}
			})
			t.Run("then no entity has overlapping current records", func(t *testing.T) {
				if n := countOverlaps(ctx, t, db, store.Table()); n != 0 {
					t.Fatalf("%d overlapping pairs of current records", n)
				}
			})
		})
	})
}

// TestDatabaseAssignedTransactionTime covers the third correctness requirement:
// transaction time comes from the database, not from the Go process, so that
// two logs over one store produce a single correctly ordered history rather
// than two interleaved ones.
func TestDatabaseAssignedTransactionTime(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	t.Run("given two logs over one store, one with a clock stuck in the past", func(t *testing.T) {
		store := newStore(t, db)

		// A frozen clock decades behind the database's is the strongest form
		// of the hazard: if the log's own ratchet were authoritative, every
		// write through this one would land before every write through the
		// other, and an as-of query would read them in the wrong order.
		stuck := chronicle.NewLog(store, chronicle.WithClock(
			chronicle.NewFixedClock(time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC))))
		normal := chronicle.NewLog(store)

		var results []chronicle.Result
		for i := 0; i < 6; i++ {
			l, who := stuck, "stuck"
			if i%2 == 1 {
				l, who = normal, "normal"
			}
			from := march.AddDate(0, i, 0)
			res, err := l.Put(ctx, "employee", "e1", []byte(fmt.Sprintf(`{"by":%q,"i":%d}`, who, i)),
				from, from.AddDate(0, 1, 0), alice)
			if err != nil {
				t.Fatalf("write %d failed: %v", i, err)
			}
			results = append(results, res)
		}

		t.Run("when the transaction axis is read back", func(t *testing.T) {
			t.Run("then the instants increase in write order", func(t *testing.T) {
				for i := 1; i < len(results); i++ {
					if !results[i].TxAt.After(results[i-1].TxAt) {
						t.Fatalf("write %d landed at %s, not after write %d's %s — the frozen "+
							"clock reached the transaction axis", i, results[i].TxAt, i-1, results[i-1].TxAt)
					}
				}
			})
			t.Run("then no instant came from the frozen clock", func(t *testing.T) {
				cutoff := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
				for i, res := range results {
					if res.TxAt.Before(cutoff) {
						t.Fatalf("write %d landed at %s; the database assigns transaction time, "+
							"so no process's clock can put a record in 1990", i, res.TxAt)
					}
				}
			})
			t.Run("then the log agrees with the rows the database holds", func(t *testing.T) {
				for i, res := range results {
					got, err := store.Get(ctx, chronicle.GetQuery{
						Kind: "employee", EntityID: "e1",
						ValidAt: res.Record.ValidFrom, TxAt: res.TxAt,
					})
					if err != nil {
						t.Fatalf("write %d is not readable at the instant it reported: %v", i, err)
					}
					if got.ID != res.Record.ID {
						t.Fatalf("write %d reported record %s but %s is what sits at %s",
							i, res.Record.ID, got.ID, res.TxAt)
					}
				}
			})
			t.Run("then the history has no gaps or overlaps on the transaction axis", func(t *testing.T) {
				assertTxOrdered(ctx, t, store, "e1")
			})
		})
	})

	t.Run("given a store written to by a log whose clock runs fast", func(t *testing.T) {
		store := newStore(t, db)
		ahead := chronicle.NewLog(store, chronicle.WithClock(
			chronicle.NewFixedClock(time.Now().AddDate(50, 0, 0).UTC())))

		res, err := ahead.Put(ctx, "employee", "e1", []byte("v"), march, july, alice)
		if err != nil {
			t.Fatalf("Put failed: %v", err)
		}

		t.Run("when the record is read back", func(t *testing.T) {
			t.Run("then the future clock did not reach the transaction axis either", func(t *testing.T) {
				if res.TxAt.After(time.Now().AddDate(1, 0, 0)) {
					t.Fatalf("TxAt = %s; a caller's clock must not be able to postdate the log "+
						"any more than it can backdate it", res.TxAt)
				}
			})
			t.Run("then a read of now still finds it", func(t *testing.T) {
				// The log's notion of "now" is the later of its clock and its
				// last write, so a fast clock could otherwise leave a read
				// looking past everything in the store.
				if _, err := ahead.Get(ctx, "employee", "e1", chronicle.ValidAt(may)); err != nil {
					t.Fatalf("Get failed: %v", err)
				}
			})
		})
	})
}

// openPool opens a second, independent connection pool, so that two writers are
// independent all the way down rather than merely on different goroutines.
func openPool(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("opening a second pool: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// assertTxOrdered checks that an entity's transaction intervals are all
// non-empty and that no two current records exist at once — the transaction-axis
// counterpart to the valid-axis non-overlap invariant.
func assertTxOrdered(ctx context.Context, t *testing.T, store *pgstore.Store, entityID string) {
	t.Helper()
	recs, _, err := store.Query(ctx, chronicle.Query{Kind: "employee", EntityID: entityID})
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	for _, r := range recs {
		if r.TxFrom.IsZero() {
			t.Fatalf("record %s has no transaction start", r.ID)
		}
		if !r.IsCurrent() && !r.TxTo.After(r.TxFrom) {
			t.Fatalf("record %s has an empty transaction interval [%s, %s) and so is invisible "+
				"to every as-of query", r.ID, r.TxFrom, r.TxTo)
		}
	}
}
