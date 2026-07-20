package chronicle

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// stampingStore wraps a MemStore and substitutes its own transaction instant
// for the log's proposal, the way a store shared between processes must. It
// stands in for the Postgres adapter in the root module's tests, so that the
// log's handling of a store-assigned transaction time is covered without a
// database.
type stampingStore struct {
	inner *MemStore
	mu    sync.Mutex
	next  time.Time
}

func newStampingStore(start time.Time) *stampingStore {
	return &stampingStore{inner: NewMemStore(), next: start}
}

func (s *stampingStore) Apply(ctx context.Context, w Write) (time.Time, error) {
	s.mu.Lock()
	s.next = s.next.Add(time.Hour)
	stamped := s.next
	s.mu.Unlock()

	w.TxAt = stamped
	for i := range w.Insert {
		w.Insert[i].TxFrom = stamped
	}
	return s.inner.Apply(ctx, w)
}

func (s *stampingStore) Get(ctx context.Context, q GetQuery) (*Record, error) {
	return s.inner.Get(ctx, q)
}

func (s *stampingStore) Query(ctx context.Context, q Query) ([]Record, Cursor, error) {
	return s.inner.Query(ctx, q)
}

// conflictStore fails a fixed number of writes with ErrConflict before letting
// them through, standing in for a rival writer that keeps winning the race.
type conflictStore struct {
	Store
	remaining int
	attempts  int
}

func (s *conflictStore) Apply(ctx context.Context, w Write) (time.Time, error) {
	s.attempts++
	if s.remaining > 0 {
		s.remaining--
		return time.Time{}, conflictf("record %s was already superseded", "elsewhere")
	}
	return s.Store.Apply(ctx, w)
}

