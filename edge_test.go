package chronicle

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The paths exercised here are reachable through the public API but not through
// the ordinary happy path: option guards, timezone normalisation, and what the
// log does when the store underneath it misbehaves. They are the branches a
// reader would otherwise have to take on trust.

// nullCodec decodes anything to a nil map, which a codec is permitted to do and
// which the log has to normalise: a nil map and an empty one must diff the same
// way, or "the record decoded to nothing" and "the record has no fields" would
// be different answers.
type nullCodec struct{}

func (nullCodec) Name() string { return "null" }

func (nullCodec) Decode([]byte) (map[string]any, error) { return nil, nil }

// stubStore is a store that answers however a test needs it to. It exists to
// drive the log's error paths, which no correct store reaches.
type stubStore struct {
	*MemStore
	getErr   error
	queryErr error
	applyErr error
	rows     []Record

	// getFailAfter makes Get succeed that many times and fail thereafter, for
	// driving the second of two reads.
	getFailAfter int
	gets         int

	// zeroTxAt makes Apply succeed but report no instant, which the log has to
	// fall back from rather than let its ratchet slide to zero.
	zeroTxAt bool

	// cancelAfter cancels the context once Apply has been called that many
	// times, for interrupting a retry loop mid-backoff.
	cancelAfter int
	cancel      context.CancelFunc
	applies     int
}

func (s *stubStore) Get(ctx context.Context, q GetQuery) (*Record, error) {
	s.gets++
	if s.getErr != nil && s.gets > s.getFailAfter {
		return nil, s.getErr
	}
	return s.MemStore.Get(ctx, q)
}

func (s *stubStore) Apply(ctx context.Context, req ApplyRequest) (time.Time, error) {
	s.applies++
	if s.cancelAfter > 0 && s.applies >= s.cancelAfter && s.cancel != nil {
		s.cancel()
	}
	if s.applyErr != nil {
		return time.Time{}, s.applyErr
	}
	txAt, err := s.MemStore.Apply(ctx, req)
	if err != nil || !s.zeroTxAt {
		return txAt, err
	}
	return time.Time{}, nil
}

func (s *stubStore) Query(ctx context.Context, q Query) ([]Record, Cursor, error) {
	if s.queryErr != nil {
		return nil, NoCursor, s.queryErr
	}
	if s.rows != nil {
		return cloneRecords(s.rows), NoCursor, nil
	}
	return s.MemStore.Query(ctx, q)
}

func TestOptionGuards(t *testing.T) {
	t.Run("given a log built with degenerate options", func(t *testing.T) {
		t.Run("when a nil codec is supplied", func(t *testing.T) {
			t.Run("then the default survives rather than being replaced with nothing", func(t *testing.T) {
				l := NewLog(NewMemStore(), WithCodec(nil))
				if l.Codec() == nil {
					t.Fatal("WithCodec(nil) cleared the default codec; a nil codec makes Diff " +
						"fail on every record instead of doing nothing")
				}
			})
		})

		t.Run("when a codec is supplied", func(t *testing.T) {
			t.Run("then it replaces the default", func(t *testing.T) {
				l := NewLog(NewMemStore(), WithCodec(nullCodec{}))
				if l.Codec().Name() != "null" {
					t.Fatalf("Codec() = %s; want the one supplied", l.Codec().Name())
				}
			})
		})
	})

	t.Run("given degenerate write options", func(t *testing.T) {
		ctx := context.Background()

		t.Run("when metadata is supplied as an empty map", func(t *testing.T) {
			t.Run("then the record's metadata stays absent rather than becoming empty", func(t *testing.T) {
				l, _, _ := newTestLog(t)
				res, err := l.Put(ctx, employee, "e1", []byte(`{"a":1}`), t0, t1, alice,
					WithMeta(nil), WithMeta(map[string]string{}))
				if err != nil {
					t.Fatalf("Put failed: %v", err)
				}
				if len(res.Record.Meta) != 0 {
					t.Fatalf("Meta = %v; want nothing", res.Record.Meta)
				}
			})
		})

		t.Run("when a metadata value is supplied under an empty key", func(t *testing.T) {
			t.Run("then it is dropped rather than stored under one", func(t *testing.T) {
				l, _, _ := newTestLog(t)
				res, err := l.Put(ctx, employee, "e1", []byte(`{"a":1}`), t0, t1, alice,
					WithMetaValue("", "orphan"), WithMetaValue("ticket", "HR-1"))
				if err != nil {
					t.Fatalf("Put failed: %v", err)
				}
				meta := res.Record.Meta
				if _, ok := meta[""]; ok {
					t.Fatalf("Meta = %v; a value under an empty key is unreachable by any reader", meta)
				}
				if meta["ticket"] != "HR-1" {
					t.Fatalf("Meta = %v; want the well-formed entry kept", meta)
				}
			})
		})
	})
}

