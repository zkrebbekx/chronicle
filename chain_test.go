package chronicle

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// tamperQuery wraps MemStore and edits records on the way out of Query, which
// is what a retrospective edit to the underlying storage looks like to
// Verify. Everything else — Get, Apply, Delete, Tombstones — is the real
// store, promoted.
type tamperQuery struct {
	*MemStore
	fn func(*Record)
}

func (s *tamperQuery) Query(ctx context.Context, q Query) ([]Record, Cursor, error) {
	recs, c, err := s.MemStore.Query(ctx, q)
	for i := range recs {
		s.fn(&recs[i])
	}
	return recs, c, err
}

// hideTombstones wraps MemStore and denies its tombstones, which is what a
// store that deleted chained records without retaining their hashes looks
// like.
type hideTombstones struct{ *MemStore }

func (s *hideTombstones) Tombstones(context.Context, string, string) ([]Tombstone, error) {
	return nil, nil
}

// failTombstones cannot read its tombstones at all.
type failTombstones struct{ *MemStore }

func (s *failTombstones) Tombstones(context.Context, string, string) ([]Tombstone, error) {
	return nil, errors.New("tombstones unreadable")
}

// newChainedLog is newTestLog with chaining on.
func newChainedLog(t *testing.T) (*Log, *MemStore, *FixedClock) {
	t.Helper()
	return newTestLog(t, WithChaining())
}

func putAt(t *testing.T, l *Log, clock *FixedClock, id, data string, from, to time.Time) Result {
	t.Helper()
	clock.Advance(time.Second)
	return mustPut(t, l, id, data, from, to)
}

func verify(t *testing.T, s Store, entityID string) VerifyReport {
	t.Helper()
	rep, err := NewLog(s).Verify(context.Background(), employee, entityID)
	if err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	return rep
}