func TestMemStore(t *testing.T) {
	ctx := context.Background()

	t.Run("given an empty store", func(t *testing.T) {
		t.Run("when it is inspected", func(t *testing.T) {
			s := NewMemStore()
			t.Run("then it holds nothing", func(t *testing.T) {
				if s.Len() != 0 {
					t.Fatalf("Len() = %d; want 0", s.Len())
				}
			})
			t.Run("then a lookup reports not found", func(t *testing.T) {
				_, err := s.Get(ctx, GetQuery{Kind: employee, EntityID: "e", ValidAt: t1, TxAt: t1})
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("Get = %v; want ErrNotFound", err)
				}
			})
			t.Run("then a query returns nothing and no cursor", func(t *testing.T) {
				recs, cursor, err := s.Query(ctx, Query{})
				if err != nil {
					t.Fatalf("Query failed: %v", err)
				}
				if len(recs) != 0 || !cursor.IsZero() {
					t.Fatalf("Query = %d records, cursor %q; want none", len(recs), cursor)
				}
			})
		})
	})

	t.Run("given a store with a record", func(t *testing.T) {
		s := NewMemStore()
		rec := Record{ID: "r1", Kind: employee, EntityID: "e", Data: []byte("v"), ValidFrom: t1, ValidTo: t3, TxFrom: t1, Actor: alice}
		seedRecords(t, s, []Record{rec})

		t.Run("when the same ID is inserted again", func(t *testing.T) {
			t.Run("then the original is kept rather than overwritten", func(t *testing.T) {
				dup := rec
				dup.Data = []byte("overwritten")
				if _, err := s.Apply(ctx, Write{Insert: []Record{dup}}); err != nil {
					t.Fatalf("Apply failed: %v", err)
				}
				if s.Len() != 1 {
					t.Fatalf("Len() = %d; want 1 — a duplicate ID must not add a row", s.Len())
				}
				got, err := s.Get(ctx, GetQuery{Kind: employee, EntityID: "e", ValidAt: t2, TxAt: t2})
				if err != nil {
					t.Fatalf("Get failed: %v", err)
				}
				if string(got.Data) != "v" {
					t.Fatalf("data = %s; an append-only log must not overwrite", got.Data)
				}
			})
		})

		t.Run("when it is superseded", func(t *testing.T) {
			if _, err := s.Apply(ctx, Write{Supersede: []RecordID{"r1"}, TxAt: t2}); err != nil {
				t.Fatalf("Apply failed: %v", err)
			}
			t.Run("then its transaction interval is closed", func(t *testing.T) {
				recs, _, err := s.Query(ctx, Query{})
				if err != nil {
					t.Fatalf("Query failed: %v", err)
				}
				if len(recs) != 1 || !recs[0].TxTo.Equal(t2) {
					t.Fatalf("TxTo = %v; want %s", recs[0].TxTo, t2)
				}
			})
			t.Run("then superseding again is idempotent", func(t *testing.T) {
				if _, err := s.Apply(ctx, Write{Supersede: []RecordID{"r1"}, TxAt: t4}); err != nil {
					t.Fatalf("Apply failed: %v", err)
				}
				recs, _, err := s.Query(ctx, Query{})
				if err != nil {
					t.Fatalf("Query failed: %v", err)
				}
				if !recs[0].TxTo.Equal(t2) {
					t.Fatalf("TxTo = %s; want the original %s — transaction time is never rewritten",
						recs[0].TxTo, t2)
				}
			})
			t.Run("then superseding an unknown ID is not an error", func(t *testing.T) {
				if _, err := s.Apply(ctx, Write{Supersede: []RecordID{"nope"}, TxAt: t4}); err != nil {
					t.Fatalf("Apply of an unknown ID = %v; want nil", err)
				}
			})
			t.Run("then the same supersession alongside an insert is a conflict", func(t *testing.T) {
				_, err := s.Apply(ctx, Write{
					Supersede: []RecordID{"r1"},
					TxAt:      t4,
					Insert:    []Record{{ID: "r2", Kind: employee, EntityID: "e", TxFrom: t4, Actor: alice}},
				})
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("Apply = %v; want ErrConflict — a split planned against a record "+
						"someone else already superseded must not land half-applied", err)
				}
				if s.Len() != 1 {
					t.Fatalf("Len() = %d; want 1 — a conflicting write must insert nothing", s.Len())
				}
			})
			t.Run("then an unknown supersession alongside an insert is a conflict too", func(t *testing.T) {
				_, err := s.Apply(ctx, Write{
					Supersede: []RecordID{"nope"},
					TxAt:      t4,
					Insert:    []Record{{ID: "r3", Kind: employee, EntityID: "e", TxFrom: t4, Actor: alice}},
				})
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("Apply = %v; want ErrConflict", err)
				}
			})
		})
	})

	t.Run("given a store that assigns transaction time itself", func(t *testing.T) {
		s := newStampingStore(t1)
		l := NewLog(s, WithClock(NewFixedClock(t0)))

		t.Run("when writes are made through a log", func(t *testing.T) {
			first, err := l.Put(ctx, employee, "e", []byte("v1"), t1, time.Time{}, alice)
			if err != nil {
				t.Fatalf("Put failed: %v", err)
			}
			second, err := l.Put(ctx, employee, "e", []byte("v2"), t1, time.Time{}, bob)
			if err != nil {
				t.Fatalf("Put failed: %v", err)
			}

			t.Run("then the log reports the store's instant, not its own clock", func(t *testing.T) {
				if !first.TxAt.Equal(t1.Add(time.Hour)) {
					t.Fatalf("TxAt = %s; want the store's %s", first.TxAt, t1.Add(time.Hour))
				}
				if !second.TxAt.After(first.TxAt) {
					t.Fatalf("TxAt did not advance: %s then %s", first.TxAt, second.TxAt)
				}
			})
			t.Run("then the returned records carry the store's instant", func(t *testing.T) {
				for _, r := range second.Written {
					if !r.TxFrom.Equal(second.TxAt) {
						t.Fatalf("record %s has TxFrom %s; want %s", r.ID, r.TxFrom, second.TxAt)
					}
				}
			})
			t.Run("then reads resolve now against the store's instant", func(t *testing.T) {
				got, err := l.Get(ctx, employee, "e", Now())
				if err != nil {
					t.Fatalf("Get failed: %v", err)
				}
				if string(got.Data) != "v2" {
					t.Fatalf("data = %s; want v2 — a read must see a write the store has "+
						"stamped ahead of the local clock", got.Data)
				}
			})
			t.Run("then the transaction axis is left without gaps or overlaps", func(t *testing.T) {
				assertInvariants(t, s.inner)
			})
		})
	})

	t.Run("given a store that reports conflicts", func(t *testing.T) {
		t.Run("when the conflicts stop within the retry budget", func(t *testing.T) {
			s := &conflictStore{Store: NewMemStore(), remaining: 2}
			l := NewLog(s, WithClock(NewFixedClock(t0)))
			t.Run("then the write eventually lands", func(t *testing.T) {
				if _, err := l.Put(ctx, employee, "e", []byte("v"), t1, t3, alice); err != nil {
					t.Fatalf("Put = %v; want the retry to succeed", err)
				}
				if s.attempts != 3 {
					t.Fatalf("attempts = %d; want 3", s.attempts)
				}
			})
		})

		t.Run("when the conflicts outlast the retry budget", func(t *testing.T) {
			s := &conflictStore{Store: NewMemStore(), remaining: 100}
			l := NewLog(s, WithClock(NewFixedClock(t0)), WithWriteRetries(2))
			t.Run("then the write reports a conflict naming the entity", func(t *testing.T) {
				_, err := l.Put(ctx, employee, "e", []byte("v"), t1, t3, alice)
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("Put = %v; want ErrConflict", err)
				}
				var ce *ConflictError
				if !errors.As(err, &ce) || ce.Attempts != 3 {
					t.Fatalf("error = %v; want a *ConflictError after 3 attempts", err)
				}
				if s.attempts != 3 {
					t.Fatalf("attempts = %d; want 3 — the budget is retries beyond the first try", s.attempts)
				}
			})
		})

		t.Run("when retries are disabled", func(t *testing.T) {
			s := &conflictStore{Store: NewMemStore(), remaining: 1}
			l := NewLog(s, WithClock(NewFixedClock(t0)), WithWriteRetries(-1))
			t.Run("then the first conflict is fatal", func(t *testing.T) {
				if _, err := l.Put(ctx, employee, "e", []byte("v"), t1, t3, alice); !errors.Is(err, ErrConflict) {
					t.Fatalf("Put = %v; want ErrConflict", err)
				}
				if s.attempts != 1 {
					t.Fatalf("attempts = %d; want 1", s.attempts)
				}
			})
		})
	})

	t.Run("given a store being closed", func(t *testing.T) {
		s := NewMemStore()
		seedRecords(t, s, []Record{{ID: "r1", Kind: employee, EntityID: "e", TxFrom: t1, Actor: alice}})
		if err := s.Close(); err != nil {
			t.Fatalf("Close failed: %v", err)
		}

		t.Run("when it is used afterwards", func(t *testing.T) {
			t.Run("then every operation reports ErrClosed", func(t *testing.T) {
				if _, err := s.Apply(ctx, Write{Insert: []Record{{ID: "r2"}}}); !errors.Is(err, ErrClosed) {
					t.Fatalf("Apply = %v; want ErrClosed", err)
				}
				if _, err := s.Apply(ctx, Write{Supersede: []RecordID{"r1"}, TxAt: t2}); !errors.Is(err, ErrClosed) {
					t.Fatalf("Apply = %v; want ErrClosed", err)
				}
				if _, err := s.Apply(ctx, Write{}); !errors.Is(err, ErrClosed) {
					t.Fatalf("Apply = %v; want ErrClosed", err)
				}
				if _, err := s.Get(ctx, GetQuery{}); !errors.Is(err, ErrClosed) {
					t.Fatalf("Get = %v; want ErrClosed", err)
				}
				if _, _, err := s.Query(ctx, Query{}); !errors.Is(err, ErrClosed) {
					t.Fatalf("Query = %v; want ErrClosed", err)
				}
			})
			t.Run("then closing again is not an error", func(t *testing.T) {
				if err := s.Close(); err != nil {
					t.Fatalf("second Close = %v; want nil", err)
				}
			})
		})
	})

	t.Run("given a cancelled context", func(t *testing.T) {
		s := NewMemStore()
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		t.Run("when the store is used", func(t *testing.T) {
			t.Run("then every operation reports the context error", func(t *testing.T) {
				if _, err := s.Apply(cancelled, Write{}); !errors.Is(err, context.Canceled) {
					t.Fatalf("Apply = %v; want context.Canceled", err)
				}
				if _, err := s.Get(cancelled, GetQuery{}); !errors.Is(err, context.Canceled) {
					t.Fatalf("Get = %v; want context.Canceled", err)
				}
				if _, _, err := s.Query(cancelled, Query{}); !errors.Is(err, context.Canceled) {
					t.Fatalf("Query = %v; want context.Canceled", err)
				}
			})
		})
	})

	t.Run("given a query with a malformed filter", func(t *testing.T) {
		s := NewMemStore()
		t.Run("when it is run", func(t *testing.T) {
			t.Run("then an inverted valid range is rejected", func(t *testing.T) {
				_, _, err := s.Query(ctx, Query{Valid: Between(t3, t1)})
				if !errors.Is(err, ErrInvalidInterval) {
					t.Fatalf("Query = %v; want ErrInvalidInterval", err)
				}
				var ie *IntervalError
				if !errors.As(err, &ie) || ie.Field != "valid" {
					t.Fatalf("error = %v; want an *IntervalError naming the valid axis", err)
				}
			})
			t.Run("then an inverted transaction range is rejected", func(t *testing.T) {
				_, _, err := s.Query(ctx, Query{Tx: Between(t3, t1)})
				if !errors.Is(err, ErrInvalidInterval) {
					t.Fatalf("Query = %v; want ErrInvalidInterval", err)
				}
				var ie *IntervalError
				if !errors.As(err, &ie) || ie.Field != "transaction" {
					t.Fatalf("error = %v; want an *IntervalError naming the transaction axis", err)
				}
			})
			t.Run("then an undefined intent is rejected", func(t *testing.T) {
				_, _, err := s.Query(ctx, Query{Intent: Intent(200), HasIntent: true})
				if !errors.Is(err, ErrUnknownKind) {
					t.Fatalf("Query = %v; want an error", err)
				}
			})
		})
	})

	t.Run("given records across several kinds and actors", func(t *testing.T) {
		s := NewMemStore()
		seedRecords(t, s, []Record{
			{ID: "a", Kind: "employee", EntityID: "e1", TxFrom: t1, ValidFrom: t1, ValidTo: t3, Actor: alice, Intent: IntentAssert},
			{ID: "b", Kind: "employee", EntityID: "e2", TxFrom: t2, ValidFrom: t2, ValidTo: t4, Actor: bob, Intent: IntentCorrection},
			{ID: "c", Kind: "invoice", EntityID: "i1", TxFrom: t3, ValidFrom: t3, Actor: alice, Intent: IntentAssert, TxTo: t5},
		})

		filters := []struct {
			name string
			q    Query
			want []RecordID
		}{
			{"no filter", Query{}, []RecordID{"a", "b", "c"}},
			{"by kind", Query{Kind: "employee"}, []RecordID{"a", "b"}},
			{"by kind and entity", Query{Kind: "employee", EntityID: "e2"}, []RecordID{"b"}},
			{"by actor", Query{ActorID: alice.ID}, []RecordID{"a", "c"}},
			{"by intent", Query{Intent: IntentCorrection, HasIntent: true}, []RecordID{"b"}},
			{"current only", Query{CurrentOnly: true}, []RecordID{"a", "b"}},
			{"by valid instant", Query{ValidAt: t2}, []RecordID{"a", "b"}},
			{"by transaction instant", Query{TxAt: t2}, []RecordID{"a", "b"}},
			{"by valid range", Query{Valid: Between(t3, t5)}, []RecordID{"b", "c"}},
			{"by transaction range", Query{Tx: Between(t0, t2)}, []RecordID{"a"}},
			{"by kind and actor together", Query{Kind: "employee", ActorID: bob.ID}, []RecordID{"b"}},
			{"matching nothing", Query{Kind: "nonexistent"}, nil},
		}

		for _, tc := range filters {
			t.Run("when filtered "+tc.name, func(t *testing.T) {
				t.Run("then the expected records are returned", func(t *testing.T) {
					recs, _, err := s.Query(ctx, tc.q)
					if err != nil {
						t.Fatalf("Query failed: %v", err)
					}
					got := make([]RecordID, len(recs))
					for i, r := range recs {
						got[i] = r.ID
					}
					if len(got) != len(tc.want) {
						t.Fatalf("filter %s returned %v; want %v", tc.name, got, tc.want)
					}
					for i := range got {
						if got[i] != tc.want[i] {
							t.Fatalf("filter %s returned %v; want %v", tc.name, got, tc.want)
						}
					}
				})
			})
		}
	})

	t.Run("given a store reached only through the Store interface", func(t *testing.T) {
		// The log holds a Store and nothing more: there is no longer an
		// optional atomic extension to detect, and so no path by which a write
		// can be split into two calls.
		var store Store = NewMemStore()
		l := NewLog(store, WithClock(NewFixedClock(t0)))

		t.Run("when writes are made through it", func(t *testing.T) {
			if _, err := l.Put(ctx, employee, "e", []byte("v1"), t1, time.Time{}, alice); err != nil {
				t.Fatalf("Put failed: %v", err)
			}
			if _, err := l.Put(ctx, employee, "e", []byte("v2"), t2, t3, bob); err != nil {
				t.Fatalf("Put failed: %v", err)
			}

			t.Run("then the split produces a correct tiling", func(t *testing.T) {
				want := []string{
					"[2026-02-01T00:00:00Z, 2026-03-01T00:00:00Z)=v1",
					"[2026-03-01T00:00:00Z, 2026-04-01T00:00:00Z)=v2",
					"[2026-04-01T00:00:00Z, ∞)=v1",
				}
				if got := currentSegments(t, l, "e"); !equalStrings(got, want) {
					t.Fatalf("tiling:\n got %v\nwant %v", got, want)
				}
			})
			t.Run("then every invariant still holds", func(t *testing.T) {
				assertInvariants(t, store.(*MemStore))
			})
		})
	})

	t.Run("given concurrent readers and writers on the store itself", func(t *testing.T) {
		s := NewMemStore()
		var wg sync.WaitGroup
		errCh := make(chan error, 64)

		for w := 0; w < 6; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < 50; i++ {
					id := RecordID(fmt.Sprintf("w%d-%d", w, i))
					_, err := s.Apply(context.Background(), Write{
						TxAt: t2,
						Insert: []Record{{
							ID: id, Kind: employee, EntityID: fmt.Sprintf("e%d", i%3),
							Data: []byte("v"), ValidFrom: t1, ValidTo: t3, TxFrom: t1, Actor: alice,
						}},
					})
					if err != nil {
						errCh <- err
						return
					}
				}
			}(w)
		}
		for r := 0; r < 4; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 50; i++ {
					if _, _, err := s.Query(context.Background(), Query{Limit: 5}); err != nil {
						errCh <- err
						return
					}
					if _, err := s.Get(context.Background(), GetQuery{Kind: employee, EntityID: "e0", ValidAt: t2, TxAt: t1}); err != nil && !errors.Is(err, ErrNotFound) {
						errCh <- err
						return
					}
					_ = s.Len()
				}
			}()
		}
		wg.Wait()
		close(errCh)

		t.Run("when they have all finished", func(t *testing.T) {
			t.Run("then no operation failed", func(t *testing.T) {
				for err := range errCh {
					t.Fatalf("concurrent store operation failed: %v", err)
				}
			})
			t.Run("then every record landed exactly once", func(t *testing.T) {
				if s.Len() != 6*50 {
					t.Fatalf("Len() = %d; want %d", s.Len(), 6*50)
				}
			})
		})
	})
}