func TestQueryNormalisesInstantsToUTC(t *testing.T) {
	t.Run("given a query whose instants carry a non-UTC location", func(t *testing.T) {
		l, _, _ := newTestLog(t)
		ctx := context.Background()
		mustPut(t, l, "e1", `{"a":1}`, t0, t1)

		zone := time.FixedZone("UTC+7", 7*3600)

		t.Run("when it is run", func(t *testing.T) {
			recs, _, err := l.Query(ctx, Query{
				Kind:    employee,
				ValidAt: t0.Add(time.Hour).In(zone),
				TxAt:    t0.In(zone),
			})
			t.Run("then the location does not change which records match", func(t *testing.T) {
				if err != nil {
					t.Fatalf("Query failed: %v", err)
				}
				if len(recs) != 1 {
					t.Fatalf("records = %d; want 1 — an instant is an instant whatever zone it "+
						"was written in", len(recs))
				}
			})
		})
	})
}

func TestDiffWithACodecThatDecodesToNothing(t *testing.T) {
	t.Run("given a log whose codec decodes every record to a nil map", func(t *testing.T) {
		store := NewMemStore()
		clock := NewFixedClock(t0)
		l := NewLog(store, WithClock(clock), WithCodec(nullCodec{}))
		ctx := context.Background()

		first, err := l.Put(ctx, employee, "e1", []byte(`{"a":1}`), t0, t1, alice)
		if err != nil {
			t.Fatalf("Put failed: %v", err)
		}
		clock.Advance(time.Hour)
		second, err := l.Correct(ctx, employee, "e1", []byte(`{"a":2}`), t0, t1, bob)
		if err != nil {
			t.Fatalf("Correct failed: %v", err)
		}

		t.Run("when two beliefs are diffed", func(t *testing.T) {
			delta, err := l.Diff(ctx, employee, "e1",
				As{ValidAt: t0, TxAt: first.TxAt}, As{ValidAt: t0, TxAt: second.TxAt})
			t.Run("then a nil decode is treated as no fields rather than as a failure", func(t *testing.T) {
				if err != nil {
					t.Fatalf("Diff = %v; a codec returning a nil map has decoded nothing, not failed", err)
				}
				if !delta.IsEmpty() {
					t.Fatalf("changes = %+v; want none, since neither side decoded any fields", delta.Changes)
				}
			})
		})
	})
}

func TestLogSurfacesStoreFailures(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("the database is not there")

	t.Run("given a store that cannot answer a query", func(t *testing.T) {
		l := NewLog(&stubStore{MemStore: NewMemStore(), queryErr: boom})

		for _, tc := range []struct {
			name string
			call func() error
		}{
			{"History", func() error { _, err := l.History(ctx, employee, "e1"); return err }},
			{"Timeline", func() error { _, err := l.Timeline(ctx, employee, "e1", Now()); return err }},
			{"Query", func() error { _, _, err := l.Query(ctx, Query{}); return err }},
		} {
			t.Run("when "+tc.name+" is called", func(t *testing.T) {
				t.Run("then the store's error is surfaced rather than read as an empty result", func(t *testing.T) {
					if err := tc.call(); !errors.Is(err, boom) {
						t.Fatalf("%s = %v; want the store's error — reporting emptiness for a "+
							"failed read is how a log silently loses history", tc.name, err)
					}
				})
			})
		}
	})

	t.Run("given a store that cannot answer a lookup", func(t *testing.T) {
		l := NewLog(&stubStore{MemStore: NewMemStore(), getErr: boom})

		t.Run("when a diff needs one of its two operands", func(t *testing.T) {
			t.Run("then the store's error is surfaced rather than read as absence", func(t *testing.T) {
				_, err := l.Diff(ctx, employee, "e1", Now(), Now())
				if !errors.Is(err, boom) {
					t.Fatalf("Diff = %v; want the store's error — treating a failed read as "+
						"'the entity did not exist' would report every field as added", err)
				}
			})
		})
	})

	t.Run("given a store that returns two records sharing a valid start", func(t *testing.T) {
		// Only a broken store can do this, but Timeline still has to be
		// deterministic about it rather than ordering by whatever came back.
		store := &stubStore{MemStore: NewMemStore(), rows: []Record{
			{ID: "r-b", Kind: employee, EntityID: "e1", Data: []byte("b"), ValidFrom: t0, TxFrom: t0},
			{ID: "r-a", Kind: employee, EntityID: "e1", Data: []byte("a"), ValidFrom: t0, TxFrom: t0},
		}}
		l := NewLog(store)

		t.Run("when the timeline is read", func(t *testing.T) {
			recs, err := l.Timeline(ctx, employee, "e1", AsOf(t1))
			t.Run("then the tie is broken by the total order rather than left to the store", func(t *testing.T) {
				if err != nil {
					t.Fatalf("Timeline failed: %v", err)
				}
				if len(recs) != 2 || recs[0].ID != "r-a" || recs[1].ID != "r-b" {
					t.Fatalf("timeline = %s then %s; want r-a then r-b", recs[0].ID, recs[1].ID)
				}
			})
		})
	})
}