func TestChainVerify(t *testing.T) {
	ctx := context.Background()

	t.Run("given an entity written under chaining", func(t *testing.T) {
		l, store, clock := newChainedLog(t)
		putAt(t, l, clock, "e1", `{"v":1}`, t1, t2)
		putAt(t, l, clock, "e1", `{"v":2}`, t2, t3)
		putAt(t, l, clock, "e1", `{"v":3}`, t1, t3)

		t.Run("when it is verified", func(t *testing.T) {
			rep := verify(t, store, "e1")
			t.Run("then the chain is intact and covers every record", func(t *testing.T) {
				if !rep.Intact() {
					t.Fatalf("divergence = %+v; want an intact chain", rep.Divergence)
				}
				recs, _ := l.History(ctx, employee, "e1")
				if rep.ChainedRecords != len(recs) || rep.UnchainedPrefix != 0 {
					t.Fatalf("report = %+v; want all %d records chained", rep, len(recs))
				}
			})
			t.Run("then every stored record carries a chain value", func(t *testing.T) {
				recs, _ := l.History(ctx, employee, "e1")
				for _, r := range recs {
					if _, ok := r.Meta[MetaChain]; !ok {
						t.Fatalf("record %s has no %s metadata", r.ID, MetaChain)
					}
				}
			})
			t.Run("then the head matches ChainHead", func(t *testing.T) {
				head, err := l.ChainHead(ctx, employee, "e1")
				if err != nil {
					t.Fatalf("ChainHead failed: %v", err)
				}
				if string(head) != string(rep.Head) {
					t.Fatal("ChainHead disagrees with the verified head")
				}
			})
		})

		t.Run("when a second chained log continues the entity", func(t *testing.T) {
			clock.Advance(time.Second)
			l2 := NewLog(store, WithClock(clock), WithChaining())
			if _, err := l2.Put(ctx, employee, "e1", []byte(`{"v":4}`), t3, t4, bob); err != nil {
				t.Fatalf("Put failed: %v", err)
			}
			t.Run("then the chain is continuous across writers", func(t *testing.T) {
				if rep := verify(t, store, "e1"); !rep.Intact() {
					t.Fatalf("divergence = %+v; a new chained writer must extend the chain, not fork it", rep.Divergence)
				}
			})
		})
	})

	t.Run("given a split that produced remainders", func(t *testing.T) {
		l, store, clock := newChainedLog(t)
		putAt(t, l, clock, "e1", `{"v":1}`, t1, t5)
		res := putAt(t, l, clock, "e1", `{"v":2}`, t2, t3)

		t.Run("when the write is examined", func(t *testing.T) {
			t.Run("then the remainders are chained too, with fresh links", func(t *testing.T) {
				if len(res.Written) != 3 {
					t.Fatalf("written = %d records; want the record and two remainders", len(res.Written))
				}
				seen := map[string]bool{}
				for _, r := range res.Written {
					token := r.Meta[MetaChain]
					if token == "" {
						t.Fatalf("record %s (%s) is unchained", r.ID, r.Intent)
					}
					if seen[token] {
						t.Fatalf("two records share the chain value %s — a remainder inherited "+
							"its predecessor's link instead of getting its own", token)
					}
					seen[token] = true
				}
			})
			t.Run("then the chain verifies through the split", func(t *testing.T) {
				if rep := verify(t, store, "e1"); !rep.Intact() {
					t.Fatalf("divergence = %+v", rep.Divergence)
				}
			})
		})
	})

	t.Run("given a frozen clock forcing sub-microsecond transaction instants", func(t *testing.T) {
		l, store, _ := newChainedLog(t)
		// No Advance between writes: the ratchet spaces these by a nanosecond,
		// below the canonical form's microsecond resolution.
		mustPut(t, l, "e1", `{"v":1}`, t1, t2)
		mustPut(t, l, "e1", `{"v":2}`, t1, t2)
		mustPut(t, l, "e1", `{"v":3}`, t1, t2)

		t.Run("when it is verified", func(t *testing.T) {
			t.Run("then the chain still adds up", func(t *testing.T) {
				if rep := verify(t, store, "e1"); !rep.Intact() {
					t.Fatalf("divergence = %+v; canonicalization must survive instants that "+
						"collide at microsecond resolution", rep.Divergence)
				}
			})
		})
	})

	t.Run("given an entity with no chain", func(t *testing.T) {
		l, store, clock := newTestLog(t)
		putAt(t, l, clock, "e1", `{"v":1}`, t1, t2)

		t.Run("when it is verified", func(t *testing.T) {
			_, err := NewLog(store).Verify(ctx, employee, "e1")
			t.Run("then the answer is ErrNoChain, not a pass", func(t *testing.T) {
				if !errors.Is(err, ErrNoChain) {
					t.Fatalf("Verify = %v; want ErrNoChain — nothing verified is not the same "+
						"as verified", err)
				}
			})
		})
		t.Run("when its head is asked for", func(t *testing.T) {
			t.Run("then that is ErrNoChain too", func(t *testing.T) {
				if _, err := NewLog(store).ChainHead(ctx, employee, "e1"); !errors.Is(err, ErrNoChain) {
					t.Fatalf("ChainHead = %v; want ErrNoChain", err)
				}
			})
		})
	})

	t.Run("given history that predates the chain", func(t *testing.T) {
		clock := NewFixedClock(t0)
		store := NewMemStore()
		plain := NewLog(store, WithClock(clock))
		clock.Advance(time.Second)
		if _, err := plain.Put(ctx, employee, "e1", []byte(`{"v":1}`), t1, t2, alice); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
		chained := NewLog(store, WithClock(clock), WithChaining())
		clock.Advance(time.Second)
		if _, err := chained.Put(ctx, employee, "e1", []byte(`{"v":2}`), t2, t3, alice); err != nil {
			t.Fatalf("Put failed: %v", err)
		}

		t.Run("when it is verified", func(t *testing.T) {
			rep := verify(t, store, "e1")
			t.Run("then the chain is intact and the prefix is counted, not hidden", func(t *testing.T) {
				if !rep.Intact() {
					t.Fatalf("divergence = %+v", rep.Divergence)
				}
				if rep.UnchainedPrefix != 1 || rep.ChainedRecords != 1 {
					t.Fatalf("report = %+v; want one unchained prefix record and one chained", rep)
				}
			})
		})

		t.Run("when an unchained write lands after the chain began", func(t *testing.T) {
			clock.Advance(time.Second)
			if _, err := plain.Put(ctx, employee, "e1", []byte(`{"v":3}`), t3, t4, bob); err != nil {
				t.Fatalf("Put failed: %v", err)
			}
			rep := verify(t, store, "e1")
			t.Run("then it reads as an insertion and fails verification", func(t *testing.T) {
				if rep.Intact() {
					t.Fatal("an out-of-band write after the chain began must not verify")
				}
				if !strings.Contains(rep.Divergence.Reason, "not covered by the chain") {
					t.Fatalf("reason = %q; want the uncovered-record reason", rep.Divergence.Reason)
				}
			})
		})
	})

	t.Run("given a store whose tombstones are unreadable", func(t *testing.T) {
		l, store, clock := newChainedLog(t)
		putAt(t, l, clock, "e1", `{"v":1}`, t1, t2)
		t.Run("when it is verified", func(t *testing.T) {
			t.Run("then the failure surfaces rather than verifying a partial chain", func(t *testing.T) {
				if _, err := NewLog(&failTombstones{store}).Verify(ctx, employee, "e1"); err == nil {
					t.Fatal("Verify succeeded without being able to read tombstones")
				}
			})
		})
	})

	t.Run("given a log restricted to known kinds", func(t *testing.T) {
		l := NewLog(NewMemStore(), WithKinds(employee), WithChaining())
		t.Run("when verification names an unknown kind or no entity", func(t *testing.T) {
			t.Run("then the checks match the read paths'", func(t *testing.T) {
				if _, err := l.Verify(ctx, "ghost", "e1"); !errors.Is(err, ErrUnknownKind) {
					t.Fatalf("Verify = %v; want ErrUnknownKind", err)
				}
				if _, err := l.Verify(ctx, employee, ""); !errors.Is(err, ErrMissingEntityID) {
					t.Fatalf("Verify = %v; want ErrMissingEntityID", err)
				}
				if _, err := l.ChainHead(ctx, "ghost", "e1"); !errors.Is(err, ErrUnknownKind) {
					t.Fatalf("ChainHead = %v; want ErrUnknownKind", err)
				}
			})
		})
	})
}