func TestWriteEntities(t *testing.T) {
	t.Run("given a write touching several entities", func(t *testing.T) {
		w := Write{Insert: []Record{
			{Kind: "invoice", EntityID: "i1"},
			{Kind: "employee", EntityID: "e2"},
			{Kind: "employee", EntityID: "e1"},
			{Kind: "employee", EntityID: "e2"},
		}}

		t.Run("when its entities are listed", func(t *testing.T) {
			got := w.Entities()
			t.Run("then each appears once, in a deterministic order", func(t *testing.T) {
				want := []EntityRef{
					{Kind: "employee", EntityID: "e1"},
					{Kind: "employee", EntityID: "e2"},
					{Kind: "invoice", EntityID: "i1"},
				}
				if len(got) != len(want) {
					t.Fatalf("Entities() = %v; want %v", got, want)
				}
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("Entities() = %v; want %v", got, want)
					}
				}
			})
		})
	})

	t.Run("given a write that inserts nothing", func(t *testing.T) {
		t.Run("when its entities are listed", func(t *testing.T) {
			t.Run("then the list is empty, because an ID does not name its entity", func(t *testing.T) {
				if got := (Write{Supersede: []RecordID{"r1"}}).Entities(); len(got) != 0 {
					t.Fatalf("Entities() = %v; want none", got)
				}
			})
		})
	})
}