func TestMemStoreRejectsARequestWithNoPlan(t *testing.T) {
	t.Run("given an apply request carrying no planner", func(t *testing.T) {
		s := NewMemStore()
		t.Run("when it is applied", func(t *testing.T) {
			_, err := s.Apply(context.Background(), ApplyRequest{TxAt: t0})
			t.Run("then it is rejected rather than treated as an empty write", func(t *testing.T) {
				if err == nil {
					t.Fatal("Apply with no Plan = nil; a request that computes nothing is a " +
						"caller bug, not a no-op")
				}
			})
		})
	})
}

func TestMemStoreSurfacesPlannerFailures(t *testing.T) {
	t.Run("given a planner that fails", func(t *testing.T) {
		s := NewMemStore()
		boom := errors.New("the plan could not be computed")

		t.Run("when the write is applied", func(t *testing.T) {
			_, err := s.Apply(context.Background(), ApplyRequest{
				TxAt: t0,
				Plan: func([]Record, time.Time) (Write, error) { return Write{}, boom },
			})
			t.Run("then the error is returned unchanged", func(t *testing.T) {
				if !errors.Is(err, boom) {
					t.Fatalf("Apply = %v; want the planner's error", err)
				}
			})
			t.Run("then nothing was written", func(t *testing.T) {
				if n := s.Len(); n != 0 {
					t.Fatalf("store holds %d records; want none", n)
				}
			})
		})
	})
}

func TestQueryMatchesOnEntityID(t *testing.T) {
	t.Run("given a query pinned to one entity", func(t *testing.T) {
		q := Query{Kind: employee, EntityID: "e1"}
		t.Run("when a record for another entity of the same kind is tested", func(t *testing.T) {
			t.Run("then it does not match", func(t *testing.T) {
				if q.Matches(Record{Kind: employee, EntityID: "e2"}) {
					t.Fatal("Matches = true for another entity; entity IDs are scoped by kind, " +
						"not shared across it")
				}
			})
		})
		t.Run("when a record for the named entity is tested", func(t *testing.T) {
			t.Run("then it matches", func(t *testing.T) {
				if !q.Matches(Record{Kind: employee, EntityID: "e1"}) {
					t.Fatal("Matches = false for the entity the query named")
				}
			})
		})
	})
}

