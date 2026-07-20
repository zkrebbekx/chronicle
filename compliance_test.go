package chronicle

import (
	"context"
	"errors"
	"testing"
	"time"
)

// This file tests the compliance capabilities on MemStore — deletion with
// tombstones, and legal holds — plus the Hold type's own semantics.

func seedSuperseded(t *testing.T, s *MemStore) (old, current Record) {
	t.Helper()
	ctx := context.Background()
	tx1, err := s.Apply(ctx, ApplyRequest{TxAt: t1, Plan: StaticWrite(Write{Insert: []Record{{
		ID: "r-old", Kind: "employee", EntityID: "e1", Data: []byte("v1"), ValidFrom: t1, Actor: alice,
	}}})})
	if err != nil {
		t.Fatalf("seed Apply failed: %v", err)
	}
	_, err = s.Apply(ctx, ApplyRequest{TxAt: tx1.Add(time.Second), Plan: StaticWrite(Write{
		Supersede: []RecordID{"r-old"},
		Insert: []Record{{
			ID: "r-cur", Kind: "employee", EntityID: "e1", Data: []byte("v2"), ValidFrom: t1, Actor: alice,
		}}})})
	if err != nil {
		t.Fatalf("seed Apply failed: %v", err)
	}
	return byIDStore(t, s, "r-old"), byIDStore(t, s, "r-cur")
}

