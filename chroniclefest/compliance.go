package chroniclefest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zkrebbekx/chronicle"
)

// This file is the conformance suite for the compliance capabilities:
// [chronicle.Deleter], [chronicle.HoldStore] and [chronicle.Keyring]. They
// are optional extensions of the store contract, so they get their own entry
// points rather than a place inside [Run] — a store that does not claim a
// capability is not broken, but a store that claims one is held to all of it.

// RunCompliance executes the deletion and legal-hold conformance suite. The
// store the factory returns must implement both [chronicle.Deleter] and
// [chronicle.HoldStore]; calling this is how a store author claims the
// capabilities, and a store that turns out not to carry them fails rather
// than skips.
func RunCompliance(t *testing.T, newStore Factory) {
	t.Helper()
	RunComplianceT(Wrap(t), newStore)
}

// RunComplianceT is [RunCompliance] for a harness that is not a Go test.
func RunComplianceT(t T, newStore Factory) {
	t.Helper()
	t.Run("deletion contract", func(t T) { runDeletion(t, newStore) })
	t.Run("hold contract", func(t T) { runHolds(t, newStore) })
}

// KeyringFactory returns a fresh keyring for one test, independent of every
// other one.
type KeyringFactory func(t T) chronicle.Keyring

// RunKeyring executes the [chronicle.Keyring] conformance suite.
func RunKeyring(t *testing.T, newKeyring KeyringFactory) {
	t.Helper()
	RunKeyringT(Wrap(t), newKeyring)
}

// RunKeyringT is [RunKeyring] for a harness that is not a Go test.
func RunKeyringT(t T, newKeyring KeyringFactory) {
	t.Helper()
	runKeyring(t, newKeyring)
}

// asDeleter asserts the capability under test, loudly.
func asDeleter(t T, s chronicle.Store) chronicle.Deleter {
	t.Helper()
	d, ok := s.(chronicle.Deleter)
	if !ok {
		t.Fatalf("store %T does not implement chronicle.Deleter; RunCompliance is only for stores claiming the capability", s)
	}
	return d
}

// asHoldStore asserts the capability under test, loudly.
func asHoldStore(t T, s chronicle.Store) chronicle.HoldStore {
	t.Helper()
	h, ok := s.(chronicle.HoldStore)
	if !ok {
		t.Fatalf("store %T does not implement chronicle.HoldStore; RunCompliance is only for stores claiming the capability", s)
	}
	return h
}

// seedSupersededPair writes one entity with a superseded generation and a
// current one, and returns both transaction instants.
func seedSupersededPair(t T, s chronicle.Store, entityID string) (tx1, tx2 time.Time) {
	t.Helper()
	tx1 = apply(t, s, chronicle.ApplyRequest{TxAt: jan, Plan: chronicle.StaticWrite(chronicle.Write{Insert: []chronicle.Record{{
		ID: chronicle.RecordID("old-" + entityID), Kind: employee, EntityID: entityID,
		Data: []byte("v1"), ValidFrom: feb, Actor: alice,
	}}})})
	tx2 = apply(t, s, chronicle.ApplyRequest{TxAt: tx1.Add(time.Second), Plan: chronicle.StaticWrite(chronicle.Write{
		Supersede: []chronicle.RecordID{chronicle.RecordID("old-" + entityID)},
		Insert: []chronicle.Record{{
			ID: chronicle.RecordID("cur-" + entityID), Kind: employee, EntityID: entityID,
			Data: []byte("v2"), ValidFrom: feb, Actor: alice,
		}}})})
	return tx1, tx2
}

// ---------------------------------------------------------------------------
// deletion
// ---------------------------------------------------------------------------