func TestWriteRetryPolicy(t *testing.T) {
	t.Run("given a store whose Apply fails for a reason that is not a conflict", func(t *testing.T) {
		boom := errors.New("the disk is full")
		l := NewLog(&stubStore{MemStore: NewMemStore(), applyErr: boom})

		t.Run("when a write is attempted", func(t *testing.T) {
			_, err := l.Put(context.Background(), employee, "e1", []byte(`{"a":1}`), t0, t1, alice)
			t.Run("then it is returned rather than retried", func(t *testing.T) {
				if !errors.Is(err, boom) {
					t.Fatalf("Put = %v; want the store's error", err)
				}
				if errors.Is(err, ErrConflict) {
					t.Fatal("a plain store failure was reported as a conflict; only a conflict " +
						"is worth recomputing the write for")
				}
			})
		})
	})

	t.Run("given a store that loses every race", func(t *testing.T) {
		conflict := &ConflictError{Reason: "someone else got there"}

		t.Run("when a write exhausts its retries", func(t *testing.T) {
			l := NewLog(&stubStore{MemStore: NewMemStore(), applyErr: conflict}, WithWriteRetries(8))
			_, err := l.Put(context.Background(), employee, "e1", []byte(`{"a":1}`), t0, t1, alice)

			t.Run("then it gives up as a conflict, counting the attempts", func(t *testing.T) {
				var ce *ConflictError
				if !errors.As(err, &ce) {
					t.Fatalf("Put = %v; want a *ConflictError", err)
				}
				if ce.Attempts != 9 {
					t.Fatalf("Attempts = %d; want 9 — the count is what tells an operator the "+
						"difference between contention and a stuck writer", ce.Attempts)
				}
			})
			t.Run("then it kept the store's own error reachable", func(t *testing.T) {
				if !errors.Is(err, conflict) {
					t.Fatalf("Put = %v; want the store's conflict still wrapped", err)
				}
			})
		})

		t.Run("when the context is cancelled part-way through the retries", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store := &stubStore{MemStore: NewMemStore(), applyErr: conflict, cancelAfter: 1, cancel: cancel}
			l := NewLog(store, WithWriteRetries(100))

			_, err := l.Put(ctx, employee, "e1", []byte(`{"a":1}`), t0, t1, alice)

			t.Run("then the retry loop stops on the context rather than running to its limit", func(t *testing.T) {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Put = %v; want context.Canceled — a caller who has given up must "+
						"not be kept waiting through the whole backoff schedule", err)
				}
				if store.applies > 3 {
					t.Fatalf("the store was called %d times after cancellation; want it to stop "+
						"almost immediately", store.applies)
				}
			})
		})
	})
}

func TestTransactionRatchetSurvivesAStoreThatReportsNothing(t *testing.T) {
	t.Run("given a store whose Apply succeeds but reports no instant", func(t *testing.T) {
		clock := NewFixedClock(t0)
		l := NewLog(&stubStore{MemStore: NewMemStore(), zeroTxAt: true}, WithClock(clock))

		t.Run("when a write lands", func(t *testing.T) {
			res, err := l.Put(context.Background(), employee, "e1", []byte(`{"a":1}`), t0, t1, alice)
			t.Run("then the log falls back to the instant it proposed", func(t *testing.T) {
				if err != nil {
					t.Fatalf("Put failed: %v", err)
				}
				if res.TxAt.IsZero() {
					t.Fatal("Result.TxAt is the zero time; a caller has no coordinate to read " +
						"its own write back at, and the ratchet has slid backwards")
				}
			})
		})
	})
}

func TestDiffSurfacesAFailureOnItsSecondOperand(t *testing.T) {
	t.Run("given a store that answers one lookup and then fails", func(t *testing.T) {
		boom := errors.New("the connection dropped")
		store := &stubStore{MemStore: NewMemStore(), getErr: boom, getFailAfter: 1}
		clock := NewFixedClock(t0)
		l := NewLog(store, WithClock(clock))

		if _, err := l.Put(context.Background(), employee, "e1", []byte(`{"a":1}`), t0, t1, alice); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
		store.gets = 0

		t.Run("when two beliefs are diffed", func(t *testing.T) {
			_, err := l.Diff(context.Background(), employee, "e1",
				As{ValidAt: t0, TxAt: t0}, As{ValidAt: t0, TxAt: t1})
			t.Run("then the second read's failure is surfaced too", func(t *testing.T) {
				if !errors.Is(err, boom) {
					t.Fatalf("Diff = %v; want the store's error — a diff that silently treats "+
						"its second operand as absent reports every field as removed", err)
				}
			})
		})
	})
}

// planSkippingStore reports a successful Apply without ever invoking the plan,
// which the Store contract forbids: Apply computes and applies the plan, and a
// store that skipped it has written nothing the log asked for.
type planSkippingStore struct{ *MemStore }

func (s *planSkippingStore) Apply(context.Context, ApplyRequest) (time.Time, error) {
	return t1, nil
}

func TestLogRejectsAStoreThatSkipsThePlan(t *testing.T) {
	t.Run("given a store whose Apply succeeds without executing the plan", func(t *testing.T) {
		l := NewLog(&planSkippingStore{MemStore: NewMemStore()})

		t.Run("when a write is attempted", func(t *testing.T) {
			var res Result
			var err error
			func() {
				defer func() {
					if p := recover(); p != nil {
						t.Fatalf("Put panicked: %v — a misbehaving store must surface as an error, "+
							"not as an index panic blamed on the log", p)
					}
				}()
				res, err = l.Put(context.Background(), employee, "e1", []byte(`{"a":1}`), t0, t1, alice)
			}()
			t.Run("then the contract violation is reported as an error", func(t *testing.T) {
				if err == nil {
					t.Fatalf("Put = %+v, nil; want an error naming the store's contract violation", res)
				}
				if !strings.Contains(err.Error(), "did not execute the plan") {
					t.Fatalf("Put = %v; want the error to say the store did not execute the plan", err)
				}
			})
		})
	})
}