func TestConflictErrorFormatting(t *testing.T) {
	t.Run("given a conflict reported straight from a store", func(t *testing.T) {
		err := conflictf("record %s was already superseded", "r1")
		t.Run("when it is rendered", func(t *testing.T) {
			t.Run("then it names the record and no attempt count", func(t *testing.T) {
				want := "chronicle: write conflict: record r1 was already superseded"
				if err.Error() != want {
					t.Fatalf("Error() = %q; want %q", err, want)
				}
			})
			t.Run("then it matches ErrConflict", func(t *testing.T) {
				if !errors.Is(err, ErrConflict) {
					t.Fatal("a ConflictError should match ErrConflict")
				}
			})
		})
	})

	t.Run("given a conflict raised after exhausting retries", func(t *testing.T) {
		inner := conflictf("record r1 was already superseded")
		err := &ConflictError{Reason: "lost the race for employee/e", Attempts: 3, Err: inner}
		t.Run("when it is rendered", func(t *testing.T) {
			t.Run("then it reports the attempts and the underlying reason", func(t *testing.T) {
				want := "chronicle: write conflict: lost the race for employee/e (after 3 attempts): " +
					"chronicle: write conflict: record r1 was already superseded"
				if err.Error() != want {
					t.Fatalf("Error() = %q; want %q", err, want)
				}
			})
			t.Run("then the store's own error stays reachable", func(t *testing.T) {
				if errors.Unwrap(err) != inner {
					t.Fatal("the wrapped store error should remain reachable")
				}
			})
		})
	})
}