func TestChainTamperDetection(t *testing.T) {
	// One chained fixture per case: three writes, the middle one split.
	seed := func(t *testing.T) (*MemStore, []Record) {
		t.Helper()
		l, store, clock := newChainedLog(t)
		putAt(t, l, clock, "e1", `{"v":1}`, t1, t4)
		putAt(t, l, clock, "e1", `{"v":2}`, t2, t3)
		putAt(t, l, clock, "e1", `{"v":3}`, t3, t5)
		recs, err := l.History(context.Background(), employee, "e1")
		if err != nil {
			t.Fatalf("History failed: %v", err)
		}
		return store, recs
	}

	cases := []struct {
		name   string
		tamper func(*Record)
		reason string
	}{
		{
			name:   "the data is rewritten",
			tamper: func(r *Record) { r.Data = []byte(`{"v":99}`) },
			reason: "stored chain value does not match",
		},
		{
			name:   "the valid interval is shifted",
			tamper: func(r *Record) { r.ValidFrom = r.ValidFrom.Add(time.Hour) },
			reason: "stored chain value does not match",
		},
		{
			name:   "the write is re-attributed",
			tamper: func(r *Record) { r.Actor = Actor{ID: "u-mallory"} },
			reason: "stored chain value does not match",
		},
		{
			name:   "the reason is edited",
			tamper: func(r *Record) { r.Reason = "routine" },
			reason: "stored chain value does not match",
		},
		{
			name:   "the intent is repainted",
			tamper: func(r *Record) { r.Intent = IntentCorrection },
			reason: "stored chain value does not match",
		},
		{
			name:   "caller metadata is edited",
			tamper: func(r *Record) { r.Meta["injected"] = "yes" },
			reason: "stored chain value does not match",
		},
		{
			name:   "the record is moved on the transaction axis",
			tamper: func(r *Record) { r.TxFrom = r.TxFrom.Add(time.Hour) },
			reason: "", // any divergence: reordering breaks the hash of whichever record now comes first
		},
		{
			name:   "the chain value itself is replaced with an unknown version",
			tamper: func(r *Record) { r.Meta[MetaChain] = "v9:deadbeef" },
			reason: "malformed or of an unrecognised format version",
		},
		{
			name:   "the chain value is stripped",
			tamper: func(r *Record) { delete(r.Meta, MetaChain) },
			reason: "not covered by the chain",
		},
	}

	t.Run("given a survivor tampered with in storage", func(t *testing.T) {
		for _, tc := range cases {
			t.Run("when "+tc.name, func(t *testing.T) {
				store, recs := seed(t)
				// Tamper with the second write's surviving record: mid-chain,
				// superseded, with chained records after it.
				target := recs[2].ID
				tampered := &tamperQuery{MemStore: store, fn: func(r *Record) {
					if r.ID == target {
						tc.tamper(r)
					}
				}}
				rep := verify(t, tampered, "e1")
				t.Run("then verification fails", func(t *testing.T) {
					if rep.Intact() {
						t.Fatal("the tampered chain verified")
					}
					if rep.Head != nil {
						t.Fatal("a diverged report must not offer a head to anchor")
					}
				})
				if tc.reason != "" {
					t.Run("then the divergence names the record and the failure", func(t *testing.T) {
						if rep.Divergence.RecordID != target {
							t.Fatalf("divergence at %s; want %s", rep.Divergence.RecordID, target)
						}
						if !strings.Contains(rep.Divergence.Reason, tc.reason) {
							t.Fatalf("reason = %q; want it to mention %q", rep.Divergence.Reason, tc.reason)
						}
					})
				}
			})
		}
	})

	t.Run("given a superseded record whose TxTo is quietly shifted", func(t *testing.T) {
		store, recs := seed(t)
		target := recs[0].ID
		tampered := &tamperQuery{MemStore: store, fn: func(r *Record) {
			if r.ID == target && !r.TxTo.IsZero() {
				r.TxTo = r.TxTo.Add(time.Hour)
			}
		}}
		t.Run("when it is verified", func(t *testing.T) {
			rep := verify(t, tampered, "e1")
			t.Run("then the supersession instant is caught against later writes", func(t *testing.T) {
				if rep.Intact() {
					t.Fatal("a shifted TxTo verified; the hash cannot cover TxTo, so the " +
						"cross-check against later transaction starts must catch it")
				}
				if rep.Divergence.RecordID != target ||
					!strings.Contains(rep.Divergence.Reason, "matching no later chained write") {
					t.Fatalf("divergence = %+v; want the TxTo cross-check naming %s", rep.Divergence, target)
				}
			})
		})
	})

	t.Run("given a chained record deleted without a tombstone", func(t *testing.T) {
		store, recs := seed(t)
		// recs[0] is superseded; delete it for real, then hide the tombstone.
		if _, err := store.Delete(context.Background(), []RecordID{recs[0].ID}); err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
		t.Run("when it is verified through a store that kept no tombstone", func(t *testing.T) {
			rep := verify(t, &hideTombstones{store}, "e1")
			t.Run("then the gap is a divergence, not a silent skip", func(t *testing.T) {
				if rep.Intact() {
					t.Fatal("a tombstone-free gap verified; deletion without a retained " +
						"hash must break the chain")
				}
			})
		})
		t.Run("when it is verified with the tombstone in place", func(t *testing.T) {
			rep := verify(t, store, "e1")
			t.Run("then the chain passes over the gap and reports it", func(t *testing.T) {
				if !rep.Intact() {
					t.Fatalf("divergence = %+v", rep.Divergence)
				}
				if rep.Tombstones != 1 {
					t.Fatalf("Tombstones = %d; want 1", rep.Tombstones)
				}
			})
		})
	})

	t.Run("given a tombstone whose hash was lost", func(t *testing.T) {
		store, recs := seed(t)
		if _, err := store.Delete(context.Background(), []RecordID{recs[0].ID}); err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
		// Corrupt the tombstone in place.
		store.mu.Lock()
		for key, ts := range store.tombs {
			for i := range ts {
				ts[i].ChainHash = ""
			}
			store.tombs[key] = ts
		}
		store.mu.Unlock()
		t.Run("when it is verified", func(t *testing.T) {
			rep := verify(t, store, "e1")
			t.Run("then the unusable tombstone is a divergence", func(t *testing.T) {
				if rep.Intact() || !strings.Contains(rep.Divergence.Reason, "no usable chain hash") {
					t.Fatalf("report = %+v; want the tombstone divergence", rep.Divergence)
				}
			})
		})
	})
}