func TestZeroClockCannotDisableTheRatchet(t *testing.T) {
	t.Run("given a log whose clock reports the zero instant", func(t *testing.T) {
		ctx := context.Background()
		store := NewMemStore()
		l := NewLog(store, WithClock(&FixedClock{}))

		t.Run("when overlapping writes land", func(t *testing.T) {
			first, err := l.Put(ctx, employee, "e1", []byte(`{"v":1}`), t1, t3, alice)
			if err != nil {
				t.Fatalf("first Put failed: %v", err)
			}
			second, err := l.Put(ctx, employee, "e1", []byte(`{"v":2}`), t1, t3, alice)
			if err != nil {
				t.Fatalf("second Put failed: %v", err)
			}

			t.Run("then every write still gets a nonzero transaction instant", func(t *testing.T) {
				if first.TxAt.IsZero() || second.TxAt.IsZero() {
					t.Fatalf("TxAt = %s then %s; a zero transaction instant stamps TxFrom as "+
						"'always believed' and makes a supersession's TxTo read as current",
						first.TxAt, second.TxAt)
				}
			})
			t.Run("then the instants are strictly increasing", func(t *testing.T) {
				if !second.TxAt.After(first.TxAt) {
					t.Fatalf("TxAt did not advance: %s then %s", first.TxAt, second.TxAt)
				}
			})
			t.Run("then the second write actually superseded the first", func(t *testing.T) {
				recs, _, err := store.Query(ctx, Query{Kind: employee, EntityID: "e1", CurrentOnly: true})
				if err != nil {
					t.Fatalf("Query failed: %v", err)
				}
				if len(recs) != 1 {
					t.Fatalf("%d current records for one fully overlapping interval; want exactly 1 — "+
						"overlapping current belief is the invariant the library exists to hold", len(recs))
				}
				if string(recs[0].Data) != `{"v":2}` {
					t.Fatalf("current data = %s; want the second write's", recs[0].Data)
				}
			})
			t.Run("then the whole log still satisfies the invariants", func(t *testing.T) {
				assertInvariants(t, store)
			})
		})
	})
}

func TestWriteRejectsMetadataNoStoreCanHold(t *testing.T) {
	ctx := context.Background()

	t.Run("given metadata containing a NUL byte", func(t *testing.T) {
		cases := []struct {
			name string
			opt  WriteOption
		}{
			{"in a key", WithMetaValue("bad\x00key", "v")},
			{"in a value", WithMetaValue("k", "bad\x00value")},
			{"via WithMeta", WithMeta(map[string]string{"k": "\x00"})},
		}
		for _, tc := range cases {
			t.Run("when a write carries one "+tc.name, func(t *testing.T) {
				l, store, _ := newTestLog(t)
				_, err := l.Put(ctx, employee, "e1", []byte(`{"a":1}`), t0, t1, alice, tc.opt)
				t.Run("then it is rejected with ErrInvalidMeta", func(t *testing.T) {
					if !errors.Is(err, ErrInvalidMeta) {
						t.Fatalf("Put = %v; want ErrInvalidMeta — jsonb cannot hold a NUL, so "+
							"accepting this write would make MemStore and pgstore disagree", err)
					}
				})
				t.Run("then nothing was written", func(t *testing.T) {
					if n := store.Len(); n != 0 {
						t.Fatalf("store holds %d records; want none", n)
					}
				})
			})
		}
	})

	t.Run("given metadata that is merely unusual", func(t *testing.T) {
		t.Run("when a write carries control characters short of NUL", func(t *testing.T) {
			l, _, _ := newTestLog(t)
			_, err := l.Put(ctx, employee, "e1", []byte(`{"a":1}`), t0, t1, alice,
				WithMetaValue("k", "tab\tand\nnewline"))
			t.Run("then it is accepted", func(t *testing.T) {
				if err != nil {
					t.Fatalf("Put = %v; only NUL is unrepresentable, and rejecting more than "+
						"necessary would be a different bug", err)
				}
			})
		})
	})
}