func byIDStore(t *testing.T, s Store, id RecordID) Record {
	t.Helper()
	recs, _, err := s.Query(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	for _, r := range recs {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no record %s", id)
	return Record{}
}

func TestMemStoreDelete(t *testing.T) {
	ctx := context.Background()

	t.Run("given a store with a superseded and a current record", func(t *testing.T) {
		s := NewMemStore()
		seedSuperseded(t, s)

		t.Run("when the superseded record is deleted", func(t *testing.T) {
			n, err := s.Delete(ctx, []RecordID{"r-old"})
			t.Run("then it is destroyed and counted", func(t *testing.T) {
				if err != nil || n != 1 {
					t.Fatalf("Delete = (%d, %v); want (1, nil)", n, err)
				}
				recs, _, _ := s.Query(ctx, Query{})
				if len(recs) != 1 || recs[0].ID != "r-cur" {
					t.Fatalf("records after delete = %v; want just r-cur", len(recs))
				}
			})
			t.Run("then deleting it again is an idempotent no-op", func(t *testing.T) {
				n, err := s.Delete(ctx, []RecordID{"r-old"})
				if err != nil || n != 0 {
					t.Fatalf("repeat Delete = (%d, %v); want (0, nil)", n, err)
				}
			})
			t.Run("then it left no tombstone, because it carried no chain hash", func(t *testing.T) {
				ts, err := s.Tombstones(ctx, "employee", "e1")
				if err != nil || len(ts) != 0 {
					t.Fatalf("Tombstones = (%v, %v); want none", ts, err)
				}
			})
		})
	})

	t.Run("given a deletion naming a current record", func(t *testing.T) {
		s := NewMemStore()
		seedSuperseded(t, s)

		t.Run("when the batch mixes it with a deletable record", func(t *testing.T) {
			n, err := s.Delete(ctx, []RecordID{"r-old", "r-cur"})
			t.Run("then the whole batch is refused, naming the record", func(t *testing.T) {
				if !errors.Is(err, ErrCurrentRecord) {
					t.Fatalf("Delete = %v; want ErrCurrentRecord", err)
				}
				var de *DeleteError
				if !errors.As(err, &de) || de.RecordID != "r-cur" {
					t.Fatalf("error = %v; want a *DeleteError naming r-cur", err)
				}
				if n != 0 {
					t.Fatalf("Delete reported %d deletions on refusal; want 0", n)
				}
			})
			t.Run("then nothing was deleted, including the deletable record", func(t *testing.T) {
				recs, _, _ := s.Query(ctx, Query{})
				if len(recs) != 2 {
					t.Fatalf("records = %d; want both — refusal is all-or-nothing", len(recs))
				}
			})
		})
	})

	t.Run("given a superseded record carrying a chain hash", func(t *testing.T) {
		s := NewMemStore()
		tx1, err := s.Apply(ctx, ApplyRequest{TxAt: t1, Plan: StaticWrite(Write{Insert: []Record{{
			ID: "r-chained", Kind: "employee", EntityID: "e1", Data: []byte("v1"),
			ValidFrom: t1, Actor: alice,
			Meta: map[string]string{MetaChain: "v1:00ff", "ticket": "HR-1"},
		}}})})
		if err != nil {
			t.Fatalf("Apply failed: %v", err)
		}
		if _, err := s.Apply(ctx, ApplyRequest{TxAt: tx1.Add(time.Second), Plan: StaticWrite(Write{
			Supersede: []RecordID{"r-chained"},
			Insert: []Record{{
				ID: "r-cur", Kind: "employee", EntityID: "e1", Data: []byte("v2"), ValidFrom: t1, Actor: alice,
			}}})}); err != nil {
			t.Fatalf("Apply failed: %v", err)
		}
		stored := byIDStore(t, s, "r-chained")

		t.Run("when it is deleted", func(t *testing.T) {
			if _, err := s.Delete(ctx, []RecordID{"r-chained"}); err != nil {
				t.Fatalf("Delete failed: %v", err)
			}
			t.Run("then a tombstone preserves its coordinates and chain hash", func(t *testing.T) {
				ts, err := s.Tombstones(ctx, "employee", "e1")
				if err != nil || len(ts) != 1 {
					t.Fatalf("Tombstones = (%v, %v); want one", ts, err)
				}
				tomb := ts[0]
				if tomb.RecordID != "r-chained" || tomb.Kind != "employee" || tomb.EntityID != "e1" {
					t.Fatalf("tombstone identity = %+v", tomb)
				}
				if tomb.ChainHash != "v1:00ff" {
					t.Fatalf("ChainHash = %q; want the record's verbatim chain value", tomb.ChainHash)
				}
				if !tomb.TxFrom.Equal(stored.TxFrom) || !tomb.ValidFrom.Equal(stored.ValidFrom) {
					t.Fatalf("tombstone coordinates = %+v; want the record's", tomb)
				}
				if tomb.DeletedAt.IsZero() {
					t.Fatal("DeletedAt was not assigned")
				}
			})
			t.Run("then tombstones do not leak to other entities", func(t *testing.T) {
				ts, err := s.Tombstones(ctx, "employee", "e2")
				if err != nil || len(ts) != 0 {
					t.Fatalf("Tombstones for e2 = (%v, %v); want none", ts, err)
				}
			})
		})
	})

	t.Run("given malformed tombstone lookups", func(t *testing.T) {
		s := NewMemStore()
		t.Run("when the kind is empty", func(t *testing.T) {
			if _, err := s.Tombstones(ctx, "", "e1"); !errors.Is(err, ErrUnknownKind) {
				t.Fatalf("Tombstones = %v; want ErrUnknownKind", err)
			}
		})
		t.Run("when the entity is empty", func(t *testing.T) {
			if _, err := s.Tombstones(ctx, "employee", ""); !errors.Is(err, ErrMissingEntityID) {
				t.Fatalf("Tombstones = %v; want ErrMissingEntityID", err)
			}
		})
	})

	t.Run("given a closed store", func(t *testing.T) {
		s := NewMemStore()
		_ = s.Close()
		t.Run("when the compliance surface is used", func(t *testing.T) {
			if _, err := s.Delete(ctx, []RecordID{"x"}); !errors.Is(err, ErrClosed) {
				t.Fatalf("Delete = %v; want ErrClosed", err)
			}
			if _, err := s.Tombstones(ctx, "employee", "e1"); !errors.Is(err, ErrClosed) {
				t.Fatalf("Tombstones = %v; want ErrClosed", err)
			}
			if _, err := s.PlaceHold(ctx, Hold{ID: "h", PlacedBy: alice}); !errors.Is(err, ErrClosed) {
				t.Fatalf("PlaceHold = %v; want ErrClosed", err)
			}
			if _, err := s.ReleaseHold(ctx, "h", alice, ""); !errors.Is(err, ErrClosed) {
				t.Fatalf("ReleaseHold = %v; want ErrClosed", err)
			}
			if _, err := s.Holds(ctx); !errors.Is(err, ErrClosed) {
				t.Fatalf("Holds = %v; want ErrClosed", err)
			}
		})
	})

	t.Run("given a cancelled context", func(t *testing.T) {
		s := NewMemStore()
		cctx, cancel := context.WithCancel(context.Background())
		cancel()
		t.Run("when the compliance surface is used", func(t *testing.T) {
			if _, err := s.Delete(cctx, nil); !errors.Is(err, context.Canceled) {
				t.Fatalf("Delete = %v; want context.Canceled", err)
			}
			if _, err := s.Tombstones(cctx, "employee", "e1"); !errors.Is(err, context.Canceled) {
				t.Fatalf("Tombstones = %v; want context.Canceled", err)
			}
			if _, err := s.PlaceHold(cctx, Hold{ID: "h", PlacedBy: alice}); !errors.Is(err, context.Canceled) {
				t.Fatalf("PlaceHold = %v; want context.Canceled", err)
			}
			if _, err := s.ReleaseHold(cctx, "h", alice, ""); !errors.Is(err, context.Canceled) {
				t.Fatalf("ReleaseHold = %v; want context.Canceled", err)
			}
			if _, err := s.Holds(cctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("Holds = %v; want context.Canceled", err)
			}
		})
	})
}

func TestMemStoreHolds(t *testing.T) {
	ctx := context.Background()

	t.Run("given a hold placed with a backdated effective instant", func(t *testing.T) {
		s := NewMemStore()
		backdated := time.Now().UTC().AddDate(-1, 0, 0)
		placed, err := s.PlaceHold(ctx, Hold{
			ID:            "matter-1",
			Kind:          "employee",
			EffectiveFrom: backdated,
			Reason:        "anticipated litigation",
			PlacedBy:      alice,
			// A caller trying to write the audit fields directly:
			PlacedAt:   t1,
			ReleasedAt: t2,
			ReleasedBy: bob,
		})
		if err != nil {
			t.Fatalf("PlaceHold failed: %v", err)
		}

		t.Run("then the backdated effective instant is stored verbatim", func(t *testing.T) {
			if !placed.EffectiveFrom.Equal(backdated) {
				t.Fatalf("EffectiveFrom = %s; want the backdated %s — FRCP 37(e)'s trigger is "+
					"anticipation, judged after the fact", placed.EffectiveFrom, backdated)
			}
		})
		t.Run("then PlacedAt is store-assigned, not the caller's", func(t *testing.T) {
			if placed.PlacedAt.IsZero() || placed.PlacedAt.Equal(t1) {
				t.Fatalf("PlacedAt = %s; a caller must not write the control's own timeline", placed.PlacedAt)
			}
		})
		t.Run("then the release fields arrive empty whatever the caller sent", func(t *testing.T) {
			if !placed.ReleasedAt.IsZero() || !placed.ReleasedBy.IsZero() {
				t.Fatalf("release fields = %s/%+v; want empty at placement", placed.ReleasedAt, placed.ReleasedBy)
			}
		})
		t.Run("then it is listed", func(t *testing.T) {
			holds, err := s.Holds(ctx)
			if err != nil || len(holds) != 1 || holds[0].ID != "matter-1" {
				t.Fatalf("Holds = (%v, %v); want just matter-1", holds, err)
			}
		})
		t.Run("then placing the same ID again is refused", func(t *testing.T) {
			_, err := s.PlaceHold(ctx, Hold{ID: "matter-1", PlacedBy: bob})
			if !errors.Is(err, ErrHoldExists) {
				t.Fatalf("PlaceHold = %v; want ErrHoldExists", err)
			}
			var he *HoldError
			if !errors.As(err, &he) || he.ID != "matter-1" {
				t.Fatalf("error = %v; want a *HoldError naming matter-1", err)
			}
		})

		t.Run("when it is released", func(t *testing.T) {
			released, err := s.ReleaseHold(ctx, "matter-1", bob, "matter settled")
			if err != nil {
				t.Fatalf("ReleaseHold failed: %v", err)
			}
			t.Run("then the release is attributed and timestamped by the store", func(t *testing.T) {
				if released.ReleasedAt.IsZero() || released.ReleasedBy.ID != bob.ID || released.ReleaseReason != "matter settled" {
					t.Fatalf("release = %+v; want bob's attributed release", released)
				}
			})
			t.Run("then the hold record survives its release", func(t *testing.T) {
				holds, _ := s.Holds(ctx)
				if len(holds) != 1 || holds[0].ReleasedAt.IsZero() {
					t.Fatalf("Holds after release = %+v; want the released hold still listed", holds)
				}
			})
			t.Run("then releasing again is refused rather than rewriting the first release", func(t *testing.T) {
				if _, err := s.ReleaseHold(ctx, "matter-1", alice, ""); !errors.Is(err, ErrHoldReleased) {
					t.Fatalf("second release = %v; want ErrHoldReleased", err)
				}
			})
		})
	})

	t.Run("given malformed hold operations", func(t *testing.T) {
		s := NewMemStore()
		cases := []struct {
			name string
			err  error
			want error
		}{
			{"placement without an ID", func() error {
				_, err := s.PlaceHold(ctx, Hold{PlacedBy: alice})
				return err
			}(), ErrMissingHoldID},
			{"placement without an actor", func() error {
				_, err := s.PlaceHold(ctx, Hold{ID: "h"})
				return err
			}(), ErrMissingActor},
			{"release without an ID", func() error {
				_, err := s.ReleaseHold(ctx, "", alice, "")
				return err
			}(), ErrMissingHoldID},
			{"release without an actor", func() error {
				_, err := s.ReleaseHold(ctx, "h", Actor{}, "")
				return err
			}(), ErrMissingActor},
			{"release of an unknown hold", func() error {
				_, err := s.ReleaseHold(ctx, "no-such-hold", alice, "")
				return err
			}(), ErrNotFound},
		}
		for _, tc := range cases {
			t.Run("when "+tc.name, func(t *testing.T) {
				t.Run("then it is rejected", func(t *testing.T) {
					if !errors.Is(tc.err, tc.want) {
						t.Fatalf("got %v; want %v", tc.err, tc.want)
					}
				})
			})
		}
	})
}

func TestHoldSemantics(t *testing.T) {
	rec := Record{Kind: "employee", EntityID: "e1"}

	t.Run("given hold scopes", func(t *testing.T) {
		cases := []struct {
			name  string
			hold  Hold
			match bool
		}{
			{"everything", Hold{}, true},
			{"the record's kind", Hold{Kind: "employee"}, true},
			{"another kind", Hold{Kind: "invoice"}, false},
			{"the record's entity in its kind", Hold{Kind: "employee", EntityID: "e1"}, true},
			{"another entity", Hold{Kind: "employee", EntityID: "e2"}, false},
			{"the entity ID across all kinds", Hold{EntityID: "e1"}, true},
			{"another entity ID across all kinds", Hold{EntityID: "e9"}, false},
		}
		for _, tc := range cases {
			t.Run("when the hold scopes "+tc.name, func(t *testing.T) {
				t.Run("then matching agrees", func(t *testing.T) {
					if got := tc.hold.Matches(rec); got != tc.match {
						t.Fatalf("Matches = %v; want %v", got, tc.match)
					}
				})
			})
		}
	})

	t.Run("given a hold's effective interval", func(t *testing.T) {
		h := Hold{EffectiveFrom: t1, ReleasedAt: t3}
		t.Run("when probed across it", func(t *testing.T) {
			t.Run("then it is half-open, like every chronicle interval", func(t *testing.T) {
				if h.ActiveAt(t1.Add(-time.Second)) {
					t.Fatal("active before EffectiveFrom")
				}
				if !h.ActiveAt(t1) {
					t.Fatal("inactive at EffectiveFrom; the lower bound is inclusive")
				}
				if !h.ActiveAt(t2) {
					t.Fatal("inactive between the bounds")
				}
				if h.ActiveAt(t3) {
					t.Fatal("active at ReleasedAt; the upper bound is exclusive")
				}
			})
		})
		t.Run("when the bounds are unbounded", func(t *testing.T) {
			t.Run("then a zero EffectiveFrom means always was, a zero ReleasedAt means still is", func(t *testing.T) {
				if !(Hold{}).ActiveAt(t1) {
					t.Fatal("a hold with no bounds is always active until released")
				}
				if (Hold{EffectiveFrom: t2}).ActiveAt(t1) {
					t.Fatal("a future-dated hold must not be active yet")
				}
			})
		})
	})
}