func TestAsHelpers(t *testing.T) {
	t.Run("given the As constructors", func(t *testing.T) {
		t.Run("when Now is used", func(t *testing.T) {
			t.Run("then both axes are unset", func(t *testing.T) {
				if !Now().IsZero() {
					t.Fatal("Now() should be the zero As")
				}
			})
		})
		t.Run("when ValidAt is used", func(t *testing.T) {
			t.Run("then only the valid axis is pinned", func(t *testing.T) {
				a := ValidAt(t2)
				if !a.ValidAt.Equal(t2) || !a.TxAt.IsZero() || a.IsZero() {
					t.Fatalf("ValidAt(%s) = %+v", t2, a)
				}
			})
		})
		t.Run("when AsOf is used", func(t *testing.T) {
			t.Run("then both axes are pinned to the same instant", func(t *testing.T) {
				a := AsOf(t2)
				if !a.ValidAt.Equal(t2) || !a.TxAt.Equal(t2) {
					t.Fatalf("AsOf(%s) = %+v", t2, a)
				}
			})
		})
		t.Run("when an As is resolved", func(t *testing.T) {
			t.Run("then unset axes become the given instant", func(t *testing.T) {
				got := As{}.resolve(t3)
				if !got.ValidAt.Equal(t3) || !got.TxAt.Equal(t3) {
					t.Fatalf("resolve = %+v; want both axes at %s", got, t3)
				}
			})
			t.Run("then set axes are normalised to UTC and otherwise left alone", func(t *testing.T) {
				zone := time.FixedZone("test", 3*3600)
				got := As{ValidAt: t1.In(zone), TxAt: t2.In(zone)}.resolve(t3)
				if !got.ValidAt.Equal(t1) || !got.TxAt.Equal(t2) {
					t.Fatalf("resolve changed the instants: %+v", got)
				}
				if got.ValidAt.Location() != time.UTC || got.TxAt.Location() != time.UTC {
					t.Fatal("resolve did not normalise to UTC")
				}
			})
		})
	})
}

