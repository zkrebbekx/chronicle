package chroniclefest_test

import (
	"context"
	"testing"
	"time"

	"github.com/zkrebbekx/chronicle"
	"github.com/zkrebbekx/chronicle/chroniclefest"
)

// This file holds deliberately broken implementations of the compliance
// capabilities — deletion, holds, keyrings — and the tests asserting that
// RunCompliance and RunKeyring fail against each one, on the check that names
// the fault. The core suite earned its standing this way and found three of
// its own holes doing it; the compliance suite starts under the same
// discipline.

// ---------------------------------------------------------------------------
// broken deleters
// ---------------------------------------------------------------------------

type entityRef struct{ kind, id string }

// deleteTakesEntity destroys every record of the entities the named records
// belong to — the shape of a DELETE keyed on the entity instead of the record
// ID — so a retention sweep of superseded history takes the current belief
// with it.
type deleteTakesEntity struct {
	base
	gone map[entityRef]bool
}

func (s *deleteTakesEntity) Delete(ctx context.Context, ids []chronicle.RecordID) (int, error) {
	named := make(map[chronicle.RecordID]bool, len(ids))
	for _, id := range ids {
		named[id] = true
	}
	recs, _, err := s.inner().Query(ctx, chronicle.Query{})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range recs {
		if named[r.ID] {
			n++
			s.gone[entityRef{r.Kind, r.EntityID}] = true
		}
	}
	return n, nil
}

func (s *deleteTakesEntity) Query(ctx context.Context, q chronicle.Query) ([]chronicle.Record, chronicle.Cursor, error) {
	recs, c, err := s.inner().Query(ctx, q)
	if err != nil {
		return nil, c, err
	}
	kept := recs[:0]
	for _, r := range recs {
		if !s.gone[entityRef{r.Kind, r.EntityID}] {
			kept = append(kept, r)
		}
	}
	return kept, c, nil
}

// skipsCurrentSilently drops the current records from a deletion batch and
// destroys the rest, instead of refusing the whole batch loudly.
type skipsCurrentSilently struct{ base }

func (s *skipsCurrentSilently) Delete(ctx context.Context, ids []chronicle.RecordID) (int, error) {
	recs, _, err := s.inner().Query(ctx, chronicle.Query{CurrentOnly: true})
	if err != nil {
		return 0, err
	}
	current := make(map[chronicle.RecordID]bool, len(recs))
	for _, r := range recs {
		current[r.ID] = true
	}
	deletable := make([]chronicle.RecordID, 0, len(ids))
	for _, id := range ids {
		if !current[id] {
			deletable = append(deletable, id)
		}
	}
	return s.inner().Delete(ctx, deletable)
}

// dropsTombstones deletes correctly and then has nothing to say about it: the
// retained chain hashes are never readable, so every gap retention leaves is
// a break in the chain.
type dropsTombstones struct{ base }