func runDeletion(t T, newStore Factory) {
	ctx := context.Background()

	t.Run("given a superseded and a current record", func(t T) {
		s := newStore(t)
		d := asDeleter(t, s)
		seedSupersededPair(t, s, "e1")

		t.Run("when the superseded record is deleted", func(t T) {
			n, err := d.Delete(ctx, []chronicle.RecordID{"old-e1"})
			t.Run("then it is destroyed and counted", func(t T) {
				if err != nil || n != 1 {
					t.Fatalf("Delete = (%d, %v); want (1, nil)", n, err)
				}
			})
			t.Run("then the current belief survives the deletion", func(t T) {
				recs := queryAll(t, s, chronicle.Query{Kind: employee, EntityID: "e1"})
				if len(recs) != 1 || recs[0].ID != "cur-e1" {
					t.Fatalf("records = %v; want only cur-e1 — a deletion must destroy exactly "+
						"what it was asked to, never the entity's current belief", ids(recs))
				}
			})
			t.Run("then deleting the same ID again is an idempotent no-op", func(t T) {
				n, err := d.Delete(ctx, []chronicle.RecordID{"old-e1"})
				if err != nil || n != 0 {
					t.Fatalf("repeated Delete = (%d, %v); want (0, nil) so an interrupted "+
						"sweep can be retried whole", n, err)
				}
			})
			t.Run("then it left no tombstone, because it carried no chain hash", func(t T) {
				ts, err := d.Tombstones(ctx, employee, "e1")
				if err != nil || len(ts) != 0 {
					t.Fatalf("Tombstones = (%v, %v); want none for an unchained record", ts, err)
				}
			})
		})
	})

	t.Run("given a deletion that names a current record", func(t T) {
		s := newStore(t)
		d := asDeleter(t, s)
		seedSupersededPair(t, s, "e1")

		t.Run("when the batch mixes it with a deletable record", func(t T) {
			n, err := d.Delete(ctx, []chronicle.RecordID{"old-e1", "cur-e1"})
			t.Run("then the store refuses with an error wrapping ErrCurrentRecord", func(t T) {
				if !errors.Is(err, chronicle.ErrCurrentRecord) {
					t.Fatalf("Delete = %v; want an error wrapping ErrCurrentRecord — current "+
						"belief is never retention-deleted", err)
				}
				if n != 0 {
					t.Fatalf("Delete counted %d on refusal; want 0", n)
				}
			})
			t.Run("then a refused deletion deletes nothing", func(t T) {
				recs := queryAll(t, s, chronicle.Query{Kind: employee, EntityID: "e1"})
				if len(recs) != 2 {
					t.Fatalf("records = %v; want both — refusal must be all-or-nothing, or a "+
						"failed sweep leaves a half-destroyed batch", ids(recs))
				}
			})
		})
	})

	t.Run("given superseded records carrying chain hashes", func(t T) {
		s := newStore(t)
		d := asDeleter(t, s)

		const token1, token2 = "v1:aa11", "v1:bb22"
		tx1 := apply(t, s, chronicle.ApplyRequest{TxAt: jan, Plan: chronicle.StaticWrite(chronicle.Write{Insert: []chronicle.Record{
			{
				ID: "ch-1", Kind: employee, EntityID: "e1", Data: []byte("v1"),
				ValidFrom: feb, ValidTo: mar, Actor: alice,
				Meta: map[string]string{chronicle.MetaChain: token1, "ticket": "HR-1"},
			},
			{
				ID: "ch-2", Kind: employee, EntityID: "e1", Data: []byte("v1"),
				ValidFrom: mar, ValidTo: apr, Actor: alice,
				Meta: map[string]string{chronicle.MetaChain: token2},
			},
		}})})
		apply(t, s, chronicle.ApplyRequest{TxAt: tx1.Add(time.Second), Plan: chronicle.StaticWrite(chronicle.Write{
			Supersede: []chronicle.RecordID{"ch-1", "ch-2"},
			Insert: []chronicle.Record{{
				ID: "ch-3", Kind: employee, EntityID: "e1", Data: []byte("v2"), ValidFrom: feb, Actor: alice,
			}}})})
		stored := byID(t, s, "ch-1")

		t.Run("when they are deleted", func(t T) {
			if _, err := d.Delete(ctx, []chronicle.RecordID{"ch-1", "ch-2"}); err != nil {
				t.Fatalf("Delete failed: %v", err)
			}
			ts, err := d.Tombstones(ctx, employee, "e1")
			if err != nil {
				t.Fatalf("Tombstones failed: %v", err)
			}
			t.Run("then the tombstones are readable after the deletion", func(t T) {
				if len(ts) != 2 {
					t.Fatalf("tombstones = %d; want 2 — deleting a chained record without "+
						"retaining its hash breaks the chain for everything after it", len(ts))
				}
			})
			t.Run("then they come back in chain order with the coordinates intact", func(t T) {
				if len(ts) != 2 {
					t.Fatalf("tombstones = %d; want 2", len(ts))
				}
				if ts[0].RecordID != "ch-1" || ts[1].RecordID != "ch-2" {
					t.Fatalf("tombstone order = %s, %s; want ch-1 then ch-2", ts[0].RecordID, ts[1].RecordID)
				}
				got := ts[0]
				if got.Kind != employee || got.EntityID != "e1" {
					t.Fatalf("tombstone identity = %+v", got)
				}
				if got.ChainHash != token1 {
					t.Fatalf("ChainHash = %q; want the verbatim stored token %q", got.ChainHash, token1)
				}
				if !got.TxFrom.Equal(stored.TxFrom) || !got.ValidFrom.Equal(stored.ValidFrom) {
					t.Fatalf("tombstone coordinates = (%s, %s); want the record's (%s, %s)",
						got.TxFrom, got.ValidFrom, stored.TxFrom, stored.ValidFrom)
				}
				if got.DeletedAt.IsZero() {
					t.Fatal("DeletedAt was not assigned by the store")
				}
			})
			t.Run("then a retried deletion does not duplicate them", func(t T) {
				if _, err := d.Delete(ctx, []chronicle.RecordID{"ch-1", "ch-2"}); err != nil {
					t.Fatalf("retried Delete failed: %v", err)
				}
				again, err := d.Tombstones(ctx, employee, "e1")
				if err != nil || len(again) != 2 {
					t.Fatalf("Tombstones after retry = (%d, %v); want still 2", len(again), err)
				}
			})
			t.Run("then tombstones do not leak between entities", func(t T) {
				other, err := d.Tombstones(ctx, employee, "e2")
				if err != nil || len(other) != 0 {
					t.Fatalf("Tombstones for e2 = (%v, %v); want none", other, err)
				}
			})
		})
	})
}