func TestActorAndRecordHelpers(t *testing.T) {
	t.Run("given an actor", func(t *testing.T) {
		t.Run("when it carries no identity at all", func(t *testing.T) {
			t.Run("then it is zero", func(t *testing.T) {
				if !(Actor{}).IsZero() {
					t.Fatal("the empty Actor should be zero")
				}
			})
		})
		t.Run("when it carries any field", func(t *testing.T) {
			t.Run("then it is not zero", func(t *testing.T) {
				for _, a := range []Actor{{ID: "x"}, {Type: "user"}, {Name: "Alice"}} {
					if a.IsZero() {
						t.Fatalf("Actor %+v reported zero", a)
					}
				}
			})
		})
	})

	t.Run("given a record", func(t *testing.T) {
		rec := Record{ID: "r", Data: []byte("abc"), Meta: map[string]string{"k": "v"}, TxFrom: t1}

		t.Run("when it is cloned", func(t *testing.T) {
			c := rec.Clone()
			c.Data[0] = 'X'
			c.Meta["k"] = "changed"
			t.Run("then the original is untouched", func(t *testing.T) {
				if string(rec.Data) != "abc" || rec.Meta["k"] != "v" {
					t.Fatal("Clone shares mutable state with the original")
				}
			})
		})

		t.Run("when it has no metadata", func(t *testing.T) {
			t.Run("then cloning leaves the map nil", func(t *testing.T) {
				c := Record{ID: "r"}.Clone()
				if c.Meta != nil {
					t.Fatal("Clone invented a metadata map")
				}
			})
		})

		t.Run("when its currency is checked", func(t *testing.T) {
			t.Run("then an open transaction interval is current", func(t *testing.T) {
				if !rec.IsCurrent() {
					t.Fatal("a record with a zero TxTo should be current")
				}
			})
			t.Run("then a closed transaction interval is not", func(t *testing.T) {
				closed := rec
				closed.TxTo = t2
				if closed.IsCurrent() {
					t.Fatal("a record with a TxTo should not be current")
				}
			})
		})

		t.Run("when a nil slice is cloned", func(t *testing.T) {
			t.Run("then the result is nil", func(t *testing.T) {
				if cloneRecords(nil) != nil {
					t.Fatal("cloneRecords(nil) should be nil")
				}
			})
		})
	})

	t.Run("given the internal integer formatter", func(t *testing.T) {
		t.Run("when values are rendered", func(t *testing.T) {
			t.Run("then they match strconv", func(t *testing.T) {
				cases := map[int]string{0: "0", 1: "1", 9: "9", 10: "10", 4095: "4095", -7: "-7"}
				for in, want := range cases {
					if got := itoa(in); got != want {
						t.Fatalf("itoa(%d) = %q; want %q", in, got, want)
					}
				}
			})
		})
	})

	t.Run("given the internal zero padder", func(t *testing.T) {
		t.Run("when values are padded", func(t *testing.T) {
			t.Run("then short values gain leading zeros", func(t *testing.T) {
				if got := pad(7, 4); got != "0007" {
					t.Fatalf("pad(7, 4) = %q; want 0007", got)
				}
			})
			t.Run("then values at or beyond the width are unchanged", func(t *testing.T) {
				if got := pad(12345, 4); got != "12345" {
					t.Fatalf("pad(12345, 4) = %q; want 12345", got)
				}
			})
		})
	})
}