func (s *dropsTombstones) Tombstones(context.Context, string, string) ([]chronicle.Tombstone, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// broken hold stores
// ---------------------------------------------------------------------------

// holdsVanish accepts a hold, reports it placed, and never lists it — so the
// sweeper it exists to restrain never hears about it.
type holdsVanish struct{ base }

func (s *holdsVanish) PlaceHold(_ context.Context, h chronicle.Hold) (chronicle.Hold, error) {
	if err := h.Validate(); err != nil {
		return chronicle.Hold{}, err
	}
	h.PlacedAt = time.Now().UTC()
	h.ReleasedAt = time.Time{}
	h.ReleasedBy = chronicle.Actor{}
	h.ReleaseReason = ""
	return h, nil
}

// releaseDeletesHold releases correctly and then removes the row from every
// listing, destroying the audit trail of the control.
type releaseDeletesHold struct {
	base
	released map[string]bool
}

func (s *releaseDeletesHold) ReleaseHold(ctx context.Context, id string, by chronicle.Actor, reason string) (chronicle.Hold, error) {
	h, err := s.inner().ReleaseHold(ctx, id, by, reason)
	if err == nil {
		s.released[id] = true
	}
	return h, err
}

func (s *releaseDeletesHold) Holds(ctx context.Context) ([]chronicle.Hold, error) {
	holds, err := s.inner().Holds(ctx)
	if err != nil {
		return nil, err
	}
	kept := holds[:0]
	for _, h := range holds {
		if !s.released[h.ID] {
			kept = append(kept, h)
		}
	}
	return kept, nil
}

// honoursIncomingPlacedAt lets the caller write the one timestamp the store
// must own, so the control's own timeline is operator-editable.
type honoursIncomingPlacedAt struct{ base }

func (s *honoursIncomingPlacedAt) PlaceHold(ctx context.Context, h chronicle.Hold) (chronicle.Hold, error) {
	placed, err := s.inner().PlaceHold(ctx, h)
	if err != nil {
		return placed, err
	}
	if !h.PlacedAt.IsZero() {
		placed.PlacedAt = h.PlacedAt
	}
	return placed, nil
}

// ---------------------------------------------------------------------------
// broken keyrings
// ---------------------------------------------------------------------------

// destroyIgnored is the fault crypto-shredding exists to rule out: DestroyKey
// reports success and the key keeps answering, so "erased" data is one read
// away from plaintext.
type destroyIgnored struct{ chronicle.Keyring }

func (k destroyIgnored) DestroyKey(context.Context, string) error { return nil }

// unstableKeys mints a fresh key on every call, which encrypts fine and can
// never decrypt anything written before the current call.
type unstableKeys struct{ chronicle.Keyring }

func (k unstableKeys) Key(ctx context.Context, subject string) ([]byte, error) {
	return chronicle.NewMemKeyring().Key(ctx, subject)
}

// sharedKey hands every subject the same key, so destroying one subject's
// key would shred every subject — or, as implemented, none.
type sharedKey struct {
	chronicle.Keyring
	shared []byte
}

func (k *sharedKey) Key(context.Context, string) ([]byte, error) {
	out := make([]byte, len(k.shared))
	copy(out, k.shared)
	return out, nil
}

// aliasedKeys hands out the stored key slice itself, so a caller's scratch
// arithmetic corrupts the ring.
type aliasedKeys struct {
	keys map[string][]byte
}

func (k *aliasedKeys) Key(_ context.Context, subject string) ([]byte, error) {
	if key, ok := k.keys[subject]; ok {
		return key, nil
	}
	key := make([]byte, chronicle.KeySize)
	key[0] = byte(len(k.keys) + 1) // distinct per subject, deterministic
	k.keys[subject] = key
	return key, nil
}

func (k *aliasedKeys) DestroyKey(_ context.Context, subject string) error {
	delete(k.keys, subject)
	return nil
}

// ---------------------------------------------------------------------------
// the fault-catching tests
// ---------------------------------------------------------------------------

// runCompliance drives the compliance suite against newStore under the
// recording harness.
func runCompliance(newStore chroniclefest.Factory) *recorder {
	return runWith(func(t chroniclefest.T) { chroniclefest.RunComplianceT(t, newStore) })
}

// runKeyring drives the keyring suite the same way.
func runKeyring(newKeyring chroniclefest.KeyringFactory) *recorder {
	return runWith(func(t chroniclefest.T) { chroniclefest.RunKeyringT(t, newKeyring) })
}

func complianceFaults() []fault {
	return []fault{
		{
			name: "deletes the whole entity, current belief included",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &deleteTakesEntity{base: newBase(t), gone: map[entityRef]bool{}}
			},
			wants: []string{
				"then the current belief survives the deletion",
				"never the entity's current belief",
			},
		},
		{
			name: "silently skips current records instead of refusing the batch",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &skipsCurrentSilently{newBase(t)}
			},
			wants: []string{
				"want an error wrapping ErrCurrentRecord",
				"then a refused deletion deletes nothing",
				"refusal must be all-or-nothing",
			},
		},
		{
			name: "leaves tombstone-free gaps where chained records were deleted",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &dropsTombstones{newBase(t)}
			},
			wants: []string{
				"then the tombstones are readable after the deletion",
				"breaks the chain for everything after it",
			},
		},
		{
			name: "accepts holds and never lists them",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &holdsVanish{newBase(t)}
			},
			wants: []string{
				"then the hold is listed after placement",
				"a hold the store does not report is a hold the sweeper cannot honour",
			},
		},
		{
			name: "deletes a hold's record when it is released",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &releaseDeletesHold{base: newBase(t), released: map[string]bool{}}
			},
			wants: []string{
				"then release does not delete the hold record",
			},
		},
		{
			name: "honours a caller-supplied PlacedAt",
			newStore: func(t chroniclefest.T) chronicle.Store {
				return &honoursIncomingPlacedAt{newBase(t)}
			},
			wants: []string{
				"then PlacedAt is store-assigned, never the caller's",
				"backdate the control itself",
			},
		},
	}
}