// ---------------------------------------------------------------------------
// holds
// ---------------------------------------------------------------------------

func runHolds(t T, newStore Factory) {
	ctx := context.Background()

	t.Run("given a hold placed with a backdated effective instant", func(t T) {
		s := newStore(t)
		h := asHoldStore(t, s)
		placed, err := h.PlaceHold(ctx, chronicle.Hold{
			ID:            "matter-1",
			Kind:          employee,
			EntityID:      "e1",
			EffectiveFrom: jan,
			Reason:        "anticipated litigation",
			PlacedBy:      alice,
			// A caller trying to write the audit fields themselves:
			PlacedAt:      feb,
			ReleasedAt:    mar,
			ReleasedBy:    bob,
			ReleaseReason: "smuggled",
		})
		if err != nil {
			t.Fatalf("PlaceHold failed: %v", err)
		}

		t.Run("when the stored hold is examined", func(t T) {
			t.Run("then the backdated effective instant round-trips exactly", func(t T) {
				if !placed.EffectiveFrom.Equal(jan) {
					t.Fatalf("EffectiveFrom = %s; want the backdated %s — FRCP 37(e)'s trigger "+
						"is anticipation, judged after the fact, so a hold must accept an "+
						"operator-asserted past instant", placed.EffectiveFrom, jan)
				}
			})
			t.Run("then PlacedAt is store-assigned, never the caller's", func(t T) {
				if placed.PlacedAt.IsZero() || placed.PlacedAt.Equal(feb) {
					t.Fatalf("PlacedAt = %s; a store that honours an incoming PlacedAt lets a "+
						"caller backdate the control itself", placed.PlacedAt)
				}
			})
			t.Run("then the release fields arrive empty whatever the caller sent", func(t T) {
				if !placed.ReleasedAt.IsZero() || !placed.ReleasedBy.IsZero() || placed.ReleaseReason != "" {
					t.Fatalf("release fields = (%s, %+v, %q); want empty at placement",
						placed.ReleasedAt, placed.ReleasedBy, placed.ReleaseReason)
				}
			})
			t.Run("then the scope, reason and actor round-trip", func(t T) {
				if placed.Kind != employee || placed.EntityID != "e1" ||
					placed.Reason != "anticipated litigation" || placed.PlacedBy != alice {
					t.Fatalf("hold = %+v; fields did not survive the round trip", placed)
				}
			})
		})

		t.Run("when the holds are listed", func(t T) {
			t.Run("then the hold is listed after placement", func(t T) {
				holds, err := h.Holds(ctx)
				if err != nil || len(holds) != 1 || holds[0].ID != "matter-1" {
					t.Fatalf("Holds = (%v, %v); want exactly matter-1 — a hold the store does "+
						"not report is a hold the sweeper cannot honour", holds, err)
				}
			})
		})

		t.Run("when the same ID is placed again", func(t T) {
			t.Run("then it is refused with ErrHoldExists", func(t T) {
				if _, err := h.PlaceHold(ctx, chronicle.Hold{ID: "matter-1", PlacedBy: bob}); !errors.Is(err, chronicle.ErrHoldExists) {
					t.Fatalf("PlaceHold = %v; want ErrHoldExists — silently replacing a hold's "+
						"scope is the edit an audit control must not permit", err)
				}
			})
		})

		t.Run("when the hold is released", func(t T) {
			released, err := h.ReleaseHold(ctx, "matter-1", bob, "matter settled")
			if err != nil {
				t.Fatalf("ReleaseHold failed: %v", err)
			}
			t.Run("then the release is attributed and store-timestamped", func(t T) {
				if released.ReleasedAt.IsZero() || released.ReleasedBy.ID != bob.ID || released.ReleaseReason != "matter settled" {
					t.Fatalf("release = (%s, %+v, %q); want bob's attributed, timestamped release",
						released.ReleasedAt, released.ReleasedBy, released.ReleaseReason)
				}
			})
			t.Run("then release does not delete the hold record", func(t T) {
				holds, err := h.Holds(ctx)
				if err != nil || len(holds) != 1 {
					t.Fatalf("Holds after release = (%v, %v); want the released hold still "+
						"listed — its lifecycle is the audit trail of the audit control", holds, err)
				}
				if holds[0].ReleasedAt.IsZero() || holds[0].PlacedBy.ID != alice.ID {
					t.Fatalf("hold after release = %+v; want placement intact and release recorded", holds[0])
				}
			})
			t.Run("then releasing again is refused rather than rewriting the first release", func(t T) {
				if _, err := h.ReleaseHold(ctx, "matter-1", alice, "again"); !errors.Is(err, chronicle.ErrHoldReleased) {
					t.Fatalf("second release = %v; want ErrHoldReleased", err)
				}
			})
		})
	})

	t.Run("given malformed hold operations", func(t T) {
		s := newStore(t)
		h := asHoldStore(t, s)

		t.Run("when a hold is placed without an ID", func(t T) {
			t.Run("then it is ErrMissingHoldID", func(t T) {
				if _, err := h.PlaceHold(ctx, chronicle.Hold{PlacedBy: alice}); !errors.Is(err, chronicle.ErrMissingHoldID) {
					t.Fatalf("PlaceHold = %v; want ErrMissingHoldID", err)
				}
			})
		})
		t.Run("when a hold is placed without an actor", func(t T) {
			t.Run("then it is ErrMissingActor, like every chronicle write", func(t T) {
				if _, err := h.PlaceHold(ctx, chronicle.Hold{ID: "h"}); !errors.Is(err, chronicle.ErrMissingActor) {
					t.Fatalf("PlaceHold = %v; want ErrMissingActor", err)
				}
			})
		})
		t.Run("when an unknown hold is released", func(t T) {
			t.Run("then it is ErrNotFound", func(t T) {
				if _, err := h.ReleaseHold(ctx, "no-such-hold", alice, ""); !errors.Is(err, chronicle.ErrNotFound) {
					t.Fatalf("ReleaseHold = %v; want ErrNotFound", err)
				}
			})
		})
		t.Run("when a release names no actor", func(t T) {
			t.Run("then it is ErrMissingActor", func(t T) {
				if _, err := h.ReleaseHold(ctx, "h", chronicle.Actor{}, ""); !errors.Is(err, chronicle.ErrMissingActor) {
					t.Fatalf("ReleaseHold = %v; want ErrMissingActor", err)
				}
			})
		})
	})
}