// TestChainSurvivesRetention is the full sequence the design demands: write a
// chain, destroy its middle on schedule, verify across the gap, then tamper
// with a survivor and watch verification name it.
func TestChainSurvivesRetention(t *testing.T) {
	ctx := context.Background()

	t.Run("given a chained entity whose middle history was retention-deleted", func(t *testing.T) {
		l, store, clock := newChainedLog(t)
		putAt(t, l, clock, "e1", `{"v":1}`, t1, t2)
		putAt(t, l, clock, "e1", `{"v":2}`, t2, t3)
		putAt(t, l, clock, "e1", `{"v":3}`, t1, t3) // supersedes both
		putAt(t, l, clock, "e1", `{"v":4}`, t2, t4) // splits v3

		// Destroy every superseded record, as a retention sweep would.
		recs, err := l.History(ctx, employee, "e1")
		if err != nil {
			t.Fatalf("History failed: %v", err)
		}
		var doomed []RecordID
		for _, r := range recs {
			if !r.IsCurrent() {
				doomed = append(doomed, r.ID)
			}
		}
		if len(doomed) == 0 {
			t.Fatal("fixture produced nothing to delete")
		}
		if _, err := store.Delete(ctx, doomed); err != nil {
			t.Fatalf("Delete failed: %v", err)
		}

		t.Run("when the chain is verified across the gap", func(t *testing.T) {
			rep := verify(t, store, "e1")
			t.Run("then it passes, and the report names the gap's size", func(t *testing.T) {
				if !rep.Intact() {
					t.Fatalf("divergence = %+v; tombstones must carry the chain across "+
						"retention", rep.Divergence)
				}
				if rep.Tombstones != len(doomed) {
					t.Fatalf("Tombstones = %d; want %d", rep.Tombstones, len(doomed))
				}
				if rep.ChainedRecords == 0 {
					t.Fatal("no surviving records verified")
				}
			})
		})

		t.Run("when a survivor is then tampered with", func(t *testing.T) {
			survivors, err := l.History(ctx, employee, "e1")
			if err != nil {
				t.Fatalf("History failed: %v", err)
			}
			target := survivors[len(survivors)-1].ID
			tampered := &tamperQuery{MemStore: store, fn: func(r *Record) {
				if r.ID == target {
					r.Data = []byte(`{"v":"forged"}`)
				}
			}}
			rep := verify(t, tampered, "e1")
			t.Run("then verification fails naming exactly that record", func(t *testing.T) {
				if rep.Intact() {
					t.Fatal("the tampered survivor verified")
				}
				if rep.Divergence.RecordID != target {
					t.Fatalf("divergence at %s; want %s", rep.Divergence.RecordID, target)
				}
			})
		})
	})
}