type keyringFault struct {
	name       string
	newKeyring chroniclefest.KeyringFactory
	wants      []string
}

func keyringFaults() []keyringFault {
	return []keyringFault{
		{
			name: "keeps answering after a key is destroyed",
			newKeyring: func(chroniclefest.T) chronicle.Keyring {
				return destroyIgnored{chronicle.NewMemKeyring()}
			},
			wants: []string{
				"then the destroyed key is unrecoverable",
				"a shredder that does not shred",
			},
		},
		{
			name: "mints a fresh key on every call",
			newKeyring: func(chroniclefest.T) chronicle.Keyring {
				return unstableKeys{chronicle.NewMemKeyring()}
			},
			wants: []string{"then the key is stable across calls"},
		},
		{
			name: "hands every subject the same key",
			newKeyring: func(chroniclefest.T) chronicle.Keyring {
				return &sharedKey{Keyring: chronicle.NewMemKeyring(), shared: make([]byte, chronicle.KeySize)}
			},
			wants: []string{
				"then different subjects get different keys",
				"two subjects share one key",
			},
		},
		{
			name: "hands out the stored key slice itself",
			newKeyring: func(chroniclefest.T) chronicle.Keyring {
				return &aliasedKeys{keys: map[string][]byte{}}
			},
			wants: []string{"the ring must hand out copies"},
		},
	}
}

// TestComplianceSuiteCatchesFaults is to RunCompliance what
// TestSuiteCatchesFaults is to Run: every case is a capability broken in one
// nameable way, and the assertion is that the suite notices on the check that
// names it.
func TestComplianceSuiteCatchesFaults(t *testing.T) {
	t.Run("given a store with one compliance capability broken", func(t *testing.T) {
		for _, f := range complianceFaults() {
			t.Run("when the store "+f.name, func(t *testing.T) {
				t.Parallel()
				rec := runCompliance(f.newStore)

				t.Run("then the compliance suite fails", func(t *testing.T) {
					if len(rec.failures()) == 0 {
						t.Fatal("the suite passed a store that violates the capability; " +
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

// TestKeyringSuiteCatchesFaults does the same for RunKeyring — including the
// fault the design brief singles out: a keyring that returns key material,
// and therefore plaintext, after a shred.
func TestKeyringSuiteCatchesFaults(t *testing.T) {
	t.Run("given a keyring broken in exactly one way", func(t *testing.T) {
		for _, f := range keyringFaults() {
			t.Run("when the keyring "+f.name, func(t *testing.T) {
				t.Parallel()
				rec := runKeyring(f.newKeyring)

				t.Run("then the keyring suite fails", func(t *testing.T) {
					if len(rec.failures()) == 0 {
						t.Fatal("the suite passed a keyring that violates the contract")
					}
				})
				for _, want := range f.wants {
					t.Run("then it fails on "+want, func(t *testing.T) {
						if !rec.matched(want) {
							t.Fatalf("no failure mentions %q.\n%s", want, rec.summary())
						}
					})
				}
			})
		}
	})
}

// TestComplianceSuitePassesReference proves the harness is not trivially
// failing everything: the reference store and keyring pass clean.
func TestComplianceSuitePassesReference(t *testing.T) {
	t.Run("given the reference implementations", func(t *testing.T) {
		t.Run("when the compliance suite runs under the recording harness", func(t *testing.T) {
			rec := runCompliance(func(t chroniclefest.T) chronicle.Store { return newBase(t) })
			t.Run("then it reports no failures", func(t *testing.T) {
				if n := len(rec.failures()); n != 0 {
					t.Fatalf("the harness failed a conforming store:\n%s", rec.summary())
				}
			})
		})
		t.Run("when the keyring suite runs under the recording harness", func(t *testing.T) {
			rec := runKeyring(func(chroniclefest.T) chronicle.Keyring { return chronicle.NewMemKeyring() })
			t.Run("then it reports no failures", func(t *testing.T) {
				if n := len(rec.failures()); n != 0 {
					t.Fatalf("the harness failed a conforming keyring:\n%s", rec.summary())
				}
			})
		})
	})
}