// ---------------------------------------------------------------------------
// keyrings
// ---------------------------------------------------------------------------

func runKeyring(t T, newKeyring KeyringFactory) {
	ctx := context.Background()

	t.Run("given a fresh keyring", func(t T) {
		k := newKeyring(t)

		first, err := k.Key(ctx, "subject-1")
		if err != nil {
			t.Fatalf("Key failed: %v", err)
		}

		t.Run("when a subject's key is used", func(t T) {
			t.Run("then it is the documented size", func(t T) {
				if len(first) != chronicle.KeySize {
					t.Fatalf("key length = %d; want %d", len(first), chronicle.KeySize)
				}
			})
			t.Run("then the key is stable across calls", func(t T) {
				again, err := k.Key(ctx, "subject-1")
				if err != nil || string(again) != string(first) {
					t.Fatalf("Key = (%x, %v); want the same key back — an unstable key makes "+
						"every earlier ciphertext unreadable", again, err)
				}
			})
			t.Run("then different subjects get different keys", func(t T) {
				other, err := k.Key(ctx, "subject-2")
				if err != nil {
					t.Fatalf("Key failed: %v", err)
				}
				if string(other) == string(first) {
					t.Fatal("two subjects share one key; destroying either would shred both")
				}
			})
			t.Run("then mutating a returned key does not corrupt the ring", func(t T) {
				got, err := k.Key(ctx, "subject-1")
				if err != nil {
					t.Fatalf("Key failed: %v", err)
				}
				// The snapshot is a string, deliberately: comparing against a
				// slice would compare a key against itself when the ring
				// aliases, which is exactly the fault under test.
				snapshot := string(got)
				got[0] ^= 0xff
				again, err := k.Key(ctx, "subject-1")
				if err != nil || string(again) != snapshot {
					t.Fatalf("Key after caller mutation = (%x, %v); the ring must hand out copies", again, err)
				}
			})
		})

		t.Run("when the subject's key is destroyed", func(t T) {
			if err := k.DestroyKey(ctx, "subject-1"); err != nil {
				t.Fatalf("DestroyKey failed: %v", err)
			}
			t.Run("then the destroyed key is unrecoverable", func(t T) {
				if _, err := k.Key(ctx, "subject-1"); !errors.Is(err, chronicle.ErrKeyDestroyed) {
					t.Fatalf("Key after destroy = %v; want ErrKeyDestroyed — a keyring that "+
						"still answers is a shredder that does not shred", err)
				}
			})
			t.Run("then destroying again is idempotent", func(t T) {
				if err := k.DestroyKey(ctx, "subject-1"); err != nil {
					t.Fatalf("second DestroyKey = %v; want nil", err)
				}
			})
			t.Run("then other subjects are untouched", func(t T) {
				if _, err := k.Key(ctx, "subject-2"); err != nil {
					t.Fatalf("Key for the surviving subject = %v; want it intact", err)
				}
			})
		})

		t.Run("when a subject that never had a key is destroyed", func(t T) {
			if err := k.DestroyKey(ctx, "never-keyed"); err != nil {
				t.Fatalf("DestroyKey = %v; want nil — making a subject unreadable must not "+
					"fail on the subject it cannot read", err)
			}
			t.Run("then no key can ever be minted for it", func(t T) {
				if _, err := k.Key(ctx, "never-keyed"); !errors.Is(err, chronicle.ErrKeyDestroyed) {
					t.Fatalf("Key = %v; want ErrKeyDestroyed — destruction is terminal, or a "+
						"re-minted key quietly revives an identifier the caller believes erased", err)
				}
			})
		})
	})
}
